package catalog

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

type Catalog struct {
	db *sql.DB
}

func Open(path string) (*Catalog, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	c := &Catalog{db: db}
	if err := c.migrate(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate catalog: %w", err)
	}
	return c, nil
}

func (c *Catalog) Close() error { return c.db.Close() }

// ---------- Video ----------

type Video struct {
	ID              string    `json:"id"`
	DriveID         string    `json:"driveId"`
	FileID          string    `json:"fileId"`
	FileName        string    `json:"fileName"`
	ContentHash     string    `json:"contentHash"`
	ParentID        string    `json:"parentId"`
	Title           string    `json:"title"`
	Author          string    `json:"author"`
	Tags            []string  `json:"tags"`
	DurationSeconds int       `json:"durationSeconds"`
	Size            int64     `json:"size"`
	Ext             string    `json:"ext"`
	Quality         string    `json:"quality"`
	ThumbnailURL    string    `json:"thumbnailUrl"`
	PreviewFileID   string    `json:"previewFileId"`
	PreviewLocal    string    `json:"previewLocal"`
	PreviewStatus   string    `json:"previewStatus"`
	Views           int       `json:"views"`
	Favorites       int       `json:"favorites"`
	Comments        int       `json:"comments"`
	Likes           int       `json:"likes"`
	Dislikes        int       `json:"dislikes"`
	Category        string    `json:"category"`
	Hidden          bool      `json:"hidden"`
	Badges          []string  `json:"badges"`
	Description     string    `json:"description"`
	PublishedAt     time.Time `json:"publishedAt"`
	CreatedAt       time.Time `json:"createdAt"`
	UpdatedAt       time.Time `json:"updatedAt"`
}

func (c *Catalog) UpsertVideo(ctx context.Context, v *Video) error {
	existed := c.videoExists(ctx, v.ID)
	v.ContentHash = normalizeContentHash(v.ContentHash)
	tagsJSON, _ := json.Marshal(v.Tags)
	badgesJSON, _ := json.Marshal(v.Badges)
	now := time.Now().UnixMilli()
	if v.CreatedAt.IsZero() {
		v.CreatedAt = time.UnixMilli(now)
	}
	v.UpdatedAt = time.UnixMilli(now)

	_, err := c.db.ExecContext(ctx, `
INSERT INTO videos (
  id, drive_id, file_id, file_name, content_hash, parent_id, title, author, tags,
  duration_seconds, size_bytes, ext, quality, thumbnail_url,
  preview_file_id, preview_local, preview_status,
  views, favorites, comments, likes, dislikes,
  category, hidden, badges, description, published_at, created_at, updated_at
) VALUES (
  ?, ?, ?, ?, ?, ?, ?, ?, ?,
  ?, ?, ?, ?, ?,
  ?, ?, ?,
  ?, ?, ?, ?, ?,
  ?, ?, ?, ?, ?, ?, ?
)
ON CONFLICT(id) DO UPDATE SET
  file_name       = CASE
                      WHEN excluded.file_name != '' THEN excluded.file_name
                      ELSE videos.file_name
                    END,
  title           = excluded.title,
  author          = excluded.author,
  tags            = excluded.tags,
  content_hash    = CASE
                      WHEN excluded.content_hash != '' THEN excluded.content_hash
                      ELSE videos.content_hash
                    END,
  duration_seconds= excluded.duration_seconds,
  size_bytes      = excluded.size_bytes,
  ext             = excluded.ext,
  quality         = excluded.quality,
  thumbnail_url   = excluded.thumbnail_url,
  category        = excluded.category,
  badges          = excluded.badges,
  description     = excluded.description,
  updated_at      = excluded.updated_at
`,
		v.ID, v.DriveID, v.FileID, v.FileName, v.ContentHash, v.ParentID, v.Title, v.Author, string(tagsJSON),
		v.DurationSeconds, v.Size, v.Ext, v.Quality, v.ThumbnailURL,
		v.PreviewFileID, v.PreviewLocal, nullableStatus(v.PreviewStatus),
		v.Views, v.Favorites, v.Comments, v.Likes, v.Dislikes,
		v.Category, boolToInt(v.Hidden), string(badgesJSON), v.Description,
		v.PublishedAt.UnixMilli(), v.CreatedAt.UnixMilli(), v.UpdatedAt.UnixMilli(),
	)
	if err != nil {
		return err
	}
	if len(v.Tags) > 0 && !existed {
		return c.replaceVideoTags(ctx, v.ID, v.Tags, "auto", false, true)
	}
	return nil
}

func nullableStatus(s string) string {
	if s == "" {
		return "pending"
	}
	return s
}

func (c *Catalog) UpdatePreview(ctx context.Context, id, previewFileID, previewLocal, status string) error {
	_, err := c.db.ExecContext(ctx,
		`UPDATE videos SET preview_file_id = ?, preview_local = ?, preview_status = ?, updated_at = ? WHERE id = ?`,
		previewFileID, previewLocal, status, time.Now().UnixMilli(), id)
	return err
}

func (c *Catalog) HideVideo(ctx context.Context, id string) error {
	res, err := c.db.ExecContext(ctx,
		`UPDATE videos SET hidden = 1, updated_at = ? WHERE id = ?`,
		time.Now().UnixMilli(), id)
	if err != nil {
		return err
	}
	if rows, err := res.RowsAffected(); err == nil && rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// IncrementLike 原子 +1，返回最新点赞数
func (c *Catalog) IncrementLike(ctx context.Context, id string) (int, error) {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`UPDATE videos SET likes = likes + 1, updated_at = ? WHERE id = ?`,
		time.Now().UnixMilli(), id); err != nil {
		return 0, err
	}
	var likes int
	if err := tx.QueryRowContext(ctx, `SELECT likes FROM videos WHERE id = ?`, id).Scan(&likes); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return likes, nil
}

// VideoMetaPatch 轻量更新视频元数据（仅非零值字段会被写入）
type VideoMetaPatch struct {
	ThumbnailURL    string
	DurationSeconds int
	Category        string
	ContentHash     string
	FileName        string
	Tags            []string
	TagsSet         bool
}

func (c *Catalog) UpdateVideoMeta(ctx context.Context, id string, p VideoMetaPatch) error {
	parts := []string{}
	args := []any{}
	if p.ThumbnailURL != "" {
		parts = append(parts, "thumbnail_url = ?")
		args = append(args, p.ThumbnailURL)
	}
	if p.DurationSeconds > 0 {
		parts = append(parts, "duration_seconds = ?")
		args = append(args, p.DurationSeconds)
	}
	if p.Category != "" {
		parts = append(parts, "category = ?")
		args = append(args, p.Category)
	}
	if p.ContentHash != "" {
		parts = append(parts, "content_hash = ?")
		args = append(args, normalizeContentHash(p.ContentHash))
	}
	if p.FileName != "" {
		parts = append(parts, "file_name = ?")
		args = append(args, p.FileName)
	}
	if p.TagsSet {
		tagsJSON, _ := json.Marshal(p.Tags)
		parts = append(parts, "tags = ?")
		args = append(args, string(tagsJSON))
	}
	if len(parts) == 0 {
		return nil
	}
	parts = append(parts, "updated_at = ?")
	args = append(args, time.Now().UnixMilli())
	args = append(args, id)
	q := `UPDATE videos SET ` + strings.Join(parts, ", ") + ` WHERE id = ?`
	if _, err := c.db.ExecContext(ctx, q, args...); err != nil {
		return err
	}
	if p.TagsSet {
		return c.SetAutoVideoTags(ctx, id, p.Tags)
	}
	return nil
}

// ListCategories 聚合所有 category，按视频数降序
type CategoryStat struct {
	Category string
	Count    int
}

func (c *Catalog) ListCategories(ctx context.Context) ([]CategoryStat, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT COALESCE(category, '') AS c, COUNT(*) AS cnt
		 FROM videos
		 WHERE category IS NOT NULL AND category != ''
		   AND COALESCE(hidden, 0) = 0
		 GROUP BY c
		 ORDER BY cnt DESC, c ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CategoryStat
	for rows.Next() {
		var s CategoryStat
		if err := rows.Scan(&s.Category, &s.Count); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

type TagStat struct {
	Label string
	Count int
}

func (c *Catalog) CountTags(ctx context.Context, labels []string) ([]TagStat, error) {
	out := make([]TagStat, 0, len(labels))
	for _, label := range labels {
		var count int
		if err := c.db.QueryRowContext(ctx,
			`SELECT COUNT(*)
			 FROM video_tags vt
			 JOIN tags t ON t.id = vt.tag_id
			 JOIN videos v ON v.id = vt.video_id
			 WHERE t.label = ? COLLATE NOCASE
			   AND COALESCE(v.hidden, 0) = 0`,
			label,
		).Scan(&count); err != nil {
			return nil, err
		}
		out = append(out, TagStat{Label: label, Count: count})
	}
	return out, nil
}

// ListVideosByPreviewStatus 按预览状态列出全部视频，通常用于启动补扫
func (c *Catalog) ListVideosByPreviewStatus(ctx context.Context, driveID, status string, limit int) ([]*Video, error) {
	if limit <= 0 {
		limit = 10000
	}
	rows, err := c.db.QueryContext(ctx,
		`SELECT `+allVideoCols+` FROM videos
		 WHERE drive_id = ? AND preview_status = ?
		   AND COALESCE(hidden, 0) = 0
		   AND `+uniqueVideoWhereSQL+`
		 ORDER BY created_at ASC LIMIT ?`,
		driveID, status, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Video
	for rows.Next() {
		v, err := scanVideo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// ListVideosNeedingThumbnail returns videos that do not have any cover URL yet.
func (c *Catalog) ListVideosNeedingThumbnail(ctx context.Context, driveID string, limit int) ([]*Video, error) {
	if limit <= 0 {
		limit = 10000
	}
	rows, err := c.db.QueryContext(ctx,
		`SELECT `+allVideoCols+` FROM videos
		 WHERE drive_id = ?
		   AND COALESCE(thumbnail_url, '') = ''
		   AND COALESCE(hidden, 0) = 0
		   AND `+uniqueVideoWhereSQL+`
		 ORDER BY created_at ASC
		 LIMIT ?`,
		driveID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Video
	for rows.Next() {
		v, err := scanVideo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func (c *Catalog) CountVideosNeedingThumbnail(ctx context.Context, driveID string) (int, error) {
	var count int
	err := c.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM videos
		 WHERE drive_id = ?
		   AND COALESCE(thumbnail_url, '') = ''
		   AND COALESCE(hidden, 0) = 0
		   AND `+uniqueVideoWhereSQL,
		driveID).Scan(&count)
	return count, err
}

func (c *Catalog) GetVideo(ctx context.Context, id string) (*Video, error) {
	row := c.db.QueryRowContext(ctx, `SELECT `+allVideoCols+` FROM videos WHERE id = ?`, id)
	return scanVideo(row)
}

func (c *Catalog) ListVideosByDrive(ctx context.Context, driveID string) ([]*Video, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT `+allVideoCols+` FROM videos WHERE drive_id = ? ORDER BY created_at ASC, id ASC`,
		driveID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Video
	for rows.Next() {
		v, err := scanVideo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (c *Catalog) DeleteVideo(ctx context.Context, id string) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM video_tags WHERE video_id = ?`, id); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM videos WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if rows, err := res.RowsAffected(); err == nil && rows == 0 {
		return sql.ErrNoRows
	}
	return tx.Commit()
}

func (c *Catalog) FindVideoByContentHash(ctx context.Context, hash string) (*Video, error) {
	hash = normalizeContentHash(hash)
	if hash == "" {
		return nil, sql.ErrNoRows
	}
	row := c.db.QueryRowContext(ctx,
		`SELECT `+allVideoCols+`
		 FROM videos
		 WHERE content_hash = ?
		 ORDER BY created_at ASC, id ASC
		 LIMIT 1`, hash)
	return scanVideo(row)
}

func (c *Catalog) FindVideoByFileSignature(ctx context.Context, fileName string, size int64) (*Video, error) {
	if fileName == "" || size <= 0 {
		return nil, sql.ErrNoRows
	}
	row := c.db.QueryRowContext(ctx,
		`SELECT `+allVideoCols+`
		 FROM videos
		 WHERE file_name = ? AND size_bytes = ?
		 ORDER BY created_at ASC, id ASC
		 LIMIT 1`, fileName, size)
	return scanVideo(row)
}

type ListParams struct {
	Keyword  string
	DriveID  string
	Tag      string
	Category string
	Sort     string // latest | hot | week | long
	Page     int
	PageSize int
}

func (c *Catalog) ListVideos(ctx context.Context, p ListParams) ([]*Video, int, error) {
	if p.PageSize <= 0 {
		p.PageSize = 24
	}
	if p.Page <= 0 {
		p.Page = 1
	}

	var where []string
	var args []any
	if p.Keyword != "" {
		where = append(where, "(title LIKE ? OR author LIKE ?)")
		like := "%" + p.Keyword + "%"
		args = append(args, like, like)
	}
	if p.DriveID != "" {
		where = append(where, "drive_id = ?")
		args = append(args, p.DriveID)
	}
	if p.Tag != "" {
		where = append(where, `EXISTS (
			SELECT 1
			FROM video_tags vt
			JOIN tags t ON t.id = vt.tag_id
			WHERE vt.video_id = videos.id AND t.label = ? COLLATE NOCASE
		)`)
		args = append(args, p.Tag)
	}
	if p.Category != "" && p.Category != "all" {
		where = append(where, "category = ?")
		args = append(args, p.Category)
	}
	where = append(where, "COALESCE(hidden, 0) = 0")
	where = append(where, uniqueVideoWhereSQL)

	whereSQL := ""
	whereSQL = " WHERE " + strings.Join(where, " AND ")

	orderBy := " ORDER BY published_at DESC"
	switch p.Sort {
	case "hot":
		// 热度 = 点赞数，点赞相同按最新
		orderBy = " ORDER BY likes DESC, published_at DESC"
	case "week":
		orderBy = " ORDER BY likes DESC"
	case "long":
		orderBy = " ORDER BY duration_seconds DESC"
	}

	// count
	var total int
	if err := c.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM videos"+whereSQL, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	// list
	offset := (p.Page - 1) * p.PageSize
	rows, err := c.db.QueryContext(ctx,
		"SELECT "+allVideoCols+" FROM videos"+whereSQL+orderBy+" LIMIT ? OFFSET ?",
		append(args, p.PageSize, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var out []*Video
	for rows.Next() {
		v, err := scanVideo(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, v)
	}
	return out, total, nil
}

type DriveTeaserCounts struct {
	Ready   int
	Pending int
	Failed  int
}

func (c *Catalog) CountTeasersByDrive(ctx context.Context) (map[string]DriveTeaserCounts, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT drive_id,
		        COUNT(CASE WHEN COALESCE(preview_status, 'pending') = 'ready' THEN 1 END) AS ready_count,
		        COUNT(CASE WHEN COALESCE(preview_status, 'pending') = 'pending' THEN 1 END) AS pending_count,
		        COUNT(CASE WHEN COALESCE(preview_status, 'pending') = 'failed' THEN 1 END) AS failed_count
		   FROM videos
		  WHERE COALESCE(hidden, 0) = 0
		    AND `+uniqueVideoWhereSQL+`
		  GROUP BY drive_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]DriveTeaserCounts)
	for rows.Next() {
		var driveID string
		var counts DriveTeaserCounts
		if err := rows.Scan(&driveID, &counts.Ready, &counts.Pending, &counts.Failed); err != nil {
			return nil, err
		}
		out[driveID] = counts
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

type LocalMediaRef struct {
	DriveID      string
	VideoID      string
	PreviewLocal string
}

func (c *Catalog) ListLocalMediaRefs(ctx context.Context) ([]LocalMediaRef, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT drive_id, id, COALESCE(preview_local, '')
		   FROM videos`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []LocalMediaRef
	for rows.Next() {
		var ref LocalMediaRef
		if err := rows.Scan(&ref.DriveID, &ref.VideoID, &ref.PreviewLocal); err != nil {
			return nil, err
		}
		out = append(out, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ---------- Drive ----------

type Drive struct {
	ID          string            `json:"id"`
	Kind        string            `json:"kind"`
	Name        string            `json:"name"`
	RootID      string            `json:"rootId"`
	ScanRootID  string            `json:"scanRootId"`
	Credentials map[string]string `json:"credentials,omitempty"`
	Status      string            `json:"status"`
	LastError   string            `json:"lastError,omitempty"`
	CreatedAt   time.Time         `json:"createdAt"`
	UpdatedAt   time.Time         `json:"updatedAt"`
}

func (c *Catalog) UpsertDrive(ctx context.Context, d *Drive) error {
	cred, _ := json.Marshal(d.Credentials)
	now := time.Now().UnixMilli()
	if d.CreatedAt.IsZero() {
		d.CreatedAt = time.UnixMilli(now)
	}
	d.UpdatedAt = time.UnixMilli(now)
	_, err := c.db.ExecContext(ctx, `
INSERT INTO drives (id, kind, name, root_id, scan_root_id, credentials, status, last_error, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
  kind         = excluded.kind,
  name         = excluded.name,
  root_id      = excluded.root_id,
  scan_root_id = excluded.scan_root_id,
  credentials  = excluded.credentials,
  status       = excluded.status,
  last_error   = excluded.last_error,
  updated_at   = excluded.updated_at
`, d.ID, d.Kind, d.Name, d.RootID, d.ScanRootID, string(cred), d.Status, d.LastError,
		d.CreatedAt.UnixMilli(), d.UpdatedAt.UnixMilli())
	return err
}

func (c *Catalog) ListDrives(ctx context.Context) ([]*Drive, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT id, kind, name, root_id, COALESCE(scan_root_id, ''), COALESCE(credentials, '{}'), status, COALESCE(last_error, ''), created_at, updated_at FROM drives ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Drive
	for rows.Next() {
		d := &Drive{}
		var credsStr string
		var createdAt, updatedAt int64
		if err := rows.Scan(&d.ID, &d.Kind, &d.Name, &d.RootID, &d.ScanRootID, &credsStr, &d.Status, &d.LastError, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(credsStr), &d.Credentials)
		d.CreatedAt = time.UnixMilli(createdAt)
		d.UpdatedAt = time.UnixMilli(updatedAt)
		out = append(out, d)
	}
	return out, nil
}

func (c *Catalog) GetDrive(ctx context.Context, id string) (*Drive, error) {
	row := c.db.QueryRowContext(ctx, `SELECT id, kind, name, root_id, COALESCE(scan_root_id, ''), COALESCE(credentials, '{}'), status, COALESCE(last_error, ''), created_at, updated_at FROM drives WHERE id = ?`, id)
	d := &Drive{}
	var credsStr string
	var createdAt, updatedAt int64
	if err := row.Scan(&d.ID, &d.Kind, &d.Name, &d.RootID, &d.ScanRootID, &credsStr, &d.Status, &d.LastError, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(credsStr), &d.Credentials)
	d.CreatedAt = time.UnixMilli(createdAt)
	d.UpdatedAt = time.UnixMilli(updatedAt)
	return d, nil
}

func (c *Catalog) DeleteDrive(ctx context.Context, id string) error {
	_, err := c.db.ExecContext(ctx, `DELETE FROM drives WHERE id = ?`, id)
	return err
}

// ---------- Admin session ----------

func (c *Catalog) CreateSession(ctx context.Context, token string, ttl time.Duration) error {
	now := time.Now()
	_, err := c.db.ExecContext(ctx,
		`INSERT INTO admin_sessions (token, created_at, expires_at) VALUES (?, ?, ?)`,
		token, now.UnixMilli(), now.Add(ttl).UnixMilli())
	return err
}

func (c *Catalog) ValidateSession(ctx context.Context, token string) (bool, error) {
	var expires int64
	err := c.db.QueryRowContext(ctx, `SELECT expires_at FROM admin_sessions WHERE token = ?`, token).Scan(&expires)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return time.Now().UnixMilli() < expires, nil
}

func (c *Catalog) DeleteSession(ctx context.Context, token string) error {
	_, err := c.db.ExecContext(ctx, `DELETE FROM admin_sessions WHERE token = ?`, token)
	return err
}

// ---------- Settings ----------

func (c *Catalog) GetSetting(ctx context.Context, key, defaultValue string) (string, error) {
	var v string
	err := c.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return defaultValue, nil
	}
	if err != nil {
		return "", err
	}
	return v, nil
}

func (c *Catalog) SetSetting(ctx context.Context, key, value string) error {
	_, err := c.db.ExecContext(ctx, `
INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at
`, key, value, time.Now().UnixMilli())
	return err
}

// ---------- helpers ----------

const allVideoCols = `
id, drive_id, file_id, COALESCE(file_name, ''), COALESCE(content_hash, ''), COALESCE(parent_id, ''), title, COALESCE(author, ''), COALESCE(tags, '[]'),
duration_seconds, size_bytes, COALESCE(ext, ''), COALESCE(quality, ''), COALESCE(thumbnail_url, ''),
COALESCE(preview_file_id, ''), COALESCE(preview_local, ''), COALESCE(preview_status, 'pending'),
views, favorites, comments, likes, dislikes,
COALESCE(category, ''), COALESCE(hidden, 0), COALESCE(badges, '[]'), COALESCE(description, ''),
published_at, created_at, updated_at
`

const uniqueVideoWhereSQL = `((COALESCE(videos.content_hash, '') = ''
		OR NOT EXISTS (
			SELECT 1
			FROM videos AS dup
			WHERE dup.content_hash = videos.content_hash
			  AND COALESCE(dup.content_hash, '') != ''
			  AND (
				dup.created_at < videos.created_at
				OR (dup.created_at = videos.created_at AND dup.id < videos.id)
			  )
		))
	AND (COALESCE(videos.file_name, '') = ''
		OR videos.size_bytes <= 0
		OR NOT EXISTS (
			SELECT 1
			FROM videos AS dup
			WHERE dup.file_name = videos.file_name
			  AND dup.size_bytes = videos.size_bytes
			  AND COALESCE(dup.file_name, '') != ''
			  AND dup.size_bytes > 0
			  AND (
				dup.created_at < videos.created_at
				OR (dup.created_at = videos.created_at AND dup.id < videos.id)
			  )
		)))`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanVideo(row rowScanner) (*Video, error) {
	v := &Video{}
	var tagsJSON, badgesJSON string
	var publishedAt, createdAt, updatedAt int64
	var hidden int
	err := row.Scan(
		&v.ID, &v.DriveID, &v.FileID, &v.FileName, &v.ContentHash, &v.ParentID, &v.Title, &v.Author, &tagsJSON,
		&v.DurationSeconds, &v.Size, &v.Ext, &v.Quality, &v.ThumbnailURL,
		&v.PreviewFileID, &v.PreviewLocal, &v.PreviewStatus,
		&v.Views, &v.Favorites, &v.Comments, &v.Likes, &v.Dislikes,
		&v.Category, &hidden, &badgesJSON, &v.Description,
		&publishedAt, &createdAt, &updatedAt,
	)
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(tagsJSON), &v.Tags)
	_ = json.Unmarshal([]byte(badgesJSON), &v.Badges)
	v.Hidden = hidden == 1
	v.PublishedAt = time.UnixMilli(publishedAt)
	v.CreatedAt = time.UnixMilli(createdAt)
	v.UpdatedAt = time.UnixMilli(updatedAt)
	return v, nil
}

func normalizeContentHash(hash string) string {
	return strings.ToLower(strings.TrimSpace(hash))
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
