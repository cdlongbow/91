package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path"
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
	"github.com/video-site/backend/internal/drives/spider91"
	"github.com/video-site/backend/internal/drives/wopan"
	"github.com/video-site/backend/internal/fingerprint"
	"github.com/video-site/backend/internal/nightly"
	"github.com/video-site/backend/internal/preview"
	"github.com/video-site/backend/internal/proxy"
	"github.com/video-site/backend/internal/scanner"
	"github.com/video-site/backend/internal/spider91migrate"
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
		cfg:                cfg,
		cat:                cat,
		registry:           proxy.NewRegistry(),
		workers:            make(map[string]*preview.Worker),
		thumbWorkers:       make(map[string]*preview.ThumbWorker),
		fingerprintWorkers: make(map[string]*fingerprint.Worker),
		spider91Crawlers:   make(map[string]*spider91.Crawler),
	}
	app.proxy = proxy.New(app.registry)
	app.spider91Migrator = spider91migrate.New(spider91migrate.Config{
		Catalog:          cat,
		Registry:         app.registry,
		GetTargetDriveID: func() string { return app.Spider91UploadDriveID() },
	})

	// 初始化现有 drives
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	app.loadTheme(ctx)
	app.loadSpider91UploadDriveID(ctx)
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
	setupRequired := config.RequiresAdminSetup(cfg)
	var setupMu sync.Mutex
	versionFilePath := strings.TrimSpace(os.Getenv("VIDEO_VERSION_FILE"))
	if versionFilePath == "" {
		versionFilePath = filepath.Join(filepath.Dir(cfgPath), ".version")
	}
	githubRepo := strings.TrimSpace(os.Getenv("VIDEO_GITHUB_REPO"))
	if githubRepo == "" {
		githubRepo = strings.TrimSpace(os.Getenv("GITHUB_REPO"))
	}

	apiServer := &api.Server{
		Catalog:   cat,
		Proxy:     app.proxy,
		LocalDir:  cfg.Storage.LocalPreviewDir,
		UploadDir: app.localUploadDir(),
		OnVideoUploaded: func(v *catalog.Video) {
			app.enqueueUploadedVideo(ctx, v)
		},
		GetTheme: func() string { return app.Theme() },
	}

	adminServer := &api.AdminServer{
		Catalog:         cat,
		Auth:            authr,
		VersionFilePath: versionFilePath,
		GitHubRepo:      githubRepo,
		SetupRequired: func() bool {
			setupMu.Lock()
			defer setupMu.Unlock()
			return setupRequired
		},
		OnSetup: func(username, password string) error {
			setupMu.Lock()
			defer setupMu.Unlock()
			if !setupRequired {
				return nil
			}
			if err := config.WriteAdminCredentials(cfgPath, username, password); err != nil {
				return err
			}
			cfg.Server.Admin.Username = username
			cfg.Server.Admin.Password = password
			authr.SetCredentials(username, password)
			setupRequired = false
			return nil
		},
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
			// spider91 的"重扫"等同于手动触发一次爬取；其它 drive 走标准 scan
			app.mu.Lock()
			_, isSpider91 := app.spider91Crawlers[driveID]
			app.mu.Unlock()
			if isSpider91 {
				go app.runSpider91Crawl(ctx, driveID)
				return
			}
			app.scheduleScan(ctx, driveID)
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
		OnRegenFailedThumbnails: func(driveID string) {
			go app.regenFailedThumbnails(ctx, driveID)
		},
		GetDriveGenerationStatuses: func() map[string]api.DriveGenerationStatuses {
			return app.driveGenerationStatuses()
		},
		OnTeaserEnabledChanged: func(driveID string, enabled bool) {
			// 从关到开时立刻补扫该盘 pending teaser，行为对齐旧的"全局开关从关到开"。
			// 关闭分支不需要做事 —— 入队前会重新查 catalog，新的 enqueue 自然停。
			if !enabled {
				return
			}
			app.mu.Lock()
			worker := app.workers[driveID]
			thumbWorker := app.thumbWorkers[driveID]
			app.mu.Unlock()
			go app.enqueueDriveGeneration(ctx, driveID, worker, thumbWorker)
		},
		GetTheme: func() string { return app.Theme() },
		SetTheme: func(theme string) error {
			return app.SetTheme(ctx, theme)
		},
		GetSpider91UploadDriveID: func() string { return app.Spider91UploadDriveID() },
		SetSpider91UploadDriveID: func(id string) error {
			return app.SetSpider91UploadDriveID(ctx, id)
		},
		OnRunNightlyJob: func() {
			if app.nightlyRunner != nil {
				app.nightlyRunner.TriggerNow()
			}
		},
		ListDriveDirChildren: func(reqCtx context.Context, driveID, parentID string) ([]api.DriveDirEntry, error) {
			return app.listDriveDirChildren(reqCtx, driveID, parentID)
		},
	}

	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(corsMiddleware(cfg.Server.AllowedOrigins))

	apiServer.RegisterRoutes(r, authr)
	adminServer.Register(r)
	mountFrontend(r)

	// 凌晨流水线：每天 cron_hour 触发一次，串行跑
	//   Phase 1 扫所有非 spider91 / localupload 网盘 + 删除检测 + 入队封面/teaser
	//   Phase 2 spider91 爬虫 + 入队 teaser
	//   Phase 3 spider91 → 云盘迁移
	// 也响应 admin "扫描所有网盘" 按钮（POST /admin/api/jobs/nightly/run → TriggerNow）。
	app.nightlyRunner = nightly.New(nightly.Config{
		Settings:              cat,
		CronHour:              cfg.Nightly.CronHour,
		MaxDuration:           cfg.Nightly.MaxDuration,
		ListScanTargets:       app.listScanTargetIDs,
		RunScan:               app.runScan,
		ListSpider91Drives:    app.listSpider91DriveIDs,
		RunSpider91Crawl:      app.runSpider91Crawl,
		WaitPreviewQueuesIdle: app.waitAllPreviewQueuesIdle,
		RunMigration:          app.spider91Migrator.RunOnce,
	})
	go app.nightlyRunner.Run(ctx)

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

	mu                 sync.Mutex
	workers            map[string]*preview.Worker
	thumbWorkers       map[string]*preview.ThumbWorker
	fingerprintWorkers map[string]*fingerprint.Worker
	cancels            map[string]context.CancelFunc
	// spider91Crawlers 按 driveID 索引，每个 spider91 drive 独立一个 Crawler
	spider91Crawlers map[string]*spider91.Crawler

	// 全站主题（"dark" | "pink"），从 DB 读
	theme string
	// 显式指定的 spider91 上传目标 drive ID。
	// 空字符串表示本地保存不上传，不再自动挑选 pikpak/p115 drive。
	spider91UploadDriveID string

	// spider91Migrator 周期把 spider91 视频上传到目标 drive（PikPak 或 115）。
	spider91Migrator *spider91migrate.Migrator

	// nightlyRunner 是凌晨流水线调度器：每天 cron_hour 串行跑扫盘 → 91 爬虫 → 迁移。
	// 也响应 admin 「扫描所有网盘」按钮（TriggerNow）。
	nightlyRunner *nightly.Runner

	// scanGlobalMu 串行化所有云盘扫盘任务，确保同一时刻全系统只有一个扫盘
	// 在跑（包括 admin 手动重扫和 nightly Phase 1）。即便用户同时点多个 drive
	// 的"重扫"按钮，goroutine 也会排队等这把锁，逐个执行。
	//
	// 设计取舍：
	//   - 不同 drive 的扫盘技术上可以并行（互不干涉），但用户希望"线性来"以
	//     避免带宽 / CPU 抢占，所以做全局串行。
	//   - nightly Phase 1 已经是 for 循环顺序调用 runScan，加了这把锁后行为
	//     不变，只是顺手把 admin 异步触发的请求也接入同一条队列。
	scanGlobalMu sync.Mutex
	// scanQueueMu 保护 scanQueued。
	scanQueueMu sync.Mutex
	// scanQueued 跟踪哪些 driveID 已经排队或正在跑，去重后续重复点击。
	// 一个 drive 在 scheduleScan 入队时被加入，在 runScan goroutine 结束时被移除。
	scanQueued map[string]bool
}

// teaserEnabledForDrive 查询某个 drive 当前的 per-drive teaser 开关。
//
// teaser 生成不再由全局 setting 控制，而是由 catalog.drives.teaser_enabled
// 决定。任何"是否入队 preview worker"的判断都应通过这个方法读，避免把状态
// 散落到 App 内存里和 DB 不一致。
//
// local-upload 是内置盘，不一定有 catalog.drives 行；缺省按开启处理。
//
// 其它 drive 读 catalog 失败时退化成 false（不生成）：比 "默认开" 更安全 —— 读不到
// 状态时倾向不消耗 ffmpeg；调用方会记日志，运维能立刻看到问题。
func (a *App) teaserEnabledForDrive(ctx context.Context, driveID string) bool {
	d, err := a.cat.GetDrive(ctx, driveID)
	if err != nil {
		if driveID == localupload.DriveID && errors.Is(err, sql.ErrNoRows) {
			return true
		}
		log.Printf("[preview] read teaser_enabled drive=%s: %v (treating as disabled)", driveID, err)
		return false
	}
	return d.TeaserEnabled
}

// Theme 线程安全读当前主题。
func (a *App) Theme() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.theme == "" {
		return "dark"
	}
	return a.theme
}

// SetTheme 切换并持久化主题；未知值会返回错误。
func (a *App) SetTheme(ctx context.Context, theme string) error {
	if theme != "dark" && theme != "pink" {
		return fmt.Errorf("unsupported theme %q", theme)
	}
	a.mu.Lock()
	a.theme = theme
	a.mu.Unlock()
	return a.cat.SetSetting(ctx, "ui.theme", theme)
}

// loadTheme 从 DB 读全站主题；找不到时回退到 "dark"。
func (a *App) loadTheme(ctx context.Context) {
	v, err := a.cat.GetSetting(ctx, "ui.theme", "dark")
	if err != nil {
		log.Printf("[theme] load setting: %v (fallback to dark)", err)
		a.mu.Lock()
		a.theme = "dark"
		a.mu.Unlock()
		return
	}
	if v != "pink" && v != "dark" {
		v = "dark"
	}
	a.mu.Lock()
	a.theme = v
	a.mu.Unlock()
}

// Spider91UploadDriveID 返回当前配置的 spider91 上传目标 drive ID。
// 空字符串表示本地保存不上传；只有管理员显式选择 pikpak/p115 drive 时才迁移上传。
func (a *App) Spider91UploadDriveID() string {
	a.mu.Lock()
	explicit := a.spider91UploadDriveID
	a.mu.Unlock()
	if explicit == "" {
		return ""
	}
	// 验证显式设置的 drive 仍然存在且 kind 合法；不在则视为未配置。
	if d, ok := a.registry.Get(explicit); ok && isSpider91UploadKind(d.Kind()) {
		return explicit
	}
	return ""
}

// SetSpider91UploadDriveID 设置 spider91 上传目标 drive ID 并持久化。
// 接受空字符串（本地保存不上传）。
// 设置一个不存在或 kind 不是 pikpak / p115 的 drive 会返回错误。
func (a *App) SetSpider91UploadDriveID(ctx context.Context, driveID string) error {
	driveID = strings.TrimSpace(driveID)
	if driveID != "" {
		d, ok := a.registry.Get(driveID)
		if !ok {
			return fmt.Errorf("drive %q not found", driveID)
		}
		if !isSpider91UploadKind(d.Kind()) {
			return fmt.Errorf("drive %q kind=%s, only pikpak or p115 can be spider91 upload target", driveID, d.Kind())
		}
	}
	a.mu.Lock()
	a.spider91UploadDriveID = driveID
	a.mu.Unlock()
	return a.cat.SetSetting(ctx, "spider91.upload_drive_id", driveID)
}

// isSpider91UploadKind 是 spider91 迁移目标盘的 allowlist。
// 与 spider91migrate.adaptUploadTarget 的支持范围保持一致。
func isSpider91UploadKind(kind string) bool {
	return kind == "pikpak" || kind == "p115"
}

// loadSpider91UploadDriveID 从 DB 读上传目标 drive ID 设置；不存在时使用空串。
func (a *App) loadSpider91UploadDriveID(ctx context.Context) {
	v, err := a.cat.GetSetting(ctx, "spider91.upload_drive_id", "")
	if err != nil {
		log.Printf("[spider91] load upload drive setting: %v", err)
		return
	}
	a.mu.Lock()
	a.spider91UploadDriveID = strings.TrimSpace(v)
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
	case spider91.Kind:
		drv = spider91.New(spider91.Config{
			ID:      d.ID,
			RootDir: a.spider91DriveDir(d.ID),
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
	})
	worker := preview.NewWorker(gen, a.cat, drv)
	thumbWorker := preview.NewThumbWorker(gen, a.cat, drv)
	fingerprintWorker := fingerprint.NewWorker(a.cat, drv, fingerprint.Config{})

	workerCtx, cancel := context.WithCancel(ctx)
	go worker.Run(workerCtx)
	go thumbWorker.Run(workerCtx)
	go fingerprintWorker.Run(workerCtx)

	a.registerPreviewWorkers(ctx, d.ID, worker, thumbWorker, fingerprintWorker, cancel)

	// spider91 driver 还需要一个 crawler，挂在专用 map 里供 crawlerLoop 调用
	if sd, ok := drv.(*spider91.Driver); ok {
		a.attachSpider91Crawler(d, sd)
	}

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
	})
	worker := preview.NewWorker(gen, a.cat, drv)
	thumbWorker := preview.NewThumbWorker(gen, a.cat, drv)
	fingerprintWorker := fingerprint.NewWorker(a.cat, drv, fingerprint.Config{})

	workerCtx, cancel := context.WithCancel(ctx)
	go worker.Run(workerCtx)
	go thumbWorker.Run(workerCtx)
	go fingerprintWorker.Run(workerCtx)

	a.registerPreviewWorkers(ctx, drv.ID(), worker, thumbWorker, fingerprintWorker, cancel)
	return nil
}

func (a *App) localUploadDir() string {
	return filepath.Join(filepath.Dir(a.cfg.Storage.LocalPreviewDir), "uploads")
}

// spider91RootDir 是所有 spider91 drive 共享的根目录。
func (a *App) spider91RootDir() string {
	return filepath.Join(filepath.Dir(a.cfg.Storage.LocalPreviewDir), "spider91")
}

// spider91DriveDir 是单个 spider91 drive 的存储目录：<root>/<driveID>。
func (a *App) spider91DriveDir(driveID string) string {
	return filepath.Join(a.spider91RootDir(), driveID)
}

// commonThumbsDir 是所有 drive 共享的封面目录，/p/thumb/{videoID} 路由命中这里。
func (a *App) commonThumbsDir() string {
	return filepath.Join(a.cfg.Storage.LocalPreviewDir, "thumbs")
}

// defaultSpider91ScriptPath 推断仓库里爬虫脚本的默认路径。
// 当前进程从 backend/ 启动时，脚本位于 ../91VideoSpider/spider_91porn.py。
// 找不到时返回空字符串，上层会在 RunOnce 时报错提示用户手动填 script_path。
func (a *App) defaultSpider91ScriptPath() string {
	candidates := []string{
		// 优先从配置目录的父目录定位
		filepath.Join(filepath.Dir(filepath.Dir(a.cfg.Storage.LocalPreviewDir)), "91VideoSpider", "spider_91porn.py"),
		// 仓库 root（cwd 在 backend/ 时）
		filepath.Join("..", "91VideoSpider", "spider_91porn.py"),
		// cwd 已经是仓库 root 时
		filepath.Join("91VideoSpider", "spider_91porn.py"),
	}
	for _, p := range candidates {
		abs, err := filepath.Abs(p)
		if err != nil {
			continue
		}
		if _, err := os.Stat(abs); err == nil {
			return abs
		}
	}
	return ""
}

// attachSpider91Crawler 创建该 drive 对应的 Crawler 并注册到 a.spider91Crawlers。
func (a *App) attachSpider91Crawler(d *catalog.Drive, drv *spider91.Driver) {
	pythonPath := strings.TrimSpace(d.Credentials["python_path"])
	if pythonPath == "" {
		pythonPath = "python3"
	}
	scriptPath := strings.TrimSpace(d.Credentials["script_path"])
	if scriptPath == "" {
		scriptPath = a.defaultSpider91ScriptPath()
	}
	// 91porn CDN 在海外；空缺时回退到 HTTPS_PROXY / HTTP_PROXY 环境变量。
	proxyURL := strings.TrimSpace(d.Credentials["proxy"])

	driveID := d.ID
	c := spider91.NewCrawler(spider91.CrawlerConfig{
		Driver:         drv,
		Catalog:        a.cat,
		PythonPath:     pythonPath,
		ScriptPath:     scriptPath,
		WorkDir:        filepath.Dir(scriptPath),
		CommonThumbDir: a.commonThumbsDir(),
		ProxyURL:       proxyURL,
		// 新流程：teaser 不在每条视频入库时立即入队，而是 RunOnce 全部下完后由
		// runSpider91Crawl 统一调 enqueueDriveGeneration 一次性入队。这样：
		//   - 下载阶段不和 ffmpeg 抢 CPU/IO
		//   - "等待 teaser 队列 idle" 在 nightly Phase 2 的语义上更直观
		// 不再传 OnNewVideo（crawler 内部的回调字段保留，仅为单测计数器之用）。
	})

	a.mu.Lock()
	a.spider91Crawlers[driveID] = c
	a.mu.Unlock()

	// 确保 "91porn" 系统标签存在，并把已入库的 spider91 视频按 author 字段
	// 匹配补打这个标签（CreateTagAndClassify 内部对所有视频走一遍 classify）。
	// 重复调用是幂等的：tags 用 INSERT OR IGNORE，video_tags 也是 INSERT OR IGNORE。
	bgCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	go func() {
		defer cancel()
		if _, err := a.cat.CreateTagAndClassify(bgCtx, spider91.DefaultTag, nil, "system"); err != nil {
			log.Printf("[spider91] ensure %q tag: %v", spider91.DefaultTag, err)
		}
	}()
}

func (a *App) registerPreviewWorkers(ctx context.Context, driveID string, worker *preview.Worker, thumbWorker *preview.ThumbWorker, fingerprintWorker *fingerprint.Worker, cancel context.CancelFunc) {
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
	if a.fingerprintWorkers == nil {
		a.fingerprintWorkers = make(map[string]*fingerprint.Worker)
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
	if fingerprintWorker != nil {
		a.fingerprintWorkers[driveID] = fingerprintWorker
	} else {
		delete(a.fingerprintWorkers, driveID)
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
	// 封面 worker 始终入队（与早期"全局 preview.enabled=false 时仍然生成封面"
	// 的行为一致）；teaser worker 仅在该 drive 的 TeaserEnabled 为 true 时入队。
	if thumbWorker != nil {
		a.enqueueThumbnails(ctx, driveID, thumbWorker)
	}
	if worker == nil || !a.teaserEnabledForDrive(ctx, driveID) {
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

func (a *App) enqueueFingerprints(ctx context.Context, driveID string, w *fingerprint.Worker) {
	if w == nil {
		return
	}
	pending, err := a.cat.ListVideosNeedingFingerprint(ctx, driveID, 0)
	if err != nil {
		log.Printf("[fingerprint] list pending %s: %v", driveID, err)
		return
	}
	if len(pending) == 0 {
		return
	}
	log.Printf("[fingerprint] enqueue %d videos for drive=%s", len(pending), driveID)
	for _, v := range pending {
		if !w.EnqueueBlocking(ctx, v) {
			log.Printf("[fingerprint] enqueue canceled for drive=%s", driveID)
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
	delete(a.fingerprintWorkers, id)
	delete(a.spider91Crawlers, id)
	a.mu.Unlock()
}

// listDriveDirChildren 实现 AdminServer.ListDriveDirChildren：
// 列指定 drive 在 parentID 下的直接子目录，仅返回目录条目（IsDir=true），文件忽略。
//
// parentID 为空时使用 drive 实例的 RootID()，与扫描起点保持一致 —— 但有意不
// 用 ScanRootID：用户在"设置跳过目录"弹窗里浏览的是整个网盘逻辑根，方便从 0
// 起逐层挑跳过点；ScanRootID 仅用于实际扫描起点。
//
// 性能优化：p115 的 Driver.List 走 SDK 的 ListWithLimit，会把目录里全部文件 +
// 目录分页拉完才返回；某些 115 根目录累积了几万个视频，单次列目录可能卡几十
// 秒（叠加 driver 的 2s 间隔限频）。所以 p115 走 ListDirsOnly 快路径：单页
// (1150)、按 file_type 排序，扫一遍只挑目录条目，1 次 API 调用搞定。其它网盘
// 走标准 List + IsDir 过滤 —— 它们的根目录通常不会有几万个文件。
//
// drive 未挂载（如凭证错误未通过 Init）时返回 error；前端展示 5xx 给用户。
func (a *App) listDriveDirChildren(ctx context.Context, driveID, parentID string) ([]api.DriveDirEntry, error) {
	drv, ok := a.registry.Get(driveID)
	if !ok {
		return nil, fmt.Errorf("drive %s not attached", driveID)
	}
	if parentID == "" {
		parentID = drv.RootID()
	}
	// p115 快路径：避免拉全部分页文件
	if fast, ok := drv.(interface {
		ListDirsOnly(ctx context.Context, dirID string) ([]drives.Entry, error)
	}); ok {
		entries, err := fast.ListDirsOnly(ctx, parentID)
		if err != nil {
			return nil, fmt.Errorf("list drive %s parent %s dirs-only: %w", driveID, parentID, err)
		}
		out := make([]api.DriveDirEntry, 0, len(entries))
		for _, e := range entries {
			out = append(out, api.DriveDirEntry{ID: e.ID, Name: e.Name})
		}
		return out, nil
	}
	// 通用路径
	entries, err := drv.List(ctx, parentID)
	if err != nil {
		return nil, fmt.Errorf("list drive %s parent %s: %w", driveID, parentID, err)
	}
	out := make([]api.DriveDirEntry, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir {
			continue
		}
		out = append(out, api.DriveDirEntry{ID: e.ID, Name: e.Name})
	}
	return out, nil
}

// scheduleScan 异步触发某个 drive 的扫盘。
//
// 调用立即返回；扫盘任务在后台 goroutine 里排队执行 —— 系统中所有扫盘共享
// 一把 scanGlobalMu，按提交顺序串行跑。
//
// 去重：如果该 drive 已经在排队或正在跑，重复请求会被丢弃并记日志。这样用户
// 反复点同一个 drive 的"重扫"按钮，也只会有一次实际工作。
//
// 用于 admin UI「重扫」、「立即抓取」这类异步触发；nightly Phase 1 应继续直接
// 调 runScan（同步、按 for 循环顺序），不需要走 scheduleScan。
func (a *App) scheduleScan(ctx context.Context, driveID string) {
	a.scanQueueMu.Lock()
	if a.scanQueued == nil {
		a.scanQueued = make(map[string]bool)
	}
	if a.scanQueued[driveID] {
		a.scanQueueMu.Unlock()
		log.Printf("[scan] drive=%s already queued or running, skip duplicate request", driveID)
		return
	}
	a.scanQueued[driveID] = true
	a.scanQueueMu.Unlock()

	go func() {
		defer func() {
			a.scanQueueMu.Lock()
			delete(a.scanQueued, driveID)
			a.scanQueueMu.Unlock()
		}()
		a.runScan(ctx, driveID)
	}()
}

func (a *App) runScan(ctx context.Context, driveID string) {
	// 全局串行：同一时刻只有一个扫盘任务在跑（admin 重扫 + nightly Phase 1 共用）。
	// 等待这把锁的 goroutine 在排队，按到达顺序逐个执行。
	a.scanGlobalMu.Lock()
	defer a.scanGlobalMu.Unlock()

	drv, ok := a.registry.Get(driveID)
	if !ok {
		log.Printf("[scan] drive %s not attached", driveID)
		return
	}

	a.mu.Lock()
	worker := a.workers[driveID]
	thumbWorker := a.thumbWorkers[driveID]
	fingerprintWorker := a.fingerprintWorkers[driveID]
	a.mu.Unlock()

	var onNew func(v *catalog.Video)
	if thumbWorker != nil {
		onNew = func(v *catalog.Video) {
			if thumbWorker != nil && v.ThumbnailURL == "" {
				thumbWorker.Enqueue(v)
			}
		}
	}

	// 使用 drive 的 scan_root_id，否则 root_id；同时把 admin 配置的 SkipDirIDs
	// 传给 scanner（命中即不递归）。
	d, err := a.cat.GetDrive(ctx, driveID)
	if err != nil {
		log.Printf("[scan] get drive %s: %v", driveID, err)
		return
	}
	sc := scanner.New(a.cat, drv, a.cfg.Scanner.VideoExtensions, d.SkipDirIDs, onNew)

	startID := d.ScanRootID
	if startID == "" {
		startID = d.RootID
	}

	log.Printf("[scan] drive=%s start=%s skip_dirs=%d", driveID, startID, len(d.SkipDirIDs))
	stats, err := sc.Run(ctx, startID)
	if err != nil {
		log.Printf("[scan] drive=%s error: %v", driveID, err)
		return
	}
	log.Printf("[scan] drive=%s done scanned=%d added=%d errors=%d", driveID, stats.Scanned, stats.Added, stats.Errors)
	// 删除检测：扫描到的 file_ids 是当前云盘上的真实存在；catalog 里这个 drive
	// 名下、且其 parent_id 处在本次扫描走过的目录内（或本次是从根扫的）、却
	// 不在 SeenFileIDs 中的视频 → 视为已被删除。
	//
	// spider91 / localupload 走自己的生命周期管理，不应该参与扫描清理；
	// stats.Errors > 0 时（云盘 API 中途抖动）保守起见跳过这一轮，避免把
	// "暂时列不出来"误认成"被用户删了"。
	if drv.Kind() != spider91.Kind && drv.ID() != localupload.DriveID {
		if stats.Errors > 0 {
			log.Printf("[cleanup] skip stale cleanup for drive=%s kind=%s: scan had %d directory errors", driveID, drv.Kind(), stats.Errors)
		} else {
			removed, err := a.cleanupMissingDriveVideos(ctx, driveID, stats.SeenFileIDs, stats.VisitedDirIDs, startID == drv.RootID())
			if err != nil {
				log.Printf("[cleanup] stale cleanup drive=%s kind=%s error: %v", driveID, drv.Kind(), err)
			} else if removed > 0 {
				log.Printf("[cleanup] removed %d stale videos for drive=%s kind=%s", removed, driveID, drv.Kind())
			}
		}
	}
	a.enqueueDriveGeneration(ctx, driveID, worker, thumbWorker)
	a.enqueueFingerprints(ctx, driveID, fingerprintWorker)
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
	fingerprintWorker := a.fingerprintWorkers[v.DriveID]
	a.mu.Unlock()

	if thumbWorker != nil && v.ThumbnailURL == "" {
		thumbWorker.Enqueue(v)
	}
	if worker != nil && a.teaserEnabledForDrive(ctx, v.DriveID) {
		worker.Enqueue(v)
	}
	if fingerprintWorker != nil {
		fingerprintWorker.Enqueue(v)
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
		if err := a.cat.UpdatePreview(ctx, v.ID, "", "pending"); err != nil {
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

// regenFailedThumbnails 把某 drive 下 thumbnail_status=failed 的视频全部重置为
// pending 并重新入队封面 worker。与 regenFailedPreviews 行为对称：那条管 teaser，
// 这条管封面图（两个 worker 是独立队列）。
//
// 操作不会触发已生成失败的视频重新去网盘取流 —— 只是把 catalog 的状态翻到 pending
// 并入队；真正的取链 / ffmpeg 在 thumb worker 里执行。
func (a *App) regenFailedThumbnails(ctx context.Context, driveID string) {
	items, err := a.cat.ListVideosByThumbnailStatus(ctx, driveID, "failed", 0)
	if err != nil {
		log.Printf("[thumb] list failed videos for regen drive=%s: %v", driveID, err)
		return
	}
	a.mu.Lock()
	thumbWorker := a.thumbWorkers[driveID]
	a.mu.Unlock()
	if thumbWorker == nil {
		log.Printf("[thumb] regen failed drive=%s skipped: thumb worker not found", driveID)
		return
	}
	log.Printf("[thumb] enqueue failed thumbnails for regen drive=%s count=%d", driveID, len(items))
	queued := 0
	for _, v := range items {
		if err := ctx.Err(); err != nil {
			log.Printf("[thumb] enqueue failed canceled drive=%s queued=%d: %v", driveID, queued, err)
			return
		}
		// 状态翻 pending；保留 thumbnail_url 字段（thumb worker 先看 url 是否已写
		// 来判断是否真的要再生）。但既然之前是 failed 说明 url 没写过，所以这里
		// 把 url 一并清空更稳。
		if err := a.cat.UpdateVideoMeta(ctx, v.ID, catalog.VideoMetaPatch{
			ThumbnailURL:    "",
			ThumbnailStatus: "pending",
		}); err != nil {
			log.Printf("[thumb] reset failed video %s drive=%s: %v", v.ID, driveID, err)
			continue
		}
		v.ThumbnailURL = ""
		if !thumbWorker.EnqueueBlocking(ctx, v) {
			log.Printf("[thumb] enqueue failed canceled drive=%s queued=%d", driveID, queued)
			return
		}
		queued++
	}
	log.Printf("[thumb] enqueued failed thumbnails for regen drive=%s queued=%d", driveID, queued)
}

// listScanTargetIDs 返回 nightly Phase 1 应扫描的所有 drive ID
// （非 spider91、非 localupload）。顺序按 registry.All 给的稳定顺序。
func (a *App) listScanTargetIDs(_ context.Context) []string {
	all := a.registry.All()
	out := make([]string, 0, len(all))
	for _, d := range all {
		if !shouldScanDrive(d) {
			continue
		}
		out = append(out, d.ID())
	}
	return out
}

// listSpider91DriveIDs 返回 nightly Phase 2 应触发爬取的 spider91 drive ID 列表。
func (a *App) listSpider91DriveIDs(_ context.Context) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.spider91Crawlers))
	for id := range a.spider91Crawlers {
		out = append(out, id)
	}
	return out
}

// waitAllPreviewQueuesIdle 阻塞直到所有 drive 的封面 worker 和 teaser worker
// 队列都为空且无 in-flight 任务。
//
// 顺序：先等所有 thumb worker（因为 enqueueDriveGeneration 内部已经先等当前
// drive 的封面再入队 teaser，但这里是跨 drive 的全局同步），再等所有 teaser。
// 若 ctx 在等待中被取消（软超时 / shutdown），立即返回 ctx.Err。
func (a *App) waitAllPreviewQueuesIdle(ctx context.Context) error {
	a.mu.Lock()
	thumbWorkers := make([]*preview.ThumbWorker, 0, len(a.thumbWorkers))
	previewWorkers := make([]*preview.Worker, 0, len(a.workers))
	for _, w := range a.thumbWorkers {
		thumbWorkers = append(thumbWorkers, w)
	}
	for _, w := range a.workers {
		previewWorkers = append(previewWorkers, w)
	}
	a.mu.Unlock()

	for _, w := range thumbWorkers {
		if err := w.WaitIdle(ctx); err != nil {
			return err
		}
	}
	for _, w := range previewWorkers {
		if err := w.WaitIdle(ctx); err != nil {
			return err
		}
	}
	return nil
}

func shouldScanDrive(d drives.Drive) bool {
	if d == nil || d.ID() == localupload.DriveID {
		return false
	}
	// spider91 由专用的 crawlerLoop 触发，不参与 scanLoop
	if d.Kind() == spider91.Kind {
		return false
	}
	return true
}

// ---------- spider91 crawl ----------

// runSpider91Crawl 运行一次完整爬取流程并把 last_crawl_at 写回 drive.credentials。
//
// 即使爬取失败也会更新 last_crawl_at，避免一直在错误循环里反复触发；下一次 nightly
// 流水线重跑时仍会重试。该方法是阻塞的，被 nightly Phase 2 串行调用，以及被
// admin "立即抓取" 单 drive 异步调用。
func (a *App) runSpider91Crawl(ctx context.Context, driveID string) {
	a.mu.Lock()
	c := a.spider91Crawlers[driveID]
	a.mu.Unlock()
	if c == nil {
		return
	}

	d, err := a.cat.GetDrive(ctx, driveID)
	if err != nil || d == nil {
		log.Printf("[spider91] drive=%s lookup failed: %v", driveID, err)
		return
	}
	targetNew := spider91IntCred(d, "target_new", spider91.DefaultTargetNew)
	if targetNew <= 0 {
		targetNew = spider91.DefaultTargetNew
	}

	log.Printf("[spider91] drive=%s start crawl target_new=%d", driveID, targetNew)
	res, runErr := c.RunOnce(ctx, targetNew)
	if runErr != nil {
		log.Printf("[spider91] drive=%s crawl failed: %v", driveID, runErr)
	} else if res != nil {
		log.Printf("[spider91] drive=%s crawl done target=%d total=%d new=%d skipped=%d failed=%d seen_snapshot=%d",
			driveID, res.TargetNew, res.TotalEntries, res.NewVideos, res.Skipped, res.Failed, res.SeenSnapshot)
	}

	// 标记最后一次爬取时间。这字段已不再用于调度判定（nightly 流水线统一调度），
	// 留着仅作为 admin UI 显示"上次抓取 N 小时前"用。
	if d.Credentials == nil {
		d.Credentials = make(map[string]string)
	}
	d.Credentials["last_crawl_at"] = strconv.FormatInt(time.Now().Unix(), 10)
	if runErr != nil {
		d.Status = "error"
		d.LastError = runErr.Error()
	} else {
		d.Status = "ok"
		d.LastError = ""
	}
	if err := a.cat.UpsertDrive(ctx, d); err != nil {
		log.Printf("[spider91] drive=%s update last_crawl_at: %v", driveID, err)
	}

	// 爬取全部完成后，统一把所有还 pending 的 teaser 入队。
	// 这是新流水线设计：crawler 自身不再每条入库就立即触发 teaser 生成，
	// 让"下载阶段"和"teaser 阶段"在时间上分清楚（也跟 nightly Phase 2
	// 的"等 teaser 队列 idle"语义对齐）。enqueueDriveGeneration 内部会读
	// 该 drive 当前的 teaser_enabled，关闭时是 noop。
	a.mu.Lock()
	worker := a.workers[driveID]
	thumbWorker := a.thumbWorkers[driveID]
	fingerprintWorker := a.fingerprintWorkers[driveID]
	a.mu.Unlock()
	a.enqueueDriveGeneration(ctx, driveID, worker, thumbWorker)
	a.enqueueFingerprints(ctx, driveID, fingerprintWorker)
}

// spider91IntCred 解析 credentials 中的整数字段，缺省时返回 def。
func spider91IntCred(d *catalog.Drive, key string, def int) int {
	if d == nil || d.Credentials == nil {
		return def
	}
	raw := strings.TrimSpace(d.Credentials[key])
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	return v
}

// ---------- middleware ----------

// corsMiddleware 返回一个 chi 中间件，按白名单匹配 Origin 决定是否回写
// CORS 响应头。
//
// 设计要点：
//   - 不再反射任意 Origin。Origin 必须出现在 allowedOrigins 中才会得到
//     Access-Control-Allow-Origin / Allow-Credentials 的"放行"响应头；
//     不在白名单的跨源请求拿不到这些头，浏览器会拒绝读响应内容。
//   - 同源请求（浏览器不发 Origin 头，或 Origin 等于自己）不需要 CORS 头，
//     直接放行。
//   - 始终带 Vary: Origin，避免反代缓存把 A Origin 的允许头喂给 B Origin。
//   - 对不在白名单的 OPTIONS 预检直接 403，避免被当成"放行"信号。
//
// allowedOrigins 由 config.Server.AllowedOrigins 注入；默认为空 = 完全
// 不允许跨源（最安全的默认值，同源部署不受影响）。
func corsMiddleware(allowedOrigins []string) func(http.Handler) http.Handler {
	allow := make(map[string]struct{}, len(allowedOrigins))
	for _, o := range allowedOrigins {
		o = strings.TrimSpace(o)
		if o == "" || o == "*" {
			// 通配符在带 cookie 的 CORS 下没意义且危险，直接忽略
			continue
		}
		allow[o] = struct{}{}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")

			// 任何走过 CORS 检查的响应都要带 Vary: Origin，避免缓存污染。
			w.Header().Add("Vary", "Origin")

			isAllowedOrigin := false
			if origin != "" {
				_, isAllowedOrigin = allow[origin]
			}

			if isAllowedOrigin {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Credentials", "true")
				w.Header().Set("Access-Control-Allow-Methods", "GET,POST,PUT,DELETE,OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
				w.Header().Set("Access-Control-Max-Age", "600")
			}

			if r.Method == http.MethodOptions {
				// 预检请求：只对白名单 Origin 返回 204；否则 403 让浏览器把请求拦下来。
				// 同源场景一般不会触发预检（浏览器只在跨源 + 复杂请求时才发 OPTIONS）。
				if isAllowedOrigin {
					w.WriteHeader(http.StatusNoContent)
					return
				}
				if origin != "" {
					http.Error(w, "cors: origin not allowed", http.StatusForbidden)
					return
				}
				// 没带 Origin 的 OPTIONS 不是 CORS 预检（可能是健康检查工具），
				// 直接交给下游处理。
			}

			next.ServeHTTP(w, r)
		})
	}
}

func mountFrontend(r chi.Router) {
	dir := strings.TrimSpace(os.Getenv("VIDEO_FRONTEND_DIR"))
	if dir == "" {
		dir = "./dist"
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return
	}
	indexPath := filepath.Join(dir, "index.html")
	if st, err := os.Stat(indexPath); err != nil || st.IsDir() {
		return
	}
	log.Printf("serving frontend from %s", dir)
	r.NotFound(frontendHandler(dir))
}

func frontendHandler(dir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.NotFound(w, r)
			return
		}
		if isBackendRoute(r.URL.Path) {
			http.NotFound(w, r)
			return
		}

		cleanPath := path.Clean("/" + r.URL.Path)
		rel := strings.TrimPrefix(cleanPath, "/")
		if rel != "" && rel != "." {
			name := filepath.FromSlash(rel)
			f, err := os.Open(filepath.Join(dir, name))
			if err == nil {
				defer f.Close()
				if st, statErr := f.Stat(); statErr == nil && !st.IsDir() {
					http.ServeContent(w, r, st.Name(), st.ModTime(), f)
					return
				}
			}
			if filepath.Ext(name) != "" {
				http.NotFound(w, r)
				return
			}
		}

		http.ServeFile(w, r, filepath.Join(dir, "index.html"))
	}
}

func isBackendRoute(p string) bool {
	return p == "/api" ||
		strings.HasPrefix(p, "/api/") ||
		p == "/admin/api" ||
		strings.HasPrefix(p, "/admin/api/") ||
		p == "/p" ||
		strings.HasPrefix(p, "/p/")
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
