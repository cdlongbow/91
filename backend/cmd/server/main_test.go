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
	"github.com/video-site/backend/internal/preview"
)

func TestRegisterPreviewWorkerBackfillsPendingWhenPreviewEnabled(t *testing.T) {
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
		cat:            cat,
		workers:        make(map[string]*preview.Worker),
		thumbWorkers:   make(map[string]*preview.ThumbWorker),
		previewEnabled: true,
	}
	worker := preview.NewWorker(&serverFakeTeaserGenerator{}, cat, &serverFakeDrive{}, "")
	go worker.Run(ctx)

	app.registerPreviewWorkers(ctx, "drive-id", worker, nil, func() {})

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

func TestRegisterPreviewWorkersGenerateThumbnailsBeforePreviews(t *testing.T) {
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

	now := time.Now()
	for _, v := range []*catalog.Video{
		{ID: "video-1", DriveID: "drive-id", FileID: "file-1", Title: "Clip 1", PreviewStatus: "pending"},
		{ID: "video-2", DriveID: "drive-id", FileID: "file-2", Title: "Clip 2", PreviewStatus: "pending"},
	} {
		v.PublishedAt = now
		v.CreatedAt = now
		v.UpdatedAt = now
		if err := cat.UpsertVideo(ctx, v); err != nil {
			t.Fatalf("seed video %s: %v", v.ID, err)
		}
	}

	app := &App{
		cat:            cat,
		workers:        make(map[string]*preview.Worker),
		thumbWorkers:   make(map[string]*preview.ThumbWorker),
		previewEnabled: true,
	}
	gen := &serverFakeTeaserGenerator{}
	drv := &serverFakeDrive{}
	worker := preview.NewWorker(gen, cat, drv, "")
	thumbWorker := preview.NewThumbWorker(gen, cat, drv)
	go worker.Run(ctx)
	go thumbWorker.Run(ctx)

	app.registerPreviewWorkers(ctx, "drive-id", worker, thumbWorker, func() {})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		first, err := cat.GetVideo(ctx, "video-1")
		if err != nil {
			t.Fatalf("get video-1: %v", err)
		}
		second, err := cat.GetVideo(ctx, "video-2")
		if err != nil {
			t.Fatalf("get video-2: %v", err)
		}
		if first.ThumbnailURL != "" && second.ThumbnailURL != "" &&
			first.PreviewStatus == "ready" && second.PreviewStatus == "ready" {
			events := gen.Events()
			if len(events) != 4 {
				t.Fatalf("events = %#v, want 4 generation events", events)
			}
			for i, event := range events[:2] {
				if event[:6] != "thumb:" {
					t.Fatalf("event %d = %q, want thumbnail before previews; all events=%#v", i, event, events)
				}
			}
			for i, event := range events[2:] {
				if event[:8] != "preview:" {
					t.Fatalf("event %d = %q, want previews after thumbnails; all events=%#v", i+2, event, events)
				}
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("generation did not finish, events=%#v", gen.Events())
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
		cat:            cat,
		workers:        make(map[string]*preview.Worker),
		thumbWorkers:   make(map[string]*preview.ThumbWorker),
		previewEnabled: true,
	}
	worker := preview.NewWorker(&serverFakeTeaserGenerator{}, cat, &serverFakeDrive{}, "")
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

func TestEnqueueUploadedVideoQueuesLocalPreviewWorker(t *testing.T) {
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
		cat:            cat,
		workers:        make(map[string]*preview.Worker),
		thumbWorkers:   make(map[string]*preview.ThumbWorker),
		previewEnabled: true,
	}
	worker := preview.NewWorker(&serverFakeTeaserGenerator{}, cat, &serverLocalUploadFakeDrive{}, "")
	go worker.Run(ctx)
	app.mu.Lock()
	app.workers["local-upload"] = worker
	app.mu.Unlock()

	app.enqueueUploadedVideo(ctx, video)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, err := cat.GetVideo(ctx, video.ID)
		if err != nil {
			t.Fatalf("get video: %v", err)
		}
		if got.PreviewStatus == "ready" {
			if got.PreviewLocal != "/tmp/local-upload-video.mp4" {
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

func TestScheduledScanWindowAllowsOnlyEarlyMorning(t *testing.T) {
	loc := time.FixedZone("CST", 8*60*60)
	cases := []struct {
		name string
		now  time.Time
		want bool
	}{
		{name: "before window", now: time.Date(2026, 5, 12, 1, 59, 0, 0, loc), want: false},
		{name: "at start", now: time.Date(2026, 5, 12, 2, 0, 0, 0, loc), want: true},
		{name: "inside window", now: time.Date(2026, 5, 12, 6, 59, 0, 0, loc), want: true},
		{name: "at end", now: time.Date(2026, 5, 12, 7, 0, 0, 0, loc), want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := scheduledScanAllowed(tc.now); got != tc.want {
				t.Fatalf("scheduledScanAllowed(%s) = %v, want %v", tc.now.Format(time.RFC3339), got, tc.want)
			}
		})
	}
}

func TestScheduledScanDueRespectsWindowAndInterval(t *testing.T) {
	loc := time.FixedZone("CST", 8*60*60)
	interval := 2 * time.Hour
	inside := time.Date(2026, 5, 12, 2, 0, 0, 0, loc)

	if scheduledScanDue(time.Date(2026, 5, 12, 1, 59, 0, 0, loc), time.Time{}, interval) {
		t.Fatal("scheduled scan due outside window, want false")
	}
	if !scheduledScanDue(inside, time.Time{}, interval) {
		t.Fatal("first scheduled scan inside window = false, want true")
	}
	if scheduledScanDue(inside.Add(time.Hour), inside, interval) {
		t.Fatal("scheduled scan due before interval elapsed, want false")
	}
	if !scheduledScanDue(inside.Add(2*time.Hour), inside, interval) {
		t.Fatal("scheduled scan due after interval elapsed, want true")
	}
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
	obsoleteTranscode := filepath.Join(localDir, "transcodes", "pikpak-PikPak-obsolete.mp4")
	obsoleteTranscodeTmp := filepath.Join(localDir, "transcodes", "pikpak-PikPak-obsolete.tmp.mp4")
	keptPreview := filepath.Join(localDir, "kept.mp4")
	for _, path := range []string{obsoletePreview, obsoleteThumb, obsoleteTranscode, obsoleteTranscodeTmp, keptPreview} {
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
	for _, path := range []string{obsoletePreview, obsoleteThumb, obsoleteTranscode, obsoleteTranscodeTmp} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("obsolete asset %s still exists, stat err=%v", path, err)
		}
	}
	if _, err := os.Stat(keptPreview); err != nil {
		t.Fatalf("kept preview missing: %v", err)
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

type serverLocalUploadFakeDrive struct {
	serverFakeDrive
}

func (d *serverLocalUploadFakeDrive) ID() string { return "local-upload" }
