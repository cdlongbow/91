package drives

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"
)

// Drive 是多家网盘统一抽象。上层不区分盘，只区分 Kind。
type Drive interface {
	// Kind 返回驱动代号："quark" / "p115" / "pikpak" / "wopan" / "onedrive" / "localstorage"
	Kind() string

	// ID 返回该盘在 catalog 中的唯一标识
	ID() string

	// Init 完成登录态校验；登录态由 Authenticator 另行获取后注入
	Init(ctx context.Context) error

	// List 列指定目录下的直接子项
	List(ctx context.Context, dirID string) ([]Entry, error)

	// Stat 拿到单个文件的元数据
	Stat(ctx context.Context, fileID string) (*Entry, error)

	// StreamURL 返回一次性直链 + 必须的请求头
	// 代理层据此回源，透传 Range
	StreamURL(ctx context.Context, fileID string) (*StreamLink, error)

	// Upload 把本地流写入指定目录，返回新文件 fileID。
	// 当前 teaser 和封面只保存在本地，不再通过该方法写回网盘。
	Upload(ctx context.Context, parentID, name string, r io.Reader, size int64) (string, error)

	// EnsureDir 保证指定路径存在（相对根目录），返回最终目录 fileID。
	EnsureDir(ctx context.Context, pathFromRoot string) (string, error)

	// RootID 返回根目录 fileID
	RootID() string
}

type Entry struct {
	ID       string
	Name     string
	Size     int64
	Hash     string
	IsDir    bool
	ParentID string
	MimeType string
	ModTime  time.Time

	// 部分网盘额外信息
	Category     int    // 1=视频 (quark)
	ThumbnailURL string // 网盘侧已提供的快速缩略图
}

type StreamLink struct {
	URL     string
	Headers http.Header
	Expires time.Time
}

// ErrNotSupported 代表某家盘不支持某操作
var ErrNotSupported = errors.New("operation not supported by this drive")

// RateLimitError 表示上游服务正在限流。RetryAfter 为 0 时由调用方选择默认冷却时间。
type RateLimitError struct {
	Provider   string
	RetryAfter time.Duration
	Err        error
}

func (e *RateLimitError) Error() string {
	if e == nil {
		return "rate limited"
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	if e.Provider != "" {
		return e.Provider + " rate limited"
	}
	return "rate limited"
}

func (e *RateLimitError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func RateLimitRetryAfter(err error) (time.Duration, bool) {
	var rateLimit *RateLimitError
	if errors.As(err, &rateLimit) {
		return rateLimit.RetryAfter, true
	}
	return 0, false
}
