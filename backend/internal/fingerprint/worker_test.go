package fingerprint

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/video-site/backend/internal/catalog"
	"github.com/video-site/backend/internal/drives"
)

func TestComputeLocalFilesWithSameContentMatch(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	body := []byte("same video bytes")
	a := filepath.Join(dir, "a.mp4")
	b := filepath.Join(dir, "b.mp4")
	if err := os.WriteFile(a, body, 0o644); err != nil {
		t.Fatalf("write a: %v", err)
	}
	if err := os.WriteFile(b, body, 0o644); err != nil {
		t.Fatalf("write b: %v", err)
	}

	sumA, err := Compute(ctx, &fakeDrive{paths: map[string]string{"a": a}}, &catalog.Video{ID: "a", FileID: "a", Size: int64(len(body))}, Config{}, nil)
	if err != nil {
		t.Fatalf("compute a: %v", err)
	}
	sumB, err := Compute(ctx, &fakeDrive{paths: map[string]string{"b": b}}, &catalog.Video{ID: "b", FileID: "b", Size: int64(len(body))}, Config{}, nil)
	if err != nil {
		t.Fatalf("compute b: %v", err)
	}
	if sumA == "" || sumA != sumB {
		t.Fatalf("fingerprints = %q / %q, want same non-empty", sumA, sumB)
	}
}

func TestComputeRemoteUsesRangeSamples(t *testing.T) {
	ctx := context.Background()
	data := make([]byte, 10*1024*1024)
	for i := range data {
		data[i] = byte(i % 251)
	}
	var ranges []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawRange := r.Header.Get("Range")
		ranges = append(ranges, rawRange)
		var start, end int
		if _, err := fmt.Sscanf(rawRange, "bytes=%d-%d", &start, &end); err != nil {
			t.Fatalf("bad range %q: %v", rawRange, err)
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[start : end+1])
	}))
	defer srv.Close()

	drv := &fakeDrive{paths: map[string]string{"remote": srv.URL + "/video.mp4"}}
	sum, err := Compute(ctx, drv, &catalog.Video{ID: "remote", FileID: "remote", Size: int64(len(data))}, Config{
		SampleSizeBytes: 4,
		FullHashMaxSize: 8,
		HTTPClient:      srv.Client(),
	}, srv.Client())
	if err != nil {
		t.Fatalf("compute remote: %v", err)
	}
	if sum == "" {
		t.Fatal("fingerprint should not be empty")
	}
	want := []string{
		"bytes=0-3",
		"bytes=2097151-2097154",
		"bytes=4194302-4194305",
		"bytes=6291453-6291456",
		"bytes=8388604-8388607",
	}
	if fmt.Sprint(ranges) != fmt.Sprint(want) {
		t.Fatalf("ranges = %#v, want %#v", ranges, want)
	}
}

type fakeDrive struct {
	paths map[string]string
}

func (d *fakeDrive) Kind() string { return "fake" }
func (d *fakeDrive) ID() string   { return "fake" }
func (d *fakeDrive) Init(context.Context) error {
	return nil
}
func (d *fakeDrive) List(context.Context, string) ([]drives.Entry, error) {
	return nil, drives.ErrNotSupported
}
func (d *fakeDrive) Stat(context.Context, string) (*drives.Entry, error) {
	return nil, drives.ErrNotSupported
}
func (d *fakeDrive) StreamURL(_ context.Context, fileID string) (*drives.StreamLink, error) {
	return &drives.StreamLink{URL: d.paths[fileID], Expires: time.Now().Add(time.Minute)}, nil
}
func (d *fakeDrive) Upload(context.Context, string, string, io.Reader, int64) (string, error) {
	return "", drives.ErrNotSupported
}
func (d *fakeDrive) EnsureDir(context.Context, string) (string, error) {
	return "", drives.ErrNotSupported
}
func (d *fakeDrive) RootID() string { return "root" }
