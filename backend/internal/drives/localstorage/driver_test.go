package localstorage

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/video-site/backend/internal/catalog"
	"github.com/video-site/backend/internal/scanner"
)

func TestListEncodesRelativePathsAndStreamURLResolvesFile(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "clips")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	videoPath := filepath.Join(sub, "sample.mp4")
	if err := os.WriteFile(videoPath, []byte("video"), 0o644); err != nil {
		t.Fatalf("write video: %v", err)
	}

	drv := New(Config{ID: "local", RootPath: root})
	if err := drv.Init(context.Background()); err != nil {
		t.Fatalf("init: %v", err)
	}
	rootEntries, err := drv.List(context.Background(), drv.RootID())
	if err != nil {
		t.Fatalf("list root: %v", err)
	}
	if len(rootEntries) != 1 || !rootEntries[0].IsDir {
		t.Fatalf("root entries = %#v, want one directory", rootEntries)
	}
	if strings.Contains(rootEntries[0].ID, "/") {
		t.Fatalf("encoded dir id contains slash: %q", rootEntries[0].ID)
	}

	fileEntries, err := drv.List(context.Background(), rootEntries[0].ID)
	if err != nil {
		t.Fatalf("list subdir: %v", err)
	}
	if len(fileEntries) != 1 || fileEntries[0].Name != "sample.mp4" {
		t.Fatalf("file entries = %#v, want sample.mp4", fileEntries)
	}
	if strings.Contains(fileEntries[0].ID, "/") {
		t.Fatalf("encoded file id contains slash: %q", fileEntries[0].ID)
	}

	link, err := drv.StreamURL(context.Background(), fileEntries[0].ID)
	if err != nil {
		t.Fatalf("stream url: %v", err)
	}
	if link.URL != videoPath {
		t.Fatalf("url = %q, want %q", link.URL, videoPath)
	}
}

func TestStreamURLRejectsEscapingID(t *testing.T) {
	drv := New(Config{ID: "local", RootPath: t.TempDir()})
	escaped := base64.RawURLEncoding.EncodeToString([]byte("../secret.mp4"))

	_, err := drv.StreamURL(context.Background(), escaped)

	if err == nil || !strings.Contains(err.Error(), "invalid relative path") {
		t.Fatalf("error = %v, want invalid relative path", err)
	}
}

func TestInitRequiresExistingDirectory(t *testing.T) {
	drv := New(Config{ID: "local", RootPath: filepath.Join(t.TempDir(), "missing")})

	err := drv.Init(context.Background())

	if err == nil || !strings.Contains(err.Error(), "stat root") {
		t.Fatalf("error = %v, want stat root failure", err)
	}
}

func TestScannerPersistsLocalStorageVideo(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "collection"), 0o755); err != nil {
		t.Fatalf("mkdir collection: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "collection", "clip.mp4"), []byte("video"), 0o644); err != nil {
		t.Fatalf("write video: %v", err)
	}
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	drv := New(Config{ID: "local", RootPath: root})
	sc := scanner.New(cat, drv, []string{".mp4"}, nil, nil)
	stats, err := sc.Run(ctx, drv.RootID())
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if stats.Added != 1 {
		t.Fatalf("added = %d, want 1", stats.Added)
	}

	fileID := encodeRel("collection/clip.mp4")
	got, err := cat.GetVideo(ctx, Kind+"-local-"+fileID)
	if err != nil {
		t.Fatalf("get video: %v", err)
	}
	if got.DriveID != "local" || got.FileID != fileID || got.Category != "collection" {
		t.Fatalf("video = %#v, want local drive video in collection", got)
	}
}
