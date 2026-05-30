package main

import (
	"context"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/video-site/backend/internal/catalog"
	"github.com/video-site/backend/internal/config"
	"github.com/video-site/backend/internal/drives"
	"github.com/video-site/backend/internal/fingerprint"
	"github.com/video-site/backend/internal/preview"
	"github.com/video-site/backend/internal/proxy"
)

func TestRegisterPreviewWorkerBackfillsPendingWhenDriveTeaserEnabled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	seedDriveWithTeaser(t, cat, "drive-id", true)
	video := &catalog.Video{
		ID:            "video-1",
		DriveID:       "drive-id",
		FileID:        "file-id",
		Title:         "Clip",
		PreviewStatus: "pending",
		PublishedAt:   time.Now(),
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
	}
	if err := cat.UpsertVideo(ctx, video); err != nil {
		t.Fatalf("seed video: %v", err)
	}

	app := &App{
		cat:          cat,
		workers:      make(map[string]*preview.Worker),
		thumbWorkers: make(map[string]*preview.ThumbWorker),
	}
	worker := preview.NewWorker(&serverFakeTeaserGenerator{}, cat, &serverFakeDrive{})
	go worker.Run(ctx)

	app.registerPreviewWorkers(ctx, "drive-id", worker, nil, nil, func() {})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, err := cat.GetVideo(ctx, video.ID)
		if err != nil {
			t.Fatalf("get video: %v", err)
		}
		if got.PreviewStatus == "ready" {
			if got.PreviewLocal != "/tmp/video-1.mp4" {
				t.Fatalf("preview local = %q, want generated local teaser path", got.PreviewLocal)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}

	got, err := cat.GetVideo(ctx, video.ID)
	if err != nil {
		t.Fatalf("get video after timeout: %v", err)
	}
	t.Fatalf("preview status = %q, want ready", got.PreviewStatus)
}

func TestRegisterPreviewWorkersRunThumbnailsAndPreviewsIndependently(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	seedDriveWithTeaser(t, cat, "drive-id", true)
	now := time.Now()
	video := &catalog.Video{
		ID:            "video-1",
		DriveID:       "drive-id",
		FileID:        "file-1",
		Title:         "Clip 1",
		PreviewStatus: "pending",
		PublishedAt:   now,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := cat.UpsertVideo(ctx, video); err != nil {
		t.Fatalf("seed video: %v", err)
	}

	app := &App{
		cat:          cat,
		workers:      make(map[string]*preview.Worker),
		thumbWorkers: make(map[string]*preview.ThumbWorker),
	}
	gen := &serverBlockingThumbGenerator{
		started: make(chan string, 1),
		release: make(chan struct{}),
	}
	drv := &serverFakeDrive{}
	worker := preview.NewWorker(gen, cat, drv)
	thumbWorker := preview.NewThumbWorker(gen, cat, drv)
	go worker.Run(ctx)
	go thumbWorker.Run(ctx)

	app.registerPreviewWorkers(ctx, "drive-id", worker, thumbWorker, nil, func() {})

	select {
	case got := <-gen.started:
		if got != video.ID {
			t.Fatalf("thumbnail started for %q, want %q", got, video.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("thumbnail generation did not start")
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, err := cat.GetVideo(ctx, video.ID)
		if err != nil {
			t.Fatalf("get video: %v", err)
		}
		if got.PreviewStatus == "ready" {
			if got.ThumbnailURL != "" {
				t.Fatalf("thumbnail url = %q, want preview ready while thumbnail is still blocked", got.ThumbnailURL)
			}
			close(gen.release)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}

	got, err := cat.GetVideo(ctx, video.ID)
	if err != nil {
		t.Fatalf("get video after timeout: %v", err)
	}
	t.Fatalf("preview status=%q thumbnail=%q, want preview ready before thumbnail finishes", got.PreviewStatus, got.ThumbnailURL)
}

func TestRegisterPreviewWorkersBackfillsHistoricalFingerprints(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	dataPath := filepath.Join(t.TempDir(), "video.mp4")
	data := []byte("historical video content for fingerprint")
	if err := os.WriteFile(dataPath, data, 0o644); err != nil {
		t.Fatalf("write video data: %v", err)
	}

	now := time.Now()
	video := &catalog.Video{
		ID:                "historical-video",
		DriveID:           "drive-id",
		FileID:            "file-id",
		Title:             "Historical",
		Size:              int64(len(data)),
		FingerprintStatus: "pending",
		PublishedAt:       now,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	if err := cat.UpsertVideo(ctx, video); err != nil {
		t.Fatalf("seed video: %v", err)
	}

	app := &App{
		cat:                cat,
		workers:            make(map[string]*preview.Worker),
		thumbWorkers:       make(map[string]*preview.ThumbWorker),
		fingerprintWorkers: make(map[string]*fingerprint.Worker),
	}
	drv := &serverFingerprintFakeDrive{path: dataPath}
	fingerprintWorker := fingerprint.NewWorker(cat, drv, fingerprint.Config{})
	go fingerprintWorker.Run(ctx)

	app.registerPreviewWorkers(ctx, "drive-id", nil, nil, fingerprintWorker, func() {})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, err := cat.GetVideo(ctx, video.ID)
		if err != nil {
			t.Fatalf("get video: %v", err)
		}
		if got.SampledSHA256 != "" && got.FingerprintStatus == "ready" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	got, err := cat.GetVideo(ctx, video.ID)
	if err != nil {
		t.Fatalf("get video after timeout: %v", err)
	}
	t.Fatalf("fingerprint status=%q sampled=%q, want ready with hash", got.FingerprintStatus, got.SampledSHA256)
}

func TestRunScanStartsFingerprintBeforeThumbnailAndPreviewDrain(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})
	seedDriveWithTeaser(t, cat, "drive-id", true)

	dataPath := filepath.Join(t.TempDir(), "scan-video.mp4")
	data := []byte("scan video content for independent fingerprint")
	if err := os.WriteFile(dataPath, data, 0o644); err != nil {
		t.Fatalf("write video data: %v", err)
	}

	drv := &serverScanFingerprintFakeDrive{
		serverFingerprintFakeDrive: serverFingerprintFakeDrive{path: dataPath},
		entries: []drives.Entry{{
			ID:       "file-id",
			Name:     "scan-video.mp4",
			Size:     int64(len(data)),
			ParentID: "root",
		}},
	}
	registry := proxy.NewRegistry()
	registry.Set("drive-id", drv)

	gen := &serverFakeTeaserGenerator{}
	worker := preview.NewWorker(gen, cat, drv)
	thumbWorker := preview.NewThumbWorker(gen, cat, drv)
	fingerprintWorker := fingerprint.NewWorker(cat, drv, fingerprint.Config{})
	go fingerprintWorker.Run(ctx)

	app := &App{
		cfg: &config.Config{
			Scanner: config.Scanner{VideoExtensions: []string{".mp4"}},
		},
		cat:                cat,
		registry:           registry,
		workers:            map[string]*preview.Worker{"drive-id": worker},
		thumbWorkers:       map[string]*preview.ThumbWorker{"drive-id": thumbWorker},
		fingerprintWorkers: map[string]*fingerprint.Worker{"drive-id": fingerprintWorker},
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		app.runScan(ctx, "drive-id")
	}()

	videoID := "fake-drive-id-file-id"
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, err := cat.GetVideo(ctx, videoID)
		if err == nil && got.SampledSHA256 != "" && got.FingerprintStatus == "ready" {
			cancel()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("scan did not stop after context cancel")
			}
			if got.ThumbnailURL != "" {
				t.Fatalf("thumbnail url = %q, want fingerprint before thumbnail generation", got.ThumbnailURL)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("scan did not stop after context cancel")
	}
	got, err := cat.GetVideo(context.Background(), videoID)
	if err != nil {
		t.Fatalf("get video after timeout: %v", err)
	}
	t.Fatalf("fingerprint status=%q sampled=%q, want ready before thumbnail/preview drain", got.FingerprintStatus, got.SampledSHA256)
}

func TestNightlyTargetsComeFromCatalogBeforeDriveAttach(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	for _, d := range []*catalog.Drive{
		{ID: "115", Kind: "p115", Name: "115", RootID: "0", TeaserEnabled: true},
		{ID: "pikpak", Kind: "pikpak", Name: "PikPak", RootID: "0", TeaserEnabled: true},
		{ID: "91-spider", Kind: "spider91", Name: "91 Spider", RootID: "0", TeaserEnabled: true},
	} {
		if err := cat.UpsertDrive(ctx, d); err != nil {
			t.Fatalf("seed drive %s: %v", d.ID, err)
		}
	}

	app := &App{cat: cat}
	scanIDs := app.listScanTargetIDs(ctx)
	if len(scanIDs) != 2 || scanIDs[0] != "115" || scanIDs[1] != "pikpak" {
		t.Fatalf("scan target ids = %#v, want 115 and pikpak from catalog", scanIDs)
	}
	spiderIDs := app.listSpider91DriveIDs(ctx)
	if len(spiderIDs) != 1 || spiderIDs[0] != "91-spider" {
		t.Fatalf("spider91 ids = %#v, want catalog spider drive", spiderIDs)
	}
}

func TestFailedThumbnailsDoNotBlockPreviewGeneration(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	seedDriveWithTeaser(t, cat, "drive-id", true)
	now := time.Now()
	video := &catalog.Video{
		ID:            "video-failed-thumb",
		DriveID:       "drive-id",
		FileID:        "file-1",
		Title:         "Clip With Failed Thumb",
		PreviewStatus: "pending",
		PublishedAt:   now,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := cat.UpsertVideo(ctx, video); err != nil {
		t.Fatalf("seed video: %v", err)
	}
	if err := cat.UpdateVideoMeta(ctx, video.ID, catalog.VideoMetaPatch{ThumbnailStatus: "failed"}); err != nil {
		t.Fatalf("mark thumbnail failed: %v", err)
	}
	missing, err := cat.CountVideosNeedingThumbnail(ctx, "drive-id")
	if err != nil {
		t.Fatalf("count missing thumbnails: %v", err)
	}
	if missing != 0 {
		t.Fatalf("missing thumbnails = %d, want failed thumbnails excluded", missing)
	}

	app := &App{
		cat:          cat,
		workers:      make(map[string]*preview.Worker),
		thumbWorkers: make(map[string]*preview.ThumbWorker),
	}
	gen := &serverFakeTeaserGenerator{}
	drv := &serverFakeDrive{}
	worker := preview.NewWorker(gen, cat, drv)
	thumbWorker := preview.NewThumbWorker(gen, cat, drv)
	go worker.Run(ctx)
	go thumbWorker.Run(ctx)

	app.registerPreviewWorkers(ctx, "drive-id", worker, thumbWorker, nil, func() {})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, err := cat.GetVideo(ctx, video.ID)
		if err != nil {
			t.Fatalf("get video: %v", err)
		}
		if got.PreviewStatus == "ready" {
			events := gen.Events()
			if len(events) != 1 || events[0] != "preview:"+video.ID {
				t.Fatalf("events = %#v, want preview only", events)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}

	got, err := cat.GetVideo(ctx, video.ID)
	if err != nil {
		t.Fatalf("get video after timeout: %v", err)
	}
	t.Fatalf("preview status = %q, want ready; events=%#v", got.PreviewStatus, gen.Events())
}

func TestRegenFailedPreviewsQueuesOnlyFailedVideosForDrive(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	seedDriveWithTeaser(t, cat, "drive-id", true)
	seedDriveWithTeaser(t, cat, "other-drive", true)
	now := time.Now()
	for _, v := range []*catalog.Video{
		{ID: "target-failed", DriveID: "drive-id", FileID: "file-1", Title: "Target Failed", PreviewStatus: "failed"},
		{ID: "target-ready", DriveID: "drive-id", FileID: "file-2", Title: "Target Ready", PreviewStatus: "ready", PreviewLocal: "/tmp/ready.mp4"},
		{ID: "other-failed", DriveID: "other-drive", FileID: "file-3", Title: "Other Failed", PreviewStatus: "failed"},
	} {
		v.PublishedAt = now
		v.CreatedAt = now
		v.UpdatedAt = now
		if err := cat.UpsertVideo(ctx, v); err != nil {
			t.Fatalf("seed video %s: %v", v.ID, err)
		}
	}

	app := &App{
		cat:          cat,
		workers:      make(map[string]*preview.Worker),
		thumbWorkers: make(map[string]*preview.ThumbWorker),
	}
	worker := preview.NewWorker(&serverFakeTeaserGenerator{}, cat, &serverFakeDrive{})
	go worker.Run(ctx)
	app.mu.Lock()
	app.workers["drive-id"] = worker
	app.mu.Unlock()

	app.regenFailedPreviews(ctx, "drive-id")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, err := cat.GetVideo(ctx, "target-failed")
		if err != nil {
			t.Fatalf("get target failed: %v", err)
		}
		if got.PreviewStatus == "ready" {
			if got.PreviewLocal != "/tmp/target-failed.mp4" {
				t.Fatalf("target preview local = %q, want regenerated local teaser path", got.PreviewLocal)
			}
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	target, err := cat.GetVideo(ctx, "target-failed")
	if err != nil {
		t.Fatalf("get regenerated target: %v", err)
	}
	if target.PreviewStatus != "ready" {
		t.Fatalf("target preview status = %q, want ready", target.PreviewStatus)
	}
	ready, err := cat.GetVideo(ctx, "target-ready")
	if err != nil {
		t.Fatalf("get target ready: %v", err)
	}
	if ready.PreviewLocal != "/tmp/ready.mp4" || ready.PreviewStatus != "ready" {
		t.Fatalf("ready video changed: status=%q local=%q", ready.PreviewStatus, ready.PreviewLocal)
	}
	other, err := cat.GetVideo(ctx, "other-failed")
	if err != nil {
		t.Fatalf("get other failed: %v", err)
	}
	if other.PreviewStatus != "failed" {
		t.Fatalf("other drive preview status = %q, want failed", other.PreviewStatus)
	}
}

func TestEnqueueUploadedVideoQueuesLocalGenerationByDefault(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	video := &catalog.Video{
		ID:            "local-upload-video",
		DriveID:       "local-upload",
		FileID:        "upload-1.mp4",
		Title:         "Uploaded",
		PreviewStatus: "pending",
		PublishedAt:   time.Now(),
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
	}
	if err := cat.UpsertVideo(ctx, video); err != nil {
		t.Fatalf("seed video: %v", err)
	}

	app := &App{
		cat:          cat,
		workers:      make(map[string]*preview.Worker),
		thumbWorkers: make(map[string]*preview.ThumbWorker),
	}
	gen := &serverFakeTeaserGenerator{}
	drv := &serverLocalUploadFakeDrive{}
	worker := preview.NewWorker(gen, cat, drv)
	thumbWorker := preview.NewThumbWorker(gen, cat, drv)
	go worker.Run(ctx)
	go thumbWorker.Run(ctx)
	app.mu.Lock()
	app.workers["local-upload"] = worker
	app.thumbWorkers["local-upload"] = thumbWorker
	app.mu.Unlock()

	app.enqueueUploadedVideo(ctx, video)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, err := cat.GetVideo(ctx, video.ID)
		if err != nil {
			t.Fatalf("get video: %v", err)
		}
		if got.PreviewStatus == "ready" && got.ThumbnailURL != "" {
			if got.PreviewLocal != "/tmp/local-upload-video.mp4" {
				t.Fatalf("preview local = %q, want generated local teaser path", got.PreviewLocal)
			}
			if got.ThumbnailURL != "/p/thumb/local-upload-video" {
				t.Fatalf("thumbnail url = %q, want generated thumbnail URL", got.ThumbnailURL)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}

	got, err := cat.GetVideo(ctx, video.ID)
	if err != nil {
		t.Fatalf("get video after timeout: %v", err)
	}
	t.Fatalf("preview status = %q, thumbnail url = %q; want generated local teaser and thumbnail", got.PreviewStatus, got.ThumbnailURL)
}

func TestShouldScanDriveSkipsLocalUpload(t *testing.T) {
	if shouldScanDrive(&serverLocalUploadFakeDrive{}) {
		t.Fatal("local upload drive should not be scanned")
	}
	if !shouldScanDrive(&serverFakeDrive{}) {
		t.Fatal("normal drive should be scanned")
	}
}

func TestCleanupMissingPikPakVideosRemovesDatabaseRowsAndLocalAssets(t *testing.T) {
	ctx := context.Background()
	localDir := t.TempDir()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	obsoletePreview := filepath.Join(localDir, "obsolete.mp4")
	obsoleteThumb := filepath.Join(localDir, "thumbs", "pikpak-PikPak-obsolete.jpg")
	keptPreview := filepath.Join(localDir, "kept.mp4")
	for _, path := range []string{obsoletePreview, obsoleteThumb, keptPreview} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
		if err := os.WriteFile(path, []byte("asset"), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	now := time.Now()
	for _, v := range []*catalog.Video{
		{
			ID:            "pikpak-PikPak-obsolete",
			DriveID:       "PikPak",
			FileID:        "obsolete",
			Title:         "Obsolete",
			PreviewStatus: "ready",
			PreviewLocal:  obsoletePreview,
		},
		{
			ID:            "pikpak-PikPak-kept",
			DriveID:       "PikPak",
			FileID:        "kept",
			Title:         "Kept",
			PreviewStatus: "ready",
			PreviewLocal:  keptPreview,
		},
		{
			ID:            "onedrive-OneDrive-obsolete",
			DriveID:       "OneDrive",
			FileID:        "obsolete",
			Title:         "Other Drive",
			PreviewStatus: "ready",
		},
	} {
		v.PublishedAt = now
		v.CreatedAt = now
		v.UpdatedAt = now
		if err := cat.UpsertVideo(ctx, v); err != nil {
			t.Fatalf("seed %s: %v", v.ID, err)
		}
	}

	app := &App{
		cfg: &config.Config{Storage: config.Storage{LocalPreviewDir: localDir}},
		cat: cat,
	}
	removed, err := app.cleanupMissingDriveVideos(ctx, "PikPak", map[string]struct{}{"kept": {}}, nil, true)
	if err != nil {
		t.Fatalf("cleanup missing videos: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	if _, err := cat.GetVideo(ctx, "pikpak-PikPak-obsolete"); err != sql.ErrNoRows {
		t.Fatalf("obsolete video lookup error = %v, want sql.ErrNoRows", err)
	}
	if _, err := cat.GetVideo(ctx, "pikpak-PikPak-kept"); err != nil {
		t.Fatalf("kept video missing after cleanup: %v", err)
	}
	if _, err := cat.GetVideo(ctx, "onedrive-OneDrive-obsolete"); err != nil {
		t.Fatalf("other drive video missing after cleanup: %v", err)
	}
	for _, path := range []string{obsoletePreview, obsoleteThumb} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("obsolete asset %s still exists, stat err=%v", path, err)
		}
	}
	if _, err := os.Stat(keptPreview); err != nil {
		t.Fatalf("kept preview missing: %v", err)
	}
}

func TestCleanupDuplicateVideoAssetsRemovesOnlyDuplicateLocalAssets(t *testing.T) {
	ctx := context.Background()
	localDir := t.TempDir()
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	canonicalPreview := filepath.Join(localDir, "canonical.mp4")
	duplicatePreview := filepath.Join(localDir, "duplicate.mp4")
	canonicalThumb := filepath.Join(localDir, "thumbs", "canonical-video.jpg")
	duplicateThumb := filepath.Join(localDir, "thumbs", "duplicate-video.jpg")
	for _, path := range []string{canonicalPreview, duplicatePreview, canonicalThumb, duplicateThumb} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
		if err := os.WriteFile(path, []byte("asset"), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	now := time.Date(2026, 5, 29, 12, 0, 0, 0, time.UTC)
	for _, v := range []*catalog.Video{
		{
			ID:            "canonical-video",
			DriveID:       "115",
			FileID:        "file-a",
			Title:         "Canonical",
			Size:          2048,
			ThumbnailURL:  "/p/thumb/canonical-video",
			PreviewLocal:  canonicalPreview,
			PreviewStatus: "ready",
			PublishedAt:   now,
			CreatedAt:     now,
			UpdatedAt:     now,
		},
		{
			ID:            "duplicate-video",
			DriveID:       "onedrive",
			FileID:        "file-b",
			Title:         "Duplicate",
			Size:          2048,
			ThumbnailURL:  "/p/thumb/duplicate-video",
			PreviewLocal:  duplicatePreview,
			PreviewStatus: "ready",
			PublishedAt:   now.Add(time.Second),
			CreatedAt:     now.Add(time.Second),
			UpdatedAt:     now.Add(time.Second),
		},
	} {
		if err := cat.UpsertVideo(ctx, v); err != nil {
			t.Fatalf("seed %s: %v", v.ID, err)
		}
		if err := cat.UpdateVideoFingerprint(ctx, v.ID, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "ready", ""); err != nil {
			t.Fatalf("fingerprint %s: %v", v.ID, err)
		}
	}

	app := &App{
		cfg: &config.Config{Storage: config.Storage{LocalPreviewDir: localDir}},
		cat: cat,
	}
	if err := app.cleanupDuplicateVideoAssets(ctx); err != nil {
		t.Fatalf("cleanup duplicate video assets: %v", err)
	}

	for _, path := range []string{canonicalPreview, canonicalThumb} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("canonical asset %s missing: %v", path, err)
		}
	}
	for _, path := range []string{duplicatePreview, duplicateThumb} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("duplicate asset %s still exists, stat err=%v", path, err)
		}
	}
	dup, err := cat.GetVideo(ctx, "duplicate-video")
	if err != nil {
		t.Fatalf("get duplicate: %v", err)
	}
	if dup.PreviewLocal != "" || dup.PreviewStatus != "pending" {
		t.Fatalf("duplicate preview local=%q status=%q, want empty pending", dup.PreviewLocal, dup.PreviewStatus)
	}
	if dup.ThumbnailURL != "" {
		t.Fatalf("duplicate thumbnail url = %q, want empty", dup.ThumbnailURL)
	}
	canon, err := cat.GetVideo(ctx, "canonical-video")
	if err != nil {
		t.Fatalf("get canonical: %v", err)
	}
	if canon.PreviewLocal != canonicalPreview || canon.ThumbnailURL != "/p/thumb/canonical-video" {
		t.Fatalf("canonical changed: preview=%q thumb=%q", canon.PreviewLocal, canon.ThumbnailURL)
	}
}

type serverFakeTeaserGenerator struct {
	mu     sync.Mutex
	events []string
}

func (g *serverFakeTeaserGenerator) record(event string) {
	g.mu.Lock()
	g.events = append(g.events, event)
	g.mu.Unlock()
}

func (g *serverFakeTeaserGenerator) Events() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.events...)
}

func (g *serverFakeTeaserGenerator) Probe(context.Context, *drives.StreamLink) (float64, error) {
	return 30, nil
}

func (g *serverFakeTeaserGenerator) Generate(context.Context, *drives.StreamLink, float64) (string, error) {
	g.record("preview")
	return "/tmp/source-teaser.mp4", nil
}

func (g *serverFakeTeaserGenerator) MoveToLocal(_ string, videoID string) (string, error) {
	g.mu.Lock()
	if len(g.events) > 0 && g.events[len(g.events)-1] == "preview" {
		g.events[len(g.events)-1] = "preview:" + videoID
	}
	g.mu.Unlock()
	return "/tmp/" + videoID + ".mp4", nil
}

func (g *serverFakeTeaserGenerator) GenerateThumbnail(_ context.Context, _ *drives.StreamLink, videoID string, _ float64) (string, error) {
	g.record("thumb:" + videoID)
	return "/tmp/" + videoID + ".jpg", nil
}

type serverBlockingThumbGenerator struct {
	serverFakeTeaserGenerator
	started chan string
	release chan struct{}
}

func (g *serverBlockingThumbGenerator) GenerateThumbnail(ctx context.Context, _ *drives.StreamLink, videoID string, _ float64) (string, error) {
	g.record("thumb:" + videoID)
	if g.started != nil {
		select {
		case g.started <- videoID:
		default:
		}
	}
	select {
	case <-g.release:
		return "/tmp/" + videoID + ".jpg", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

type serverFakeDrive struct{}

func (d *serverFakeDrive) Kind() string { return "fake" }
func (d *serverFakeDrive) ID() string   { return "drive-id" }
func (d *serverFakeDrive) Init(context.Context) error {
	return nil
}
func (d *serverFakeDrive) List(context.Context, string) ([]drives.Entry, error) {
	return nil, nil
}
func (d *serverFakeDrive) Stat(context.Context, string) (*drives.Entry, error) {
	return nil, drives.ErrNotSupported
}
func (d *serverFakeDrive) StreamURL(context.Context, string) (*drives.StreamLink, error) {
	return &drives.StreamLink{URL: "https://video.example/clip.mp4"}, nil
}
func (d *serverFakeDrive) Upload(context.Context, string, string, io.Reader, int64) (string, error) {
	return "", drives.ErrNotSupported
}
func (d *serverFakeDrive) EnsureDir(context.Context, string) (string, error) {
	return "", drives.ErrNotSupported
}
func (d *serverFakeDrive) RootID() string { return "root" }

type serverFingerprintFakeDrive struct {
	serverFakeDrive
	path string
}

func (d *serverFingerprintFakeDrive) StreamURL(context.Context, string) (*drives.StreamLink, error) {
	return &drives.StreamLink{URL: d.path}, nil
}

type serverScanFingerprintFakeDrive struct {
	serverFingerprintFakeDrive
	entries []drives.Entry
}

func (d *serverScanFingerprintFakeDrive) List(context.Context, string) ([]drives.Entry, error) {
	return d.entries, nil
}

type serverLocalUploadFakeDrive struct {
	serverFakeDrive
}

func (d *serverLocalUploadFakeDrive) ID() string { return "local-upload" }

// seedDriveWithTeaser 在 catalog 里 upsert 一个测试用的 drive 行，把 TeaserEnabled
// 设为 enabled。teaser 入队判断现在按 per-drive 而不是全局 setting，所以涉及到
// teaser worker 的测试都要先把 drive 行写进 catalog。
func seedDriveWithTeaser(t *testing.T, cat *catalog.Catalog, driveID string, enabled bool) {
	t.Helper()
	if err := cat.UpsertDrive(context.Background(), &catalog.Drive{
		ID:            driveID,
		Kind:          "fake",
		Name:          driveID,
		RootID:        "0",
		TeaserEnabled: enabled,
	}); err != nil {
		t.Fatalf("seed drive: %v", err)
	}
}
