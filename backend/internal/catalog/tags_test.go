package catalog

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

func TestListVideosNeedingThumbnailIncludesExistingThumbnailMissingDuration(t *testing.T) {
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
	videos := []*Video{
		{
			ID:           "duration-only",
			DriveID:      "drive",
			FileID:       "file-duration-only",
			Title:        "Duration Only",
			ThumbnailURL: "/p/thumb/duration-only",
			PublishedAt:  now,
			CreatedAt:    now,
			UpdatedAt:    now,
		},
		{
			ID:              "complete",
			DriveID:         "drive",
			FileID:          "file-complete",
			Title:           "Complete",
			DurationSeconds: 12,
			ThumbnailURL:    "/p/thumb/complete",
			PublishedAt:     now.Add(time.Second),
			CreatedAt:       now.Add(time.Second),
			UpdatedAt:       now.Add(time.Second),
		},
		{
			ID:              "missing-thumb",
			DriveID:         "drive",
			FileID:          "file-missing-thumb",
			Title:           "Missing Thumb",
			DurationSeconds: 18,
			PublishedAt:     now.Add(2 * time.Second),
			CreatedAt:       now.Add(2 * time.Second),
			UpdatedAt:       now.Add(2 * time.Second),
		},
		{
			ID:          "failed",
			DriveID:     "drive",
			FileID:      "file-failed",
			Title:       "Failed",
			PublishedAt: now.Add(3 * time.Second),
			CreatedAt:   now.Add(3 * time.Second),
			UpdatedAt:   now.Add(3 * time.Second),
		},
	}
	for _, v := range videos {
		if err := cat.UpsertVideo(ctx, v); err != nil {
			t.Fatalf("seed %s: %v", v.ID, err)
		}
	}
	if err := cat.UpdateVideoMeta(ctx, "failed", VideoMetaPatch{ThumbnailStatus: "failed"}); err != nil {
		t.Fatalf("mark failed thumbnail: %v", err)
	}

	items, err := cat.ListVideosNeedingThumbnail(ctx, "drive", 0)
	if err != nil {
		t.Fatalf("list videos needing thumbnail: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %#v, want duration-only and missing-thumb", items)
	}
	if items[0].ID != "duration-only" || items[1].ID != "missing-thumb" {
		t.Fatalf("item ids = %q, %q; want duration-only, missing-thumb", items[0].ID, items[1].ID)
	}

	count, err := cat.CountVideosNeedingThumbnail(ctx, "drive")
	if err != nil {
		t.Fatalf("count videos needing thumbnail: %v", err)
	}
	if count != 2 {
		t.Fatalf("count = %d, want 2", count)
	}

	counts, err := cat.CountThumbnailsByDrive(ctx)
	if err != nil {
		t.Fatalf("count thumbnails by drive: %v", err)
	}
	if got := counts["drive"]; got.Ready != 2 || got.Pending != 1 || got.Failed != 1 || got.DurationPending != 1 {
		t.Fatalf("thumbnail counts = %#v, want ready=2 pending=1 failed=1 durationPending=1", got)
	}

	if err := cat.UpdateVideoMeta(ctx, "duration-only", VideoMetaPatch{ThumbnailStatus: "skipped"}); err != nil {
		t.Fatalf("mark duration-only skipped: %v", err)
	}
	count, err = cat.CountVideosNeedingThumbnail(ctx, "drive")
	if err != nil {
		t.Fatalf("count videos needing thumbnail after skip: %v", err)
	}
	if count != 1 {
		t.Fatalf("count after skip = %d, want 1", count)
	}
	counts, err = cat.CountThumbnailsByDrive(ctx)
	if err != nil {
		t.Fatalf("count thumbnails by drive after skip: %v", err)
	}
	if got := counts["drive"]; got.Ready != 2 || got.Pending != 1 || got.Failed != 1 || got.DurationPending != 0 {
		t.Fatalf("thumbnail counts after skip = %#v, want ready=2 pending=1 failed=1 durationPending=0", got)
	}
}

func TestCreateTagAndClassifyAddsTagToMatchingExistingVideos(t *testing.T) {
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
	if err := cat.UpsertVideo(ctx, &Video{
		ID:          "video-1",
		DriveID:     "drive",
		FileID:      "file-1",
		Title:       "清纯短发合集",
		Category:    "普通目录",
		PublishedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("seed matching video: %v", err)
	}
	if err := cat.UpsertVideo(ctx, &Video{
		ID:          "video-2",
		DriveID:     "drive",
		FileID:      "file-2",
		Title:       "普通标题",
		Category:    "普通目录",
		PublishedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("seed non-matching video: %v", err)
	}

	classified, err := cat.CreateTagAndClassify(ctx, "清纯", nil, "user")
	if err != nil {
		t.Fatalf("create tag: %v", err)
	}
	if classified != 1 {
		t.Fatalf("classified = %d, want 1", classified)
	}

	got, err := cat.GetVideo(ctx, "video-1")
	if err != nil {
		t.Fatalf("get matching video: %v", err)
	}
	if !sameStrings(got.Tags, []string{"清纯"}) {
		t.Fatalf("matching tags = %#v, want 清纯", got.Tags)
	}

	other, err := cat.GetVideo(ctx, "video-2")
	if err != nil {
		t.Fatalf("get non-matching video: %v", err)
	}
	if len(other.Tags) != 0 {
		t.Fatalf("non-matching tags = %#v, want none", other.Tags)
	}
}

func TestDeleteTagRemovesTagFromVideos(t *testing.T) {
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
	if err := cat.UpsertVideo(ctx, &Video{
		ID:          "video-1",
		DriveID:     "drive",
		FileID:      "file-1",
		Title:       "清纯短发",
		PublishedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("seed video: %v", err)
	}
	if _, err := cat.CreateTagAndClassify(ctx, "清纯", nil, "user"); err != nil {
		t.Fatalf("create tag: %v", err)
	}

	tag := mustTagByLabel(t, ctx, cat, "清纯")
	removed, err := cat.DeleteTag(ctx, tag.ID)
	if err != nil {
		t.Fatalf("delete tag: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}

	got, err := cat.GetVideo(ctx, "video-1")
	if err != nil {
		t.Fatalf("get video: %v", err)
	}
	if len(got.Tags) != 0 {
		t.Fatalf("video tags = %#v, want none", got.Tags)
	}
	for _, tag := range mustListTags(t, ctx, cat) {
		if tag.Label == "清纯" {
			t.Fatal("deleted tag still appears in ListTags")
		}
	}
}

func TestDeleteTagSuppressesAutomaticCollectionRecreation(t *testing.T) {
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
	for _, id := range []string{"video-1", "video-2"} {
		if err := cat.UpsertVideo(ctx, &Video{
			ID:          id,
			DriveID:     "drive",
			FileID:      id,
			Title:       "合集视频",
			Category:    "sunny",
			PublishedAt: now,
			CreatedAt:   now,
			UpdatedAt:   now,
		}); err != nil {
			t.Fatalf("seed video %s: %v", id, err)
		}
	}

	if label, ok, err := cat.EnsureCollectionTag(ctx, "sunny"); err != nil || !ok || label != "sunny" {
		t.Fatalf("ensure collection = %q, %v, %v; want sunny true nil", label, ok, err)
	}
	tag := mustTagByLabel(t, ctx, cat, "sunny")
	if _, err := cat.DeleteTag(ctx, tag.ID); err != nil {
		t.Fatalf("delete tag: %v", err)
	}

	if label, ok, err := cat.EnsureCollectionTag(ctx, "sunny"); err != nil || ok || label != "" {
		t.Fatalf("ensure deleted collection = %q, %v, %v; want empty false nil", label, ok, err)
	}
	for _, tag := range mustListTags(t, ctx, cat) {
		if tag.Label == "sunny" {
			t.Fatal("deleted collection tag was recreated automatically")
		}
	}
}

func TestCreateTagAndClassifyRestoresDeletedTag(t *testing.T) {
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
	if err := cat.UpsertVideo(ctx, &Video{
		ID:          "video-1",
		DriveID:     "drive",
		FileID:      "file-1",
		Title:       "清纯短发",
		PublishedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("seed video: %v", err)
	}
	if _, err := cat.CreateTagAndClassify(ctx, "清纯", nil, "user"); err != nil {
		t.Fatalf("create tag: %v", err)
	}
	tag := mustTagByLabel(t, ctx, cat, "清纯")
	if _, err := cat.DeleteTag(ctx, tag.ID); err != nil {
		t.Fatalf("delete tag: %v", err)
	}

	classified, err := cat.CreateTagAndClassify(ctx, "清纯", nil, "user")
	if err != nil {
		t.Fatalf("recreate tag: %v", err)
	}
	if classified != 1 {
		t.Fatalf("classified = %d, want 1", classified)
	}
	got, err := cat.GetVideo(ctx, "video-1")
	if err != nil {
		t.Fatalf("get video: %v", err)
	}
	if !sameStrings(got.Tags, []string{"清纯"}) {
		t.Fatalf("video tags = %#v, want 清纯", got.Tags)
	}
}

func TestDeleteTagRejectsSystemTags(t *testing.T) {
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

	tag := mustTagByLabel(t, ctx, cat, "AV")
	if _, err := cat.DeleteTag(ctx, tag.ID); !errors.Is(err, ErrSystemTag) {
		t.Fatalf("delete system tag err = %v, want ErrSystemTag", err)
	}

	if tag := mustTagByLabel(t, ctx, cat, "AV"); tag.Source != "system" {
		t.Fatalf("AV source = %q, want system", tag.Source)
	}
}

func TestOpenClassifiesSystemTagsForExistingVideos(t *testing.T) {
	path := t.TempDir() + "/catalog.db"
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	now := time.Now().UnixMilli()
	if _, err := db.Exec(`
INSERT INTO videos (id, drive_id, file_id, title, tags, tags_manual, published_at, created_at, updated_at)
VALUES
	('video-auto', 'drive', 'file-auto', '巨乳后入合集', '[]', 0, ?, ?, ?),
	('video-manual', 'drive', 'file-manual', '巨乳后入合集', '[]', 1, ?, ?, ?)`,
		now, now, now, now, now, now); err != nil {
		t.Fatalf("seed videos: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	ctx := context.Background()
	cat, err := Open(path)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	got, err := cat.GetVideo(ctx, "video-auto")
	if err != nil {
		t.Fatalf("get auto video: %v", err)
	}
	if !sameStrings(got.Tags, []string{"后入", "奶子"}) {
		t.Fatalf("auto tags = %#v, want 后入/奶子", got.Tags)
	}

	manual, err := cat.GetVideo(ctx, "video-manual")
	if err != nil {
		t.Fatalf("get manual video: %v", err)
	}
	if len(manual.Tags) != 0 {
		t.Fatalf("manual tags = %#v, want unchanged", manual.Tags)
	}
}

func TestMigrateDoesNotRewriteAlreadySyncedVideoTags(t *testing.T) {
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
	for _, id := range []string{"video-1", "video-2", "video-3"} {
		if err := cat.UpsertVideo(ctx, &Video{
			ID:          id,
			DriveID:     "drive",
			FileID:      id,
			Title:       "巨乳后入合集",
			Category:    "Better Call Saul S03",
			PublishedAt: now,
			CreatedAt:   now,
			UpdatedAt:   now,
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	if err := cat.migrate(ctx); err != nil {
		t.Fatalf("first migrate: %v", err)
	}

	before := videoUpdatedAtByID(t, ctx, cat, "video-1", "video-2", "video-3")
	time.Sleep(5 * time.Millisecond)
	if err := cat.migrate(ctx); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	after := videoUpdatedAtByID(t, ctx, cat, "video-1", "video-2", "video-3")
	for id, want := range before {
		if got := after[id]; got != want {
			t.Fatalf("%s updated_at changed on no-op migrate: got %d, want %d", id, got, want)
		}
	}
}

func TestMigrateBackfillsLegacyTagsWithoutRelations(t *testing.T) {
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

	now := time.Now().UnixMilli()
	if _, err := cat.db.ExecContext(ctx, `
INSERT INTO videos (id, drive_id, file_id, title, tags, tags_manual, published_at, created_at, updated_at)
VALUES ('legacy-video', 'drive', 'file-legacy', 'legacy title', '["legacy-tag"]', 0, ?, ?, ?)`,
		now, now, now); err != nil {
		t.Fatalf("seed legacy video: %v", err)
	}
	if err := cat.migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	tag := mustTagByLabel(t, ctx, cat, "legacy-tag")
	var count int
	if err := cat.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM video_tags WHERE video_id = 'legacy-video' AND tag_id = ?`, tag.ID).Scan(&count); err != nil {
		t.Fatalf("count video tag: %v", err)
	}
	if count != 1 {
		t.Fatalf("legacy video tag relation count = %d, want 1", count)
	}
}

func TestOpenMigratesLegacyVideosWithoutFileName(t *testing.T) {
	path := t.TempDir() + "/catalog.db"
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	if _, err := db.Exec(`
CREATE TABLE videos (
	id               TEXT PRIMARY KEY,
	drive_id         TEXT NOT NULL,
	file_id          TEXT NOT NULL,
	content_hash     TEXT DEFAULT '',
	parent_id        TEXT,
	title            TEXT NOT NULL,
	author           TEXT,
	tags             TEXT,
	duration_seconds INTEGER DEFAULT 0,
	size_bytes       INTEGER DEFAULT 0,
	ext              TEXT,
	quality          TEXT,
	thumbnail_url    TEXT,
	preview_file_id  TEXT,
	preview_local    TEXT,
	preview_status   TEXT DEFAULT 'pending',
	views            INTEGER DEFAULT 0,
	favorites        INTEGER DEFAULT 0,
	comments         INTEGER DEFAULT 0,
	likes            INTEGER DEFAULT 0,
	dislikes         INTEGER DEFAULT 0,
	category         TEXT,
	hidden           INTEGER DEFAULT 0,
	tags_manual      INTEGER DEFAULT 0,
	badges           TEXT,
	description      TEXT,
	published_at     INTEGER NOT NULL,
	created_at       INTEGER NOT NULL,
	updated_at       INTEGER NOT NULL
)`); err != nil {
		t.Fatalf("create legacy videos table: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	cat, err := Open(path)
	if err != nil {
		t.Fatalf("open migrated catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	var fileNameDefault string
	if err := cat.db.QueryRow(`SELECT COALESCE(file_name, '') FROM videos LIMIT 1`).Scan(&fileNameDefault); err != nil && err != sql.ErrNoRows {
		t.Fatalf("query migrated file_name column: %v", err)
	}
}

func TestSetManualVideoTagsRejectsUnknownLabels(t *testing.T) {
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
	if err := cat.UpsertVideo(ctx, &Video{
		ID:          "video-1",
		DriveID:     "drive",
		FileID:      "file-1",
		Title:       "普通标题",
		PublishedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("seed video: %v", err)
	}

	if err := cat.SetManualVideoTags(ctx, "video-1", []string{"不存在"}); err == nil {
		t.Fatal("SetManualVideoTags accepted an unknown tag label")
	}
}

func TestSetAutoVideoTagsDoesNotOverwriteManualVideoTags(t *testing.T) {
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
	if err := cat.UpsertVideo(ctx, &Video{
		ID:          "video-1",
		DriveID:     "drive",
		FileID:      "file-1",
		Title:       "清纯后入",
		PublishedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("seed video: %v", err)
	}
	if _, err := cat.CreateTagAndClassify(ctx, "清纯", nil, "user"); err != nil {
		t.Fatalf("create user tag: %v", err)
	}
	if err := cat.SetManualVideoTags(ctx, "video-1", []string{"清纯"}); err != nil {
		t.Fatalf("set manual tags: %v", err)
	}

	if err := cat.SetAutoVideoTags(ctx, "video-1", []string{"后入"}); err != nil {
		t.Fatalf("set auto tags: %v", err)
	}

	got, err := cat.GetVideo(ctx, "video-1")
	if err != nil {
		t.Fatalf("get video: %v", err)
	}
	if !sameStrings(got.Tags, []string{"清纯"}) {
		t.Fatalf("tags = %#v, want manual 清纯 only", got.Tags)
	}
}

func TestCreateTagAndClassifyMapsAVCodeLabelToAV(t *testing.T) {
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

	if _, err := cat.CreateTagAndClassify(ctx, "cc-1750027", nil, "user"); err != nil {
		t.Fatalf("create code tag: %v", err)
	}

	tags, err := cat.ListTags(ctx)
	if err != nil {
		t.Fatalf("list tags: %v", err)
	}
	for _, tag := range tags {
		if tag.Label == "cc-1750027" {
			t.Fatal("created standalone AV code tag cc-1750027")
		}
	}
}

func TestLooksLikeCollectionTagRejectsAVCodes(t *testing.T) {
	cases := []string{
		"DASS-499-C",
		"dass-499-c",
		"ADN-778",
		"SONE-247-C",
		"JUQ-502-UC",
		"ABF-032",
		"SSIS-233",
		"MIDA-607",
		"cc-1750027",
		"FC2-PPV-74663555",
		"ADN-778-FHD(1)",
		"ADN-778-中文字幕",
		"[44x.me]idbd-786",
		"NTRH-018_FHD_CH",
		"390JAC-233",
	}
	for _, label := range cases {
		if LooksLikeCollectionTag(label) {
			t.Fatalf("LooksLikeCollectionTag(%q) = true, want false", label)
		}
	}
}

func TestMigrateCollapsesAVCodeTagsIntoAV(t *testing.T) {
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
	for _, seed := range []struct {
		id    string
		label string
	}{
		{id: "video-1", label: "cc-1750027"},
		{id: "video-2", label: "ADN-778-FHD(1)"},
		{id: "video-3", label: "[44x.me]idbd-786"},
		{id: "video-4", label: "390JAC-233"},
	} {
		if err := cat.UpsertVideo(ctx, &Video{
			ID:          seed.id,
			DriveID:     "drive",
			FileID:      seed.id,
			Title:       seed.label + " sample",
			Tags:        []string{seed.label},
			Category:    seed.label,
			PublishedAt: now,
			CreatedAt:   now,
			UpdatedAt:   now,
		}); err != nil {
			t.Fatalf("seed polluted video %s: %v", seed.label, err)
		}
	}

	if err := cat.migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	tags, err := cat.ListTags(ctx)
	if err != nil {
		t.Fatalf("list tags: %v", err)
	}
	var sawAV bool
	polluted := map[string]bool{}
	for _, tag := range tags {
		if tag.Label == "AV" {
			sawAV = true
		}
		if tag.Label != "AV" && isAVCodePollutedLabel(tag.Label) {
			polluted[tag.Label] = true
		}
	}
	if !sawAV {
		t.Fatal("AV tag was not seeded")
	}
	if len(polluted) > 0 {
		t.Fatalf("AV code tags were not removed: %#v", polluted)
	}

	for _, id := range []string{"video-1", "video-2", "video-3", "video-4"} {
		got, err := cat.GetVideo(ctx, id)
		if err != nil {
			t.Fatalf("get video %s: %v", id, err)
		}
		if !sameStrings(got.Tags, []string{"AV"}) {
			t.Fatalf("%s tags = %#v, want AV", id, got.Tags)
		}
	}
}

func TestMigrateClearsVolatileOneDriveThumbnailURLs(t *testing.T) {
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
	if err := cat.UpsertDrive(ctx, &Drive{
		ID:        "onedrive-main",
		Kind:      "onedrive",
		Name:      "OneDrive",
		RootID:    "root",
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed onedrive: %v", err)
	}

	videos := []*Video{
		{
			ID:           "onedrive-video",
			DriveID:      "onedrive-main",
			FileID:       "file-1",
			Title:        "OneDrive",
			ThumbnailURL: "https://westus21-mediap.svc.ms/transform/thumbnail?provider=spo&tempauth=expired",
		},
		{
			ID:           "local-thumb-video",
			DriveID:      "onedrive-main",
			FileID:       "file-2",
			Title:        "Local thumb",
			ThumbnailURL: "/p/thumb/local-thumb-video",
		},
		{
			ID:           "pikpak-video",
			DriveID:      "pikpak-main",
			FileID:       "file-3",
			Title:        "PikPak",
			ThumbnailURL: "https://sg-thumbnail-drive.mypikpak.net/v0/screenshot-thumbnails/demo",
		},
	}
	for _, v := range videos {
		v.PublishedAt = now
		v.CreatedAt = now
		v.UpdatedAt = now
		if err := cat.UpsertVideo(ctx, v); err != nil {
			t.Fatalf("seed video %s: %v", v.ID, err)
		}
	}

	if err := cat.migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	got, err := cat.GetVideo(ctx, "onedrive-video")
	if err != nil {
		t.Fatalf("get onedrive video: %v", err)
	}
	if got.ThumbnailURL != "" {
		t.Fatalf("onedrive thumbnail = %q, want cleared", got.ThumbnailURL)
	}

	local, err := cat.GetVideo(ctx, "local-thumb-video")
	if err != nil {
		t.Fatalf("get local thumb video: %v", err)
	}
	if local.ThumbnailURL != "/p/thumb/local-thumb-video" {
		t.Fatalf("local thumbnail = %q, want preserved", local.ThumbnailURL)
	}

	pikpak, err := cat.GetVideo(ctx, "pikpak-video")
	if err != nil {
		t.Fatalf("get pikpak video: %v", err)
	}
	if pikpak.ThumbnailURL == "" {
		t.Fatal("pikpak thumbnail was cleared")
	}
}

func TestMigrateHidesZeroSizeVideosForKnownDrives(t *testing.T) {
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
	if err := cat.UpsertDrive(ctx, &Drive{
		ID:        "drive-main",
		Kind:      "onedrive",
		Name:      "OneDrive",
		RootID:    "root",
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed drive: %v", err)
	}
	for _, v := range []*Video{
		{ID: "empty-video", DriveID: "drive-main", FileID: "file-1", Title: "Empty", Size: 0},
		{ID: "normal-video", DriveID: "drive-main", FileID: "file-2", Title: "Normal", Size: 123},
		{ID: "orphan-empty-video", DriveID: "unknown-drive", FileID: "file-3", Title: "Orphan", Size: 0},
	} {
		v.PublishedAt = now
		v.CreatedAt = now
		v.UpdatedAt = now
		if err := cat.UpsertVideo(ctx, v); err != nil {
			t.Fatalf("seed video %s: %v", v.ID, err)
		}
	}

	if err := cat.migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	empty, err := cat.GetVideo(ctx, "empty-video")
	if err != nil {
		t.Fatalf("get empty video: %v", err)
	}
	if !empty.Hidden {
		t.Fatal("empty video was not hidden")
	}

	normal, err := cat.GetVideo(ctx, "normal-video")
	if err != nil {
		t.Fatalf("get normal video: %v", err)
	}
	if normal.Hidden {
		t.Fatal("normal video was hidden")
	}

	orphan, err := cat.GetVideo(ctx, "orphan-empty-video")
	if err != nil {
		t.Fatalf("get orphan empty video: %v", err)
	}
	if orphan.Hidden {
		t.Fatal("orphan empty video without a known drive was hidden")
	}
}

func TestListVideosHidesDuplicateContentHashes(t *testing.T) {
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
			ID:          "video-1",
			DriveID:     "drive",
			FileID:      "file-1",
			Title:       "First",
			ContentHash: "hash-same",
			PublishedAt: now,
			CreatedAt:   now,
			UpdatedAt:   now,
		},
		{
			ID:          "video-2",
			DriveID:     "drive",
			FileID:      "file-2",
			Title:       "Second",
			ContentHash: "hash-same",
			PublishedAt: now.Add(time.Second),
			CreatedAt:   now.Add(time.Second),
			UpdatedAt:   now.Add(time.Second),
		},
	} {
		if err := cat.UpsertVideo(ctx, v); err != nil {
			t.Fatalf("seed video %s: %v", v.ID, err)
		}
	}

	items, total, err := cat.ListVideos(ctx, ListParams{Page: 1, PageSize: 10})
	if err != nil {
		t.Fatalf("list videos: %v", err)
	}
	if total != 1 || len(items) != 1 {
		t.Fatalf("visible videos total=%d len=%d, want 1", total, len(items))
	}
	if items[0].ID != "video-1" {
		t.Fatalf("visible id = %q, want video-1", items[0].ID)
	}
}

func TestListVideosCanFilterReadyThumbnails(t *testing.T) {
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
			ID:           "ready-video",
			DriveID:      "drive",
			FileID:       "file-ready",
			Title:        "Ready",
			ThumbnailURL: "/p/thumb/ready-video",
			PublishedAt:  now,
			CreatedAt:    now,
			UpdatedAt:    now,
		},
		{
			ID:          "pending-video",
			DriveID:     "drive",
			FileID:      "file-pending",
			Title:       "Pending",
			PublishedAt: now.Add(time.Second),
			CreatedAt:   now.Add(time.Second),
			UpdatedAt:   now.Add(time.Second),
		},
	} {
		if err := cat.UpsertVideo(ctx, v); err != nil {
			t.Fatalf("seed video %s: %v", v.ID, err)
		}
	}

	items, total, err := cat.ListVideos(ctx, ListParams{
		Page: 1, PageSize: 10, ThumbnailReadyOnly: true,
	})
	if err != nil {
		t.Fatalf("list videos: %v", err)
	}
	if total != 1 || len(items) != 1 {
		t.Fatalf("ready videos total=%d len=%d, want 1", total, len(items))
	}
	if items[0].ID != "ready-video" {
		t.Fatalf("ready video id = %q, want ready-video", items[0].ID)
	}
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func mustListTags(t *testing.T, ctx context.Context, cat *Catalog) []Tag {
	t.Helper()
	tags, err := cat.ListTags(ctx)
	if err != nil {
		t.Fatalf("list tags: %v", err)
	}
	return tags
}

func mustTagByLabel(t *testing.T, ctx context.Context, cat *Catalog, label string) Tag {
	t.Helper()
	for _, tag := range mustListTags(t, ctx, cat) {
		if tag.Label == label {
			return tag
		}
	}
	t.Fatalf("tag %q not found", label)
	return Tag{}
}

func videoUpdatedAtByID(t *testing.T, ctx context.Context, cat *Catalog, ids ...string) map[string]int64 {
	t.Helper()
	out := make(map[string]int64, len(ids))
	for _, id := range ids {
		var updatedAt int64
		if err := cat.db.QueryRowContext(ctx, `SELECT updated_at FROM videos WHERE id = ?`, id).Scan(&updatedAt); err != nil {
			t.Fatalf("read updated_at for %s: %v", id, err)
		}
		out[id] = updatedAt
	}
	return out
}

// 删除 collection 标签的最后一个引用视频后，标签应当自动从 tags 表里消失。
// user/system 标签不受影响：用户/系统标签的语义由人维护，孤儿状态保留。
func TestDeleteVideoPrunesOrphanCollectionTag(t *testing.T) {
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
	for _, id := range []string{"video-a", "video-b"} {
		if err := cat.UpsertVideo(ctx, &Video{
			ID:          id,
			DriveID:     "drive",
			FileID:      id,
			Title:       id,
			Category:    "Better Call Saul S02",
			PublishedAt: now,
			CreatedAt:   now,
			UpdatedAt:   now,
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}

	label, ok, err := cat.EnsureCollectionTag(ctx, "Better Call Saul S02")
	if err != nil {
		t.Fatalf("ensure collection tag: %v", err)
	}
	if !ok || label != "Better Call Saul S02" {
		t.Fatalf("ensure collection tag = %q ok=%v, want collection tag created", label, ok)
	}

	// 用户标签：手动建出来，让它和 video-a 关联，验证 user 标签不会被孤儿清理流程误删。
	if _, err := cat.CreateTagAndClassify(ctx, "用户标签", nil, "user"); err != nil {
		t.Fatalf("create user tag: %v", err)
	}
	if err := cat.SetManualVideoTags(ctx, "video-a", []string{"用户标签"}); err != nil {
		t.Fatalf("attach user tag: %v", err)
	}

	collectionExists := func() bool {
		var n int
		if err := cat.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM tags WHERE label = ? AND source = 'collection'`,
			"Better Call Saul S02").Scan(&n); err != nil {
			t.Fatalf("count collection tag: %v", err)
		}
		return n > 0
	}
	if !collectionExists() {
		t.Fatal("collection tag missing right after creation")
	}

	// 删第一个视频：还有 video-b 在引用 collection 标签，应保留。
	if err := cat.DeleteVideo(ctx, "video-a"); err != nil {
		t.Fatalf("delete video-a: %v", err)
	}
	if !collectionExists() {
		t.Fatal("collection tag was pruned while another video still references it")
	}

	// 删最后一个引用视频，collection 标签应当被同步清掉。
	if err := cat.DeleteVideo(ctx, "video-b"); err != nil {
		t.Fatalf("delete video-b: %v", err)
	}
	if collectionExists() {
		t.Fatal("orphan collection tag was not pruned after deleting the last referencing video")
	}

	// 用户手动建的标签即使变成孤儿（已经因为 video-a 删除而失去引用）也必须保留。
	var userCount int
	if err := cat.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tags WHERE label = ? AND source = 'user'`,
		"用户标签").Scan(&userCount); err != nil {
		t.Fatalf("count user tag: %v", err)
	}
	if userCount != 1 {
		t.Fatalf("user tag count = %d, want 1 (user-source orphans must be preserved)", userCount)
	}

	// AV 系统标签也不能被孤儿清理影响。
	var avCount int
	if err := cat.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tags WHERE label = 'AV' AND source = 'system'`).Scan(&avCount); err != nil {
		t.Fatalf("count av tag: %v", err)
	}
	if avCount != 1 {
		t.Fatalf("system AV tag count = %d, want 1", avCount)
	}
}

// 重启时 migrate 应当一次性把历史遗留的孤儿 collection 标签清掉。
func TestMigratePrunesPreexistingOrphanCollectionTags(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/catalog.db"
	cat, err := Open(path)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}

	// 直接往 tags 表里写两条 collection 行：一条没有任何 video_tags 关联（孤儿），另一条人为关联视频（非孤儿）。
	now := time.Now().UnixMilli()
	if _, err := cat.db.ExecContext(ctx,
		`INSERT INTO tags (label, aliases, source, created_at, updated_at) VALUES (?, '[]', 'collection', ?, ?)`,
		"孤儿合集", now, now); err != nil {
		t.Fatalf("insert orphan tag: %v", err)
	}
	if _, err := cat.db.ExecContext(ctx,
		`INSERT INTO tags (label, aliases, source, created_at, updated_at) VALUES (?, '[]', 'collection', ?, ?)`,
		"在用合集", now, now); err != nil {
		t.Fatalf("insert in-use tag: %v", err)
	}
	var inUseTagID int64
	if err := cat.db.QueryRowContext(ctx, `SELECT id FROM tags WHERE label = ?`, "在用合集").Scan(&inUseTagID); err != nil {
		t.Fatalf("lookup in-use tag id: %v", err)
	}
	if err := cat.UpsertVideo(ctx, &Video{
		ID:          "video-keeper",
		DriveID:     "drive",
		FileID:      "file-keeper",
		Title:       "keeper",
		PublishedAt: time.Now(),
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}); err != nil {
		t.Fatalf("seed keeper video: %v", err)
	}
	if _, err := cat.db.ExecContext(ctx,
		`INSERT INTO video_tags (video_id, tag_id, source, created_at) VALUES (?, ?, 'auto', ?)`,
		"video-keeper", inUseTagID, now); err != nil {
		t.Fatalf("attach in-use tag: %v", err)
	}

	// 同样写一个 user 来源的孤儿，验证 migrate 不会误删 user 孤儿。
	if _, err := cat.db.ExecContext(ctx,
		`INSERT INTO tags (label, aliases, source, created_at, updated_at) VALUES (?, '[]', 'user', ?, ?)`,
		"用户孤儿", now, now); err != nil {
		t.Fatalf("insert user orphan: %v", err)
	}

	if err := cat.Close(); err != nil {
		t.Fatalf("close before reopen: %v", err)
	}

	// 重新打开 → 触发 migrate → 应当只清掉 source='collection' 且无引用的 "孤儿合集"。
	cat2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat2.Close() })

	count := func(label string) int {
		var n int
		if err := cat2.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM tags WHERE label = ?`, label).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", label, err)
		}
		return n
	}
	if count("孤儿合集") != 0 {
		t.Fatal("migrate did not prune orphan collection tag")
	}
	if count("在用合集") != 1 {
		t.Fatal("migrate wrongly pruned in-use collection tag")
	}
	if count("用户孤儿") != 1 {
		t.Fatal("migrate wrongly pruned user-source orphan tag")
	}
}

// TestReconcileThumbnailStatusOnce 检查升级时的"url 已写但 status=pending"修复。
// catalog.Open 会自动跑这个 migration（调用链 Open→ensureSchema→reconcileThumbnailStatusOnce）。
// 因此这里通过手动写脏数据 + 直接调 reconcile 来验证；脏数据是"绕过 Open
// 默认运行的 migration"的近似 —— 写完后状态就和遗留库一样。
func TestReconcileThumbnailStatusOnce(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { cat.Close() })

	now := time.Now()
	cases := []struct {
		id, url, status string
		wantStatus      string
	}{
		{"v-pending-url", "/p/thumb/v-pending-url", "pending", "ready"},             // 主要修复目标
		{"v-empty-url-pending", "", "pending", "pending"},                           // 没 url 不动
		{"v-failed-with-url", "/p/thumb/v-failed-with-url", "failed", "failed"},     // 显式失败保留
		{"v-empty-url-failed", "", "failed", "failed"},                              // 失败 + 没 url 也保留
		{"v-skipped-with-url", "/p/thumb/v-skipped-with-url", "skipped", "skipped"}, // 已跳过的时长补全保留
		{"v-already-ready", "/p/thumb/v-already-ready", "ready", "ready"},           // 幂等
	}
	for _, c := range cases {
		if err := cat.UpsertVideo(ctx, &Video{
			ID: c.id, DriveID: "d", FileID: "f-" + c.id,
			Title: c.id, ThumbnailURL: c.url,
			PublishedAt: now, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("seed %s: %v", c.id, err)
		}
		// UpsertVideo 默认把 thumbnail_status 留给 schema DEFAULT 'pending'。
		// 显式覆盖成测试想要的状态。
		if _, err := cat.db.ExecContext(ctx,
			`UPDATE videos SET thumbnail_status = ? WHERE id = ?`, c.status, c.id); err != nil {
			t.Fatalf("force seed status %s: %v", c.id, err)
		}
	}
	// 抹掉 Open 自动跑过的 marker，让我们直接验证函数本体
	if err := cat.SetSetting(ctx, "videos.thumbnail_status.url_present_to_ready_migrated", ""); err != nil {
		t.Fatalf("clear marker: %v", err)
	}

	if err := cat.reconcileThumbnailStatusOnce(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	for _, c := range cases {
		var got string
		if err := cat.db.QueryRowContext(ctx,
			`SELECT thumbnail_status FROM videos WHERE id = ?`, c.id).Scan(&got); err != nil {
			t.Fatalf("read %s: %v", c.id, err)
		}
		if got != c.wantStatus {
			t.Errorf("%s: status = %q, want %q", c.id, got, c.wantStatus)
		}
	}

	// 二次调用应是 no-op（marker 已写）
	// 为验证：人为再插一条脏数据，第二次 reconcile 不应碰它
	if err := cat.UpsertVideo(ctx, &Video{
		ID: "v-second-call", DriveID: "d", FileID: "f-2nd",
		Title: "second", ThumbnailURL: "/p/thumb/v-second-call",
		PublishedAt: now, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed second-call: %v", err)
	}
	if _, err := cat.db.ExecContext(ctx,
		`UPDATE videos SET thumbnail_status='pending' WHERE id='v-second-call'`); err != nil {
		t.Fatalf("force-pending second-call: %v", err)
	}
	if err := cat.reconcileThumbnailStatusOnce(ctx); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	var status string
	if err := cat.db.QueryRowContext(ctx,
		`SELECT thumbnail_status FROM videos WHERE id='v-second-call'`).Scan(&status); err != nil {
		t.Fatalf("read second-call: %v", err)
	}
	if status != "pending" {
		t.Errorf("second-call status = %q, want pending (migration should be no-op after marker)", status)
	}
}

// TestUpsertVideoSyncsThumbnailStatus 验证 scanner 创建/补回视频时
// thumbnail_status 跟随 thumbnail_url 自动设。这是历史 bug 的修复回归测试 ——
// 之前 UpsertVideo 的 SQL 不带 thumbnail_status 列，所有新视频都依赖
// 列 DEFAULT 'pending'，url 非空时和 status 字段长期不一致。
func TestUpsertVideoSyncsThumbnailStatusFromURL(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { cat.Close() })

	now := time.Now()
	cases := []struct {
		name       string
		thumb      string
		wantStatus string
	}{
		{"insert with remote URL → ready", "https://drive.example/thumb.jpg", "ready"},
		{"insert with local /p/thumb URL → ready", "/p/thumb/insert-local", "ready"},
		{"insert without URL → pending", "", "pending"},
	}
	for _, c := range cases {
		id := "ins-" + c.wantStatus + "-" + c.thumb
		if err := cat.UpsertVideo(ctx, &Video{
			ID: id, DriveID: "d", FileID: "f-" + id, Title: c.name,
			ThumbnailURL: c.thumb, PublishedAt: now, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("%s: upsert: %v", c.name, err)
		}
		var got string
		if err := cat.db.QueryRowContext(ctx,
			`SELECT thumbnail_status FROM videos WHERE id = ?`, id).Scan(&got); err != nil {
			t.Fatalf("%s: read: %v", c.name, err)
		}
		if got != c.wantStatus {
			t.Errorf("%s: status = %q, want %q", c.name, got, c.wantStatus)
		}
	}
}

func TestUpsertVideoOnConflictSyncsStatusOnURLChange(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { cat.Close() })

	now := time.Now()
	id := "conflict-vid"

	// 第一次 upsert：无 url → pending
	if err := cat.UpsertVideo(ctx, &Video{
		ID: id, DriveID: "d", FileID: "f", Title: "v",
		PublishedAt: now, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	var s string
	cat.db.QueryRowContext(ctx, `SELECT thumbnail_status FROM videos WHERE id=?`, id).Scan(&s)
	if s != "pending" {
		t.Fatalf("first: status = %q, want pending", s)
	}

	// 第二次 upsert（ON CONFLICT 路径）：带上 url → 自动同步 status='ready'
	if err := cat.UpsertVideo(ctx, &Video{
		ID: id, DriveID: "d", FileID: "f", Title: "v",
		ThumbnailURL: "https://drive.example/thumb.jpg",
		PublishedAt:  now, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	cat.db.QueryRowContext(ctx, `SELECT thumbnail_status FROM videos WHERE id=?`, id).Scan(&s)
	if s != "ready" {
		t.Fatalf("after url upsert: status = %q, want ready", s)
	}

	// 第三次 upsert：清空 url（直 SQL 模拟 clearVolatileOneDriveThumbnails 没走 UpsertVideo
	// 这条路径，但场景就是用户手动把 thumbnail 改空。url='' 时 UpsertVideo
	// 不应改变已有 status，因为 UpsertVideo 不是清空场景的合法接口）。
	if err := cat.UpsertVideo(ctx, &Video{
		ID: id, DriveID: "d", FileID: "f", Title: "v",
		ThumbnailURL: "",
		PublishedAt:  now, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("third upsert: %v", err)
	}
	cat.db.QueryRowContext(ctx, `SELECT thumbnail_status FROM videos WHERE id=?`, id).Scan(&s)
	if s != "ready" {
		t.Fatalf("after url cleared via upsert: status = %q, want unchanged 'ready'", s)
	}
}

func TestUpdateVideoMetaInfersReadyWhenURLPresent(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { cat.Close() })

	now := time.Now()
	id := "meta-test"
	if err := cat.UpsertVideo(ctx, &Video{
		ID: id, DriveID: "d", FileID: "f", Title: "v",
		PublishedAt: now, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// status 初始 pending（无 url）
	var s string
	cat.db.QueryRowContext(ctx, `SELECT thumbnail_status FROM videos WHERE id=?`, id).Scan(&s)
	if s != "pending" {
		t.Fatalf("seed status = %q, want pending", s)
	}

	// 仅传 ThumbnailURL，期望 status 自动推到 'ready'
	if err := cat.UpdateVideoMeta(ctx, id, VideoMetaPatch{
		ThumbnailURL: "/p/thumb/" + id,
	}); err != nil {
		t.Fatalf("update meta: %v", err)
	}
	cat.db.QueryRowContext(ctx, `SELECT thumbnail_status FROM videos WHERE id=?`, id).Scan(&s)
	if s != "ready" {
		t.Errorf("after URL-only patch: status = %q, want ready (auto-inferred)", s)
	}

	// 显式传 ThumbnailStatus 时 patch 应该被尊重，而不是被自动推断覆盖
	if err := cat.UpdateVideoMeta(ctx, id, VideoMetaPatch{
		ThumbnailURL:    "/p/thumb/another",
		ThumbnailStatus: "failed",
	}); err != nil {
		t.Fatalf("update meta with status: %v", err)
	}
	cat.db.QueryRowContext(ctx, `SELECT thumbnail_status FROM videos WHERE id=?`, id).Scan(&s)
	if s != "failed" {
		t.Errorf("explicit status overrides inference: got %q, want failed", s)
	}
}

func TestClearVolatileOneDriveThumbnailsResetsStatus(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { cat.Close() })

	now := time.Now()
	if err := cat.UpsertVideo(ctx, &Video{
		ID: "od-vid", DriveID: "OneDrive", FileID: "od-1", Title: "od",
		ThumbnailURL: "https://westus21-mediap.svc.ms/transform/thumbnail?provider=spo&tempauth=expired",
		PublishedAt:  now, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// 经 UpsertVideo 自动同步，此时 status='ready'
	var s string
	cat.db.QueryRowContext(ctx, `SELECT thumbnail_status FROM videos WHERE id='od-vid'`).Scan(&s)
	if s != "ready" {
		t.Fatalf("pre-clear: status = %q, want ready", s)
	}

	if err := cat.clearVolatileOneDriveThumbnails(ctx); err != nil {
		t.Fatalf("clear: %v", err)
	}

	var url string
	cat.db.QueryRowContext(ctx, `SELECT COALESCE(thumbnail_url,''), thumbnail_status FROM videos WHERE id='od-vid'`).Scan(&url, &s)
	if url != "" {
		t.Errorf("url after clear = %q, want empty", url)
	}
	if s != "pending" {
		t.Errorf("status after clear = %q, want pending (so worker re-enqueues)", s)
	}
}
