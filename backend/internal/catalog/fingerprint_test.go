package catalog

import (
	"context"
	"testing"
	"time"
)

func TestListVideosDeduplicatesBySampledSHA256(t *testing.T) {
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

	now := time.Now()
	for _, v := range []*Video{
		{
			ID:          "drive-a-file-a",
			DriveID:     "drive-a",
			FileID:      "file-a",
			FileName:    "first-name.mp4",
			Title:       "First",
			Size:        1234,
			PublishedAt: now.Add(-time.Minute),
			CreatedAt:   now.Add(-time.Minute),
			UpdatedAt:   now.Add(-time.Minute),
		},
		{
			ID:          "drive-b-file-b",
			DriveID:     "drive-b",
			FileID:      "file-b",
			FileName:    "second-name.mp4",
			Title:       "Second",
			Size:        1234,
			PublishedAt: now,
			CreatedAt:   now,
			UpdatedAt:   now,
		},
	} {
		if err := cat.UpsertVideo(ctx, v); err != nil {
			t.Fatalf("upsert %s: %v", v.ID, err)
		}
	}

	items, total, err := cat.ListVideos(ctx, ListParams{Page: 1, PageSize: 10})
	if err != nil {
		t.Fatalf("list before fingerprint: %v", err)
	}
	if total != 2 || len(items) != 2 {
		t.Fatalf("before fingerprint total=%d len=%d, want 2", total, len(items))
	}

	const sampled = "abc123"
	if err := cat.UpdateVideoFingerprint(ctx, "drive-a-file-a", sampled, "ready", ""); err != nil {
		t.Fatalf("update a fingerprint: %v", err)
	}
	if err := cat.UpdateVideoFingerprint(ctx, "drive-b-file-b", sampled, "ready", ""); err != nil {
		t.Fatalf("update b fingerprint: %v", err)
	}

	items, total, err = cat.ListVideos(ctx, ListParams{Page: 1, PageSize: 10})
	if err != nil {
		t.Fatalf("list after fingerprint: %v", err)
	}
	if total != 1 || len(items) != 1 {
		t.Fatalf("after fingerprint total=%d len=%d, want 1", total, len(items))
	}
	if items[0].ID != "drive-a-file-a" {
		t.Fatalf("canonical id = %q, want earliest created video", items[0].ID)
	}
}
