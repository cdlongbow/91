package main

import (
	"context"
	"io"
	"testing"

	"github.com/video-site/backend/internal/catalog"
	"github.com/video-site/backend/internal/drives"
	"github.com/video-site/backend/internal/proxy"
)

func TestSpider91IntCredFallbacks(t *testing.T) {
	tests := []struct {
		name string
		d    *catalog.Drive
		key  string
		def  int
		want int
	}{
		{"nil drive", nil, "page", 1, 1},
		{"nil creds", &catalog.Drive{}, "page", 7, 7},
		{"empty value", &catalog.Drive{Credentials: map[string]string{"page": ""}}, "page", 5, 5},
		{"non-numeric", &catalog.Drive{Credentials: map[string]string{"page": "abc"}}, "page", 9, 9},
		{"happy", &catalog.Drive{Credentials: map[string]string{"page": "42"}}, "page", 1, 42},
		{"missing key", &catalog.Drive{Credentials: map[string]string{"a": "1"}}, "b", 99, 99},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := spider91IntCred(tc.d, tc.key, tc.def)
			if got != tc.want {
				t.Fatalf("spider91IntCred(%s) = %d, want %d", tc.name, got, tc.want)
			}
		})
	}
}

func TestSpider91UploadDriveIDDoesNotAutoSelectTarget(t *testing.T) {
	reg := proxy.NewRegistry()
	reg.Set("p115-one", &spider91UploadTargetFakeDrive{id: "p115-one", kind: "p115"})
	reg.Set("p123-one", &spider91UploadTargetFakeDrive{id: "p123-one", kind: "p123"})
	reg.Set("onedrive-one", &spider91UploadTargetFakeDrive{id: "onedrive-one", kind: "onedrive"})

	app := &App{registry: reg}
	if got := app.Spider91UploadDriveID(); got != "" {
		t.Fatalf("empty upload target selected %q, want local-only empty target", got)
	}

	app.spider91UploadDriveID = "p115-one"
	if got := app.Spider91UploadDriveID(); got != "p115-one" {
		t.Fatalf("explicit upload target = %q, want p115-one", got)
	}

	app.spider91UploadDriveID = "p123-one"
	if got := app.Spider91UploadDriveID(); got != "p123-one" {
		t.Fatalf("explicit p123 upload target = %q, want p123-one", got)
	}

	app.spider91UploadDriveID = "onedrive-one"
	if got := app.Spider91UploadDriveID(); got != "onedrive-one" {
		t.Fatalf("explicit onedrive upload target = %q, want onedrive-one", got)
	}

	app.spider91UploadDriveID = "missing"
	if got := app.Spider91UploadDriveID(); got != "" {
		t.Fatalf("missing upload target = %q, want empty", got)
	}
}

type spider91UploadTargetFakeDrive struct {
	id   string
	kind string
}

func (d *spider91UploadTargetFakeDrive) Kind() string { return d.kind }
func (d *spider91UploadTargetFakeDrive) ID() string   { return d.id }
func (d *spider91UploadTargetFakeDrive) Init(context.Context) error {
	return nil
}
func (d *spider91UploadTargetFakeDrive) List(context.Context, string) ([]drives.Entry, error) {
	return nil, nil
}
func (d *spider91UploadTargetFakeDrive) Stat(context.Context, string) (*drives.Entry, error) {
	return nil, drives.ErrNotSupported
}
func (d *spider91UploadTargetFakeDrive) StreamURL(context.Context, string) (*drives.StreamLink, error) {
	return nil, drives.ErrNotSupported
}
func (d *spider91UploadTargetFakeDrive) Upload(context.Context, string, string, io.Reader, int64) (string, error) {
	return "", drives.ErrNotSupported
}
func (d *spider91UploadTargetFakeDrive) EnsureDir(context.Context, string) (string, error) {
	return "", drives.ErrNotSupported
}
func (d *spider91UploadTargetFakeDrive) RootID() string { return "root" }
