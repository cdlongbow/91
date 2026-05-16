package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/video-site/backend/internal/api"
	"github.com/video-site/backend/internal/auth"
	"github.com/video-site/backend/internal/catalog"
	"github.com/video-site/backend/internal/config"
	"github.com/video-site/backend/internal/drives"
	"github.com/video-site/backend/internal/drives/localupload"
	"github.com/video-site/backend/internal/drives/onedrive"
	"github.com/video-site/backend/internal/drives/p115"
	"github.com/video-site/backend/internal/drives/pikpak"
	"github.com/video-site/backend/internal/drives/quark"
	"github.com/video-site/backend/internal/drives/wopan"
	"github.com/video-site/backend/internal/preview"
	"github.com/video-site/backend/internal/proxy"
	"github.com/video-site/backend/internal/scanner"
)

func main() {
	cfgPath := "./config.yaml"
	if v := os.Getenv("VIDEO_CONFIG"); v != "" {
		cfgPath = v
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	if err := os.MkdirAll(filepath.Dir(cfg.Storage.DBPath), 0o755); err != nil {
		log.Fatalf("mkdir db dir: %v", err)
	}
	if err := os.MkdirAll(cfg.Storage.LocalPreviewDir, 0o755); err != nil {
		log.Fatalf("mkdir preview dir: %v", err)
	}

	cat, err := catalog.Open(cfg.Storage.DBPath)
	if err != nil {
		log.Fatalf("open catalog: %v", err)
	}
	defer cat.Close()

	app := &App{
		cfg:          cfg,
		cat:          cat,
		registry:     proxy.NewRegistry(),
		workers:      make(map[string]*preview.Worker),
		thumbWorkers: make(map[string]*preview.ThumbWorker),
	}
	app.proxy = proxy.New(app.registry)

	// 初始化现有 drives
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	app.loadPreviewEnabled(ctx)
	if err := app.attachLocalUpload(ctx); err != nil {
		log.Printf("[local-upload] attach failed: %v", err)
	}

	existing, err := cat.ListDrives(ctx)
	if err != nil {
		log.Fatalf("list drives: %v", err)
	}
	for _, d := range existing {
		if err := app.attachDrive(ctx, d); err != nil {
			log.Printf("[drive %s] attach failed: %v", d.ID, err)
		}
	}

	authr := &auth.Authenticator{
		Username: cfg.Server.Admin.Username,
		Password: cfg.Server.Admin.Password,
		Catalog:  cat,
	}

	apiServer := &api.Server{
		Catalog:    cat,
		Proxy:      app.proxy,
		LocalDir:   cfg.Storage.LocalPreviewDir,
		UploadDir:  app.localUploadDir(),
		FFmpegPath: cfg.Preview.FFmpegPath,
		OnVideoUploaded: func(v *catalog.Video) {
			app.enqueueUploadedVideo(ctx, v)
		},
	}

	adminServer := &api.AdminServer{
		Catalog:         cat,
		Auth:            authr,
		LocalPreviewDir: cfg.Storage.LocalPreviewDir,
		OnDriveSaved: func(driveID string) error {
			d, err := cat.GetDrive(ctx, driveID)
			if err != nil {
				return err
			}
			return app.attachDrive(ctx, d)
		},
		OnDriveRemoved: func(driveID string) {
			app.detachDrive(driveID)
		},
		OnScanRequested: func(driveID string) {
			go app.runScan(ctx, driveID)
		},
		OnRegenPreview: func(videoID string) {
			go app.regenPreview(ctx, videoID)
		},
		OnRegenAllPreviews: func() {
			go app.regenAllPreviews(ctx)
		},
		OnRegenFailedPreviews: func(driveID string) {
			go app.regenFailedPreviews(ctx, driveID)
		},
		GetDriveGenerationStatuses: func() map[string]api.DriveGenerationStatuses {
			return app.driveGenerationStatuses()
		},
		GetPreviewEnabled: func() bool { return app.PreviewEnabled() },
		SetPreviewEnabled: func(enabled bool) error {
			return app.SetPreviewEnabled(ctx, enabled)
		},
	}

	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(corsMiddleware)

	apiServer.RegisterRoutes(r, authr)
	adminServer.Register(r)

	// 启动定时扫描
	go app.scanLoop(ctx)

	srv := &http.Server{
		Addr:    cfg.Server.Listen,
		Handler: r,
	}
	go func() {
		log.Printf("video-site backend listening on %s", cfg.Server.Listen)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	// 等待退出信号
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	<-sigs
	log.Println("shutting down...")
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutCancel()
	_ = srv.Shutdown(shutCtx)
}

// ---------- App ----------

type App struct {
	cfg      *config.Config
	cat      *catalog.Catalog
	registry *proxy.Registry
	proxy    *proxy.Proxy

	mu           sync.Mutex
	workers      map[string]*preview.Worker
	thumbWorkers map[string]*preview.ThumbWorker
	cancels      map[string]context.CancelFunc

	// 运行时 preview 开关（从 DB 读）
	previewEnabled bool
}

// PreviewEnabled 线程安全读
func (a *App) PreviewEnabled() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.previewEnabled
}

// SetPreviewEnabled 切换开关，写库 + 若开启则立刻补扫 pending
func (a *App) SetPreviewEnabled(ctx context.Context, enabled bool) error {
	a.mu.Lock()
	a.previewEnabled = enabled
	a.mu.Unlock()

	val := "0"
	if enabled {
		val = "1"
	}
	if err := a.cat.SetSetting(ctx, "preview.enabled", val); err != nil {
		return err
	}

	if enabled {
		// 异步补扫所有盘
		go func() {
			for _, d := range a.registry.All() {
				a.mu.Lock()
				w := a.workers[d.ID()]
				tw := a.thumbWorkers[d.ID()]
				a.mu.Unlock()
				a.enqueueDriveGeneration(ctx, d.ID(), w, tw)
			}
		}()
	}
	return nil
}

// loadPreviewEnabled 从 DB 读运行时开关，首次启动取 config 默认值
func (a *App) loadPreviewEnabled(ctx context.Context) {
	def := "0"
	if a.cfg.Preview.Enabled {
		def = "1"
	}
	v, err := a.cat.GetSetting(ctx, "preview.enabled", def)
	if err != nil {
		log.Printf("[preview] load setting: %v (fallback to config)", err)
		a.mu.Lock()
		a.previewEnabled = a.cfg.Preview.Enabled
		a.mu.Unlock()
		return
	}
	a.mu.Lock()
	a.previewEnabled = v == "1"
	a.mu.Unlock()
}

func (a *App) driveGenerationStatuses() map[string]api.DriveGenerationStatuses {
	a.mu.Lock()
	previewWorkers := make(map[string]*preview.Worker, len(a.workers))
	for id, worker := range a.workers {
		previewWorkers[id] = worker
	}
	thumbWorkers := make(map[string]*preview.ThumbWorker, len(a.thumbWorkers))
	for id, worker := range a.thumbWorkers {
		thumbWorkers[id] = worker
	}
	a.mu.Unlock()

	out := make(map[string]api.DriveGenerationStatuses, len(previewWorkers)+len(thumbWorkers))
	for id, worker := range previewWorkers {
		status := out[id]
		status.Preview = generationStatusFromPreview(worker.Status())
		out[id] = status
	}
	for id, worker := range thumbWorkers {
		status := out[id]
		status.Thumbnail = generationStatusFromPreview(worker.Status())
		missing, err := a.cat.CountVideosNeedingThumbnail(context.Background(), id)
		if err != nil {
			log.Printf("[thumb] count missing thumbnails %s: %v", id, err)
		} else {
			status.Thumbnail.QueueLength = missing
			if missing > 0 && status.Thumbnail.State == "idle" {
				status.Thumbnail.State = "queued"
			}
		}
		out[id] = status
	}
	return out
}

func generationStatusFromPreview(status preview.TaskStatus) api.GenerationStatus {
	state := status.State
	if state == "" {
		state = "idle"
	}
	out := api.GenerationStatus{
		State:        state,
		CurrentTitle: status.CurrentTitle,
		QueueLength:  status.QueueLength,
	}
	if !status.CooldownUntil.IsZero() {
		out.CooldownUntil = status.CooldownUntil.Format(time.RFC3339)
	}
	return out
}

func (a *App) attachDrive(ctx context.Context, d *catalog.Drive) error {
	var drv drives.Drive
	switch d.Kind {
	case "quark":
		drv = quark.New(quark.Config{
			ID:     d.ID,
			Cookie: d.Credentials["cookie"],
			RootID: d.RootID,
			OnCookieUpdate: func(cookie string) {
				d.Credentials["cookie"] = cookie
				_ = a.cat.UpsertDrive(ctx, d)
			},
		})
	case "p115":
		drv = p115.New(p115.Config{
			ID:     d.ID,
			Cookie: d.Credentials["cookie"],
			RootID: d.RootID,
		})
	case "pikpak":
		drv = pikpak.New(pikpak.Config{
			ID:               d.ID,
			Username:         d.Credentials["username"],
			Password:         d.Credentials["password"],
			Platform:         d.Credentials["platform"],
			RefreshToken:     d.Credentials["refresh_token"],
			AccessToken:      d.Credentials["access_token"],
			CaptchaToken:     d.Credentials["captcha_token"],
			DeviceID:         d.Credentials["device_id"],
			RootID:           d.RootID,
			DisableMediaLink: pikpak.ParseBoolDefault(d.Credentials["disable_media_link"], true),
			OnTokenUpdate: func(access, refresh, captcha, deviceID string) {
				d.Credentials["access_token"] = access
				d.Credentials["refresh_token"] = refresh
				d.Credentials["captcha_token"] = captcha
				d.Credentials["device_id"] = deviceID
				_ = a.cat.UpsertDrive(ctx, d)
			},
		})
	case "wopan":
		drv = wopan.New(wopan.Config{
			ID:           d.ID,
			AccessToken:  d.Credentials["access_token"],
			RefreshToken: d.Credentials["refresh_token"],
			FamilyID:     d.Credentials["family_id"],
			RootID:       d.RootID,
			OnTokenUpdate: func(access, refresh string) {
				d.Credentials["access_token"] = access
				d.Credentials["refresh_token"] = refresh
				_ = a.cat.UpsertDrive(ctx, d)
			},
		})
	case "onedrive":
		drv = onedrive.New(onedrive.Config{
			ID:           d.ID,
			RootID:       d.RootID,
			Region:       d.Credentials["region"],
			AccessToken:  d.Credentials["access_token"],
			RefreshToken: d.Credentials["refresh_token"],
			IsSharePoint: parseBoolDefault(d.Credentials["is_sharepoint"], false),
			SiteID:       d.Credentials["site_id"],
			RenewAPIURL:  d.Credentials["api_url_address"],
			OnTokenUpdate: func(access, refresh string) {
				if d.Credentials == nil {
					d.Credentials = make(map[string]string)
				}
				d.Credentials["access_token"] = access
				d.Credentials["refresh_token"] = refresh
				_ = a.cat.UpsertDrive(ctx, d)
			},
		})
	default:
		return fmt.Errorf("unknown drive kind: %s", d.Kind)
	}

	if err := drv.Init(ctx); err != nil {
		d.Status = "error"
		d.LastError = err.Error()
		_ = a.cat.UpsertDrive(ctx, d)
		return err
	}

	d.Status = "ok"
	d.LastError = ""
	_ = a.cat.UpsertDrive(ctx, d)

	a.registry.Set(d.ID, drv)

	// preview worker
	gen := preview.New(preview.Config{
		FFmpegPath:      a.cfg.Preview.FFmpegPath,
		FFprobePath:     a.cfg.Preview.FFprobePath,
		DurationSeconds: a.cfg.Preview.DurationSeconds,
		Width:           a.cfg.Preview.Width,
		Segments:        a.cfg.Preview.Segments,
		LocalDir:        a.cfg.Storage.LocalPreviewDir,
		RemoteDir:       "",
	})
	worker := preview.NewWorker(gen, a.cat, drv, "")
	thumbWorker := preview.NewThumbWorker(gen, a.cat, drv)

	workerCtx, cancel := context.WithCancel(ctx)
	go worker.Run(workerCtx)
	go thumbWorker.Run(workerCtx)

	a.registerPreviewWorkers(ctx, d.ID, worker, thumbWorker, cancel)

	return nil
}

func (a *App) attachLocalUpload(ctx context.Context) error {
	drv := localupload.New(a.localUploadDir())
	if err := drv.Init(ctx); err != nil {
		return err
	}
	a.registry.Set(drv.ID(), drv)

	gen := preview.New(preview.Config{
		FFmpegPath:      a.cfg.Preview.FFmpegPath,
		FFprobePath:     a.cfg.Preview.FFprobePath,
		DurationSeconds: a.cfg.Preview.DurationSeconds,
		Width:           a.cfg.Preview.Width,
		Segments:        a.cfg.Preview.Segments,
		LocalDir:        a.cfg.Storage.LocalPreviewDir,
		RemoteDir:       "",
	})
	worker := preview.NewWorker(gen, a.cat, drv, "")
	thumbWorker := preview.NewThumbWorker(gen, a.cat, drv)

	workerCtx, cancel := context.WithCancel(ctx)
	go worker.Run(workerCtx)
	go thumbWorker.Run(workerCtx)

	a.registerPreviewWorkers(ctx, drv.ID(), worker, thumbWorker, cancel)
	return nil
}

func (a *App) localUploadDir() string {
	return filepath.Join(filepath.Dir(a.cfg.Storage.LocalPreviewDir), "uploads")
}

func (a *App) registerPreviewWorkers(ctx context.Context, driveID string, worker *preview.Worker, thumbWorker *preview.ThumbWorker, cancel context.CancelFunc) {
	a.mu.Lock()
	if a.cancels == nil {
		a.cancels = make(map[string]context.CancelFunc)
	}
	if a.workers == nil {
		a.workers = make(map[string]*preview.Worker)
	}
	if a.thumbWorkers == nil {
		a.thumbWorkers = make(map[string]*preview.ThumbWorker)
	}
	if old, ok := a.cancels[driveID]; ok && old != nil {
		old()
	}
	if worker != nil {
		a.workers[driveID] = worker
	} else {
		delete(a.workers, driveID)
	}
	if thumbWorker != nil {
		a.thumbWorkers[driveID] = thumbWorker
	} else {
		delete(a.thumbWorkers, driveID)
	}
	if cancel != nil {
		a.cancels[driveID] = cancel
	} else {
		delete(a.cancels, driveID)
	}
	a.mu.Unlock()

	if worker != nil {
		if thumbWorker != nil {
			worker.BeforeTask = func(taskCtx context.Context) bool {
				return a.waitForThumbnailsBeforePreview(taskCtx, driveID)
			}
		} else {
			worker.BeforeTask = nil
		}
	}

	go a.enqueueDriveGeneration(ctx, driveID, worker, thumbWorker)
}

func (a *App) enqueuePending(ctx context.Context, driveID string, w *preview.Worker) {
	pending, err := a.cat.ListVideosByPreviewStatus(ctx, driveID, "pending", 0)
	if err != nil {
		log.Printf("[preview] list pending %s: %v", driveID, err)
		return
	}
	if len(pending) == 0 {
		return
	}
	log.Printf("[preview] enqueue %d pending videos for drive=%s", len(pending), driveID)
	for _, v := range pending {
		if !w.EnqueueBlocking(ctx, v) {
			log.Printf("[preview] enqueue pending canceled for drive=%s", driveID)
			return
		}
	}
}

func (a *App) enqueueDriveGeneration(ctx context.Context, driveID string, worker *preview.Worker, thumbWorker *preview.ThumbWorker) {
	if thumbWorker != nil {
		a.enqueueThumbnails(ctx, driveID, thumbWorker)
	}
	if !a.PreviewEnabled() || worker == nil {
		return
	}
	if thumbWorker != nil && !a.waitForThumbnailsBeforePreview(ctx, driveID) {
		return
	}
	a.enqueuePending(ctx, driveID, worker)
}

func (a *App) waitForThumbnailsBeforePreview(ctx context.Context, driveID string) bool {
	const pollInterval = time.Second
	var lastLog time.Time
	for {
		missing, err := a.cat.CountVideosNeedingThumbnail(ctx, driveID)
		if err != nil {
			log.Printf("[preview] count missing thumbnails drive=%s: %v", driveID, err)
			return false
		}
		if missing == 0 {
			return true
		}
		now := time.Now()
		if lastLog.IsZero() || now.Sub(lastLog) >= time.Minute {
			log.Printf("[preview] drive=%s waiting for %d thumbnails before teaser generation", driveID, missing)
			lastLog = now
		}
		timer := time.NewTimer(pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
	}
}

func (a *App) enqueueThumbnails(ctx context.Context, driveID string, w *preview.ThumbWorker) {
	pending, err := a.cat.ListVideosNeedingThumbnail(ctx, driveID, 0)
	if err != nil {
		log.Printf("[thumb] list pending %s: %v", driveID, err)
		return
	}
	if len(pending) == 0 {
		return
	}
	log.Printf("[thumb] enqueue %d missing thumbnails for drive=%s", len(pending), driveID)
	for _, v := range pending {
		if !w.EnqueueBlocking(ctx, v) {
			log.Printf("[thumb] enqueue missing thumbnails canceled for drive=%s", driveID)
			return
		}
	}
}

func (a *App) detachDrive(id string) {
	a.registry.Remove(id)
	a.mu.Lock()
	if cancel, ok := a.cancels[id]; ok {
		cancel()
		delete(a.cancels, id)
	}
	delete(a.workers, id)
	delete(a.thumbWorkers, id)
	a.mu.Unlock()
}

func (a *App) runScan(ctx context.Context, driveID string) {
	drv, ok := a.registry.Get(driveID)
	if !ok {
		log.Printf("[scan] drive %s not attached", driveID)
		return
	}

	a.mu.Lock()
	worker := a.workers[driveID]
	thumbWorker := a.thumbWorkers[driveID]
	a.mu.Unlock()

	var onNew func(v *catalog.Video)
	if thumbWorker != nil {
		onNew = func(v *catalog.Video) {
			if thumbWorker != nil && v.ThumbnailURL == "" {
				thumbWorker.Enqueue(v)
			}
		}
	}

	sc := scanner.New(a.cat, drv, a.cfg.Scanner.VideoExtensions, a.cfg.Scanner.MaxDepth, onNew)

	// 使用 drive 的 scan_root_id，否则 root_id
	d, err := a.cat.GetDrive(ctx, driveID)
	if err != nil {
		log.Printf("[scan] get drive %s: %v", driveID, err)
		return
	}
	startID := d.ScanRootID
	if startID == "" {
		startID = d.RootID
	}

	log.Printf("[scan] drive=%s start=%s", driveID, startID)
	stats, err := sc.Run(ctx, startID)
	if err != nil {
		log.Printf("[scan] drive=%s error: %v", driveID, err)
		return
	}
	log.Printf("[scan] drive=%s done scanned=%d added=%d errors=%d", driveID, stats.Scanned, stats.Added, stats.Errors)
	if drv.Kind() == "pikpak" {
		if stats.Errors > 0 {
			log.Printf("[cleanup] skip stale PikPak cleanup for drive=%s: scan had %d directory errors", driveID, stats.Errors)
		} else {
			removed, err := a.cleanupMissingDriveVideos(ctx, driveID, stats.SeenFileIDs, stats.VisitedDirIDs, startID == drv.RootID())
			if err != nil {
				log.Printf("[cleanup] stale PikPak cleanup drive=%s error: %v", driveID, err)
			} else if removed > 0 {
				log.Printf("[cleanup] removed %d stale PikPak videos for drive=%s", removed, driveID)
			}
		}
	}
	a.enqueueDriveGeneration(ctx, driveID, worker, thumbWorker)
}

func (a *App) cleanupMissingDriveVideos(ctx context.Context, driveID string, liveFileIDs map[string]struct{}, visitedDirIDs map[string]struct{}, fullDriveScan bool) (int, error) {
	items, err := a.cat.ListVideosByDrive(ctx, driveID)
	if err != nil {
		return 0, err
	}

	localDir := ""
	if a.cfg != nil {
		localDir = a.cfg.Storage.LocalPreviewDir
	}
	removed := 0
	for _, v := range items {
		if _, ok := liveFileIDs[v.FileID]; ok {
			continue
		}
		if !fullDriveScan {
			if _, ok := visitedDirIDs[v.ParentID]; !ok {
				continue
			}
		}
		if err := removeLocalVideoAssets(localDir, v); err != nil {
			return removed, fmt.Errorf("remove local assets for %s: %w", v.ID, err)
		}
		if err := a.cat.DeleteVideo(ctx, v.ID); err != nil {
			return removed, fmt.Errorf("delete catalog video %s: %w", v.ID, err)
		}
		removed++
	}
	return removed, nil
}

func removeLocalVideoAssets(localDir string, v *catalog.Video) error {
	if localDir == "" || v == nil || v.ID == "" {
		return nil
	}
	candidates := []string{
		v.PreviewLocal,
		filepath.Join(localDir, v.ID+".mp4"),
		filepath.Join(localDir, "thumbs", v.ID+".jpg"),
		filepath.Join(localDir, "transcodes", v.ID+".mp4"),
		filepath.Join(localDir, "transcodes", v.ID+".tmp.mp4"),
	}
	seen := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		clean, ok := localPathWithin(localDir, candidate)
		if !ok {
			continue
		}
		if _, ok := seen[clean]; ok {
			continue
		}
		seen[clean] = struct{}{}
		info, err := os.Stat(clean)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		if err := os.Remove(clean); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func localPathWithin(root, path string) (string, bool) {
	if strings.TrimSpace(root) == "" || strings.TrimSpace(path) == "" {
		return "", false
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", false
	}
	pathAbs, err := filepath.Abs(path)
	if err != nil {
		return "", false
	}
	rel, err := filepath.Rel(rootAbs, pathAbs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", false
	}
	return pathAbs, true
}

func (a *App) enqueueUploadedVideo(ctx context.Context, v *catalog.Video) {
	if v == nil {
		return
	}
	a.mu.Lock()
	worker := a.workers[v.DriveID]
	thumbWorker := a.thumbWorkers[v.DriveID]
	previewEnabled := a.previewEnabled
	a.mu.Unlock()

	if thumbWorker != nil && v.ThumbnailURL == "" {
		thumbWorker.Enqueue(v)
	}
	if previewEnabled && worker != nil {
		worker.Enqueue(v)
	}
}

func (a *App) regenPreview(ctx context.Context, videoID string) {
	v, err := a.cat.GetVideo(ctx, videoID)
	if err != nil {
		return
	}
	a.mu.Lock()
	worker := a.workers[v.DriveID]
	a.mu.Unlock()
	if worker != nil {
		worker.EnqueueBlocking(ctx, v)
	}
}

func (a *App) regenAllPreviews(ctx context.Context) {
	items, total, err := a.cat.ListVideos(ctx, catalog.ListParams{Page: 1, PageSize: 1000000})
	if err != nil {
		log.Printf("[preview] list all videos for regen: %v", err)
		return
	}
	log.Printf("[preview] enqueue all visible videos for regen count=%d total=%d", len(items), total)
	queued := 0
	for _, v := range items {
		if err := ctx.Err(); err != nil {
			log.Printf("[preview] enqueue all canceled after %d videos: %v", queued, err)
			return
		}
		a.mu.Lock()
		worker := a.workers[v.DriveID]
		a.mu.Unlock()
		if worker == nil {
			continue
		}
		if !worker.EnqueueBlocking(ctx, v) {
			log.Printf("[preview] enqueue all canceled after %d videos", queued)
			return
		}
		queued++
	}
	log.Printf("[preview] enqueued all visible videos for regen queued=%d", queued)
}

func (a *App) regenFailedPreviews(ctx context.Context, driveID string) {
	items, err := a.cat.ListVideosByPreviewStatus(ctx, driveID, "failed", 0)
	if err != nil {
		log.Printf("[preview] list failed videos for regen drive=%s: %v", driveID, err)
		return
	}
	a.mu.Lock()
	worker := a.workers[driveID]
	a.mu.Unlock()
	if worker == nil {
		log.Printf("[preview] regen failed drive=%s skipped: worker not found", driveID)
		return
	}
	log.Printf("[preview] enqueue failed videos for regen drive=%s count=%d", driveID, len(items))
	queued := 0
	for _, v := range items {
		if err := ctx.Err(); err != nil {
			log.Printf("[preview] enqueue failed canceled drive=%s queued=%d: %v", driveID, queued, err)
			return
		}
		if err := a.cat.UpdatePreview(ctx, v.ID, "", "", "pending"); err != nil {
			log.Printf("[preview] reset failed video %s drive=%s: %v", v.ID, driveID, err)
			continue
		}
		v.PreviewFileID = ""
		v.PreviewLocal = ""
		v.PreviewStatus = "pending"
		if !worker.EnqueueBlocking(ctx, v) {
			log.Printf("[preview] enqueue failed canceled drive=%s queued=%d", driveID, queued)
			return
		}
		queued++
	}
	log.Printf("[preview] enqueued failed videos for regen drive=%s queued=%d", driveID, queued)
}

func (a *App) scanLoop(ctx context.Context) {
	if a.cfg.Scanner.IntervalSeconds <= 0 {
		return
	}
	interval := time.Duration(a.cfg.Scanner.IntervalSeconds) * time.Second
	var lastScheduledScan time.Time
	if a.scanAllOnceIfDue(ctx, time.Now(), lastScheduledScan, interval) {
		lastScheduledScan = time.Now()
	}

	checkEvery := interval
	if checkEvery > time.Minute {
		checkEvery = time.Minute
	}
	ticker := time.NewTicker(checkEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if a.scanAllOnceIfDue(ctx, now, lastScheduledScan, interval) {
				lastScheduledScan = now
			}
		}
	}
}

func (a *App) scanAllOnceIfDue(ctx context.Context, now, lastScheduledScan time.Time, interval time.Duration) bool {
	if !scheduledScanDue(now, lastScheduledScan, interval) {
		return false
	}
	a.scanAllOnce(ctx)
	return true
}

func scheduledScanDue(now, lastScheduledScan time.Time, interval time.Duration) bool {
	if interval <= 0 || !scheduledScanAllowed(now) {
		return false
	}
	return lastScheduledScan.IsZero() || now.Sub(lastScheduledScan) >= interval
}

func scheduledScanAllowed(now time.Time) bool {
	hour := now.Hour()
	return hour >= 2 && hour < 7
}

func (a *App) scanAllOnce(ctx context.Context) {
	for _, d := range a.registry.All() {
		if !shouldScanDrive(d) {
			continue
		}
		a.runScan(ctx, d.ID())
	}
}

func shouldScanDrive(d drives.Drive) bool {
	return d != nil && d.ID() != localupload.DriveID
}

// ---------- middleware ----------

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", originOr(r, "*"))
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,PUT,DELETE,OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func originOr(r *http.Request, fallback string) string {
	if o := r.Header.Get("Origin"); o != "" {
		return o
	}
	return fallback
}

func parseBoolDefault(raw string, def bool) bool {
	if raw == "" {
		return def
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return def
	}
	return v
}
