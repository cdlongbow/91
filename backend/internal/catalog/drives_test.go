package catalog

import (
	"context"
	"testing"
)

func TestUpsertDriveUsesRootIDAsScanRootID(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	if err := cat.UpsertDrive(ctx, &Drive{
		ID:         "drive",
		Kind:       "p115",
		Name:       "115",
		RootID:     "root-folder",
		ScanRootID: "ignored-scan-root",
	}); err != nil {
		t.Fatalf("upsert drive: %v", err)
	}

	got, err := cat.GetDrive(ctx, "drive")
	if err != nil {
		t.Fatalf("get drive: %v", err)
	}
	if got.RootID != "root-folder" {
		t.Fatalf("rootId = %q, want root-folder", got.RootID)
	}
	if got.ScanRootID != "root-folder" {
		t.Fatalf("scanRootId = %q, want root-folder", got.ScanRootID)
	}
}

func TestUpsertDriveDefaultsRootIDByKind(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	cases := []struct {
		id   string
		kind string
		want string
	}{
		{id: "p115", kind: "p115", want: "0"},
		{id: "pikpak", kind: "pikpak", want: ""},
		{id: "onedrive", kind: "onedrive", want: "root"},
		{id: "googledrive", kind: "googledrive", want: "root"},
		{id: "localstorage", kind: "localstorage", want: "/"},
	}
	for _, tc := range cases {
		if err := cat.UpsertDrive(ctx, &Drive{
			ID:   tc.id,
			Kind: tc.kind,
			Name: tc.kind,
		}); err != nil {
			t.Fatalf("upsert %s: %v", tc.kind, err)
		}
		got, err := cat.GetDrive(ctx, tc.id)
		if err != nil {
			t.Fatalf("get %s: %v", tc.kind, err)
		}
		if got.RootID != tc.want {
			t.Fatalf("%s rootId = %q, want %q", tc.kind, got.RootID, tc.want)
		}
		if got.ScanRootID != tc.want {
			t.Fatalf("%s scanRootId = %q, want %q", tc.kind, got.ScanRootID, tc.want)
		}
	}
}
