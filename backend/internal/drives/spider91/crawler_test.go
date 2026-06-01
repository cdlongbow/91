package spider91

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/video-site/backend/internal/catalog"
)

// TestCrawlerRunOnceFullFlow 用一个伪 python 脚本 + httptest 服务器
// 把 Crawler.RunOnce 的完整流程跑一遍：脚本生成 JSON、下载视频和封面、入库、
// 重复运行跳过已存在的 91 源视频 ID。
func TestCrawlerRunOnceFullFlow(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-based fake script only on unix")
	}

	tmp := t.TempDir()

	// 1. 假 HTTP 服务器：根据路径返回视频数据或封面数据
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "120001.mp4"):
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write([]byte("FAKEVIDEO1"))
		case strings.Contains(r.URL.Path, "120002.mp4"):
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write([]byte("FAKEVIDEO2BYTES"))
		case strings.Contains(r.URL.Path, "/thumb/120001.jpg"):
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write([]byte("\xff\xd8\xff\xe0fakejpg1"))
		case strings.Contains(r.URL.Path, "/thumb/120002.jpg"):
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write([]byte("\xff\xd8\xff\xe0fakejpg2"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	// 2. 假 python 脚本：解析 --output / --stream-output 参数，
	//    在 stream 模式下逐行 echo 每条视频的 JSON 到 stdout（模拟 Python 端 stream），
	//    同时仍写 --output 文件作归档。
	videoEntries := []map[string]string{
		{
			"title":      "Video One",
			"thumb_url":  srv.URL + "/thumb/not-120001.jpg",
			"video_url":  srv.URL + "/videos/120001.mp4",
			"viewkey":    "vk-001",
			"detail_url": srv.URL + "/v.php?viewkey=vk-001",
		},
		{
			"title":      "Video Two",
			"thumb_url":  srv.URL + "/thumb/not-120002.jpg",
			"video_url":  srv.URL + "/videos/120002.mp4",
			"viewkey":    "vk-002",
			"detail_url": srv.URL + "/v.php?viewkey=vk-002",
		},
	}
	scriptPath := filepath.Join(tmp, "fake_spider.sh")
	scriptBody := buildFakeSpiderScript(videoEntries)
	if err := os.WriteFile(scriptPath, []byte(scriptBody), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	// 3. 准备 catalog + driver + crawler
	dbPath := filepath.Join(tmp, "test.db")
	cat, err := catalog.Open(dbPath)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer cat.Close()

	driveID := "spider91-test"
	rootDir := filepath.Join(tmp, "spider91", driveID)
	commonThumbs := filepath.Join(tmp, "previews", "thumbs")
	drv := New(Config{ID: driveID, RootDir: rootDir})

	// 把 drive 也写入 catalog（Crawler 不直接读，但 main 真实流程会写）
	if err := cat.UpsertDrive(context.Background(), &catalog.Drive{
		ID:   driveID,
		Kind: Kind,
		Name: "test crawler",
	}); err != nil {
		t.Fatalf("upsert drive: %v", err)
	}

	var newVideos []*catalog.Video
	c := NewCrawler(CrawlerConfig{
		Driver:          drv,
		Catalog:         cat,
		PythonPath:      "sh",
		ScriptPath:      scriptPath,
		CommonThumbDir:  commonThumbs,
		SpiderTimeout:   10 * time.Second,
		DownloadTimeout: 10 * time.Second,
		OnNewVideo: func(v *catalog.Video) {
			newVideos = append(newVideos, v)
		},
	})

	// 4. 第一次 RunOnce：应该新入库 2 条
	res, err := c.RunOnce(context.Background(), 15)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.NewVideos != 2 || res.Skipped != 0 || res.Failed != 0 {
		t.Fatalf("first run result: new=%d skipped=%d failed=%d, want 2/0/0",
			res.NewVideos, res.Skipped, res.Failed)
	}
	if res.TargetNew != 15 {
		t.Fatalf("first run TargetNew = %d, want 15", res.TargetNew)
	}
	if res.SeenSnapshot != 0 {
		t.Fatalf("first run SeenSnapshot = %d, want 0 (catalog empty before first run)", res.SeenSnapshot)
	}
	if len(newVideos) != 2 {
		t.Fatalf("OnNewVideo called %d times, want 2", len(newVideos))
	}

	// 5. 检查文件落盘
	for _, item := range []struct {
		sourceID string
		size     int64
	}{
		{"120001", 10},
		{"120002", 15},
	} {
		videoPath := filepath.Join(rootDir, "videos", item.sourceID+".mp4")
		info, err := os.Stat(videoPath)
		if err != nil {
			t.Fatalf("video %s missing: %v", item.sourceID, err)
		}
		if info.Size() != item.size {
			t.Fatalf("video %s size = %d, want %d", item.sourceID, info.Size(), item.size)
		}

		thumbPath := filepath.Join(rootDir, "thumbs", item.sourceID+".jpg")
		if _, err := os.Stat(thumbPath); err != nil {
			t.Fatalf("thumb %s missing: %v", item.sourceID, err)
		}

		// 复制到 common thumbs 目录的副本，名字按 videoID 来
		videoID := BuildVideoID(driveID, item.sourceID)
		commonThumb := filepath.Join(commonThumbs, videoID+".jpg")
		if _, err := os.Stat(commonThumb); err != nil {
			t.Fatalf("common thumb %s missing: %v", commonThumb, err)
		}
	}

	// 6. 检查 catalog 入库
	for _, sourceID := range []string{"120001", "120002"} {
		videoID := BuildVideoID(driveID, sourceID)
		v, err := cat.GetVideo(context.Background(), videoID)
		if err != nil {
			t.Fatalf("GetVideo %s: %v", videoID, err)
		}
		if v.DriveID != driveID {
			t.Fatalf("video %s drive_id = %q want %q", videoID, v.DriveID, driveID)
		}
		if v.FileID != sourceID+".mp4" {
			t.Fatalf("video %s file_id = %q want %q", videoID, v.FileID, sourceID+".mp4")
		}
		if v.ThumbnailURL == "" {
			t.Fatalf("video %s ThumbnailURL empty (cover should be ready)", videoID)
		}
		if v.Author != DefaultAuthor {
			t.Fatalf("video %s author = %q want %q", videoID, v.Author, DefaultAuthor)
		}
		// 每条视频都应该带 "91porn" 标签（UpsertVideo 路径自动同步 tags 表）
		hasDefaultTag := false
		for _, tag := range v.Tags {
			if tag == DefaultTag {
				hasDefaultTag = true
				break
			}
		}
		if !hasDefaultTag {
			t.Fatalf("video %s tags = %v, want contain %q", videoID, v.Tags, DefaultTag)
		}
	}

	// 7. 第二次 RunOnce：源视频 ID 已存在 → 全部 skipped，无新文件下载
	newVideos = nil
	res2, err := c.RunOnce(context.Background(), 15)
	if err != nil {
		t.Fatalf("second RunOnce: %v", err)
	}
	if res2.NewVideos != 0 {
		t.Fatalf("second run NewVideos = %d, want 0", res2.NewVideos)
	}
	if res2.Skipped != 2 {
		t.Fatalf("second run Skipped = %d, want 2", res2.Skipped)
	}
	// 第二次运行时 catalog 里已经有 2 条，seen snapshot 应该写出 2 个源视频 ID
	if res2.SeenSnapshot != 2 {
		t.Fatalf("second run SeenSnapshot = %d, want 2", res2.SeenSnapshot)
	}
	if len(newVideos) != 0 {
		t.Fatalf("second run OnNewVideo fired %d times, want 0", len(newVideos))
	}
}

// TestCrawlerRunOnceMissingScript 报错而不是 panic。
func TestCrawlerRunOnceMissingScript(t *testing.T) {
	tmp := t.TempDir()
	cat, err := catalog.Open(filepath.Join(tmp, "x.db"))
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	defer cat.Close()
	drv := New(Config{ID: "x", RootDir: filepath.Join(tmp, "x")})

	c := NewCrawler(CrawlerConfig{
		Driver:     drv,
		Catalog:    cat,
		PythonPath: "python3",
		ScriptPath: filepath.Join(tmp, "does-not-exist.py"),
	})

	if _, err := c.RunOnce(context.Background(), 1); err == nil {
		t.Fatalf("expected error for missing script")
	}
}

func TestCrawlerPassesProxyToSpiderProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-based fake script only on unix")
	}

	tmp := t.TempDir()
	scriptPath := filepath.Join(tmp, "print_proxy_env.sh")
	script := `#!/bin/sh
printf 'HTTP_PROXY=%s\n' "$HTTP_PROXY"
printf 'HTTPS_PROXY=%s\n' "$HTTPS_PROXY"
printf 'http_proxy=%s\n' "$http_proxy"
printf 'https_proxy=%s\n' "$https_proxy"
printf 'NO_PROXY=%s\n' "$NO_PROXY"
printf 'no_proxy=%s\n' "$no_proxy"
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	proxyURL := "socks5h://proxy.local:1080"
	drv := New(Config{ID: "proxy-drive", RootDir: filepath.Join(tmp, "proxy-drive")})
	c := NewCrawler(CrawlerConfig{
		Driver:     drv,
		PythonPath: "sh",
		ScriptPath: scriptPath,
		ProxyURL:   proxyURL,
	})
	cmd, stdout, err := c.startSpiderTargetNew(
		context.Background(),
		1,
		filepath.Join(tmp, "seen.txt"),
		filepath.Join(tmp, "out.json"),
	)
	if err != nil {
		t.Fatalf("startSpiderTargetNew: %v", err)
	}
	raw, err := io.ReadAll(stdout)
	if err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait: %v", err)
	}

	want := strings.Join([]string{
		"HTTP_PROXY=" + proxyURL,
		"HTTPS_PROXY=" + proxyURL,
		"http_proxy=" + proxyURL,
		"https_proxy=" + proxyURL,
		"NO_PROXY=",
		"no_proxy=",
	}, "\n") + "\n"
	if string(raw) != want {
		t.Fatalf("proxy env = %q, want %q", string(raw), want)
	}
}

func TestConfigureExplicitProxySupportsSocksSchemes(t *testing.T) {
	for _, raw := range []string{
		"socks5://127.0.0.1:1080",
		"socks5h://proxy-user:proxy-pass@127.0.0.1:1080",
	} {
		t.Run(raw, func(t *testing.T) {
			transport := &http.Transport{Proxy: http.ProxyFromEnvironment}
			if err := configureExplicitProxy(transport, raw); err != nil {
				t.Fatalf("configureExplicitProxy: %v", err)
			}
			if transport.Proxy != nil {
				t.Fatalf("Transport.Proxy should be nil for SOCKS proxy")
			}
			if transport.DialContext == nil {
				t.Fatalf("Transport.DialContext should be set for SOCKS proxy")
			}
		})
	}

	transport := &http.Transport{Proxy: http.ProxyFromEnvironment}
	if err := configureExplicitProxy(transport, "http://127.0.0.1:7890"); err != nil {
		t.Fatalf("configureExplicitProxy http: %v", err)
	}
	if transport.Proxy == nil {
		t.Fatalf("Transport.Proxy should be set for HTTP proxy")
	}
	if transport.DialContext != nil {
		t.Fatalf("Transport.DialContext should not be set for HTTP proxy")
	}

	if err := configureExplicitProxy(&http.Transport{}, "ftp://127.0.0.1:21"); err == nil {
		t.Fatalf("expected unsupported proxy scheme error")
	}
}

func TestSelectSocksTargetIPPrefersIPv4(t *testing.T) {
	got := selectSocksTargetIP([]net.IPAddr{
		{IP: net.ParseIP("2606:4700:20::681a:229")},
		{IP: net.ParseIP("104.26.3.41")},
	})
	if got == nil || got.String() != "104.26.3.41" {
		t.Fatalf("selectSocksTargetIP = %v, want IPv4 104.26.3.41", got)
	}
}

// TestCrawlerThumbDownloadFailureMarksStatusFailed 验证：网站封面下载失败时
// crawler 把 thumbnail_status 显式标 'failed'，避免后续封面补队列一直重复
// 捞到这条 spider91 视频。
//
// 历史 bug：之前 thumb 下载失败仅打 log，url=”, status 走 schema DEFAULT 'pending'。
// CountVideosNeedingThumbnail 条件是 url=” AND status != 'failed' → count=1。
// spider91 drive 的 thumb worker 按设计不处理 spider91 视频 → 没人会改 status，
// 后续补队列会一直认为它还缺封面。
func TestCrawlerThumbDownloadFailureMarksStatusFailed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-based fake script only on unix")
	}
	tmp := t.TempDir()

	// 假 HTTP 服务器：thumb 路径返回 500，video 正常返回字节。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "120101.mp4"):
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write([]byte("FAKEVIDEO"))
		case strings.Contains(r.URL.Path, "120101.jpg"):
			http.Error(w, "broken", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	videoEntries := []map[string]string{
		{
			"title":      "Thumb Failure Video",
			"thumb_url":  srv.URL + "/thumb/120101.jpg",
			"video_url":  srv.URL + "/videos/120101.mp4",
			"viewkey":    "vk-thumb-fail",
			"detail_url": srv.URL + "/v.php?viewkey=vk-thumb-fail",
		},
	}
	scriptPath := filepath.Join(tmp, "fake.sh")
	if err := os.WriteFile(scriptPath, []byte(buildFakeSpiderScript(videoEntries)), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	cat, err := catalog.Open(filepath.Join(tmp, "test.db"))
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	defer cat.Close()

	driveID := "thumbfail-drive"
	drv := New(Config{ID: driveID, RootDir: filepath.Join(tmp, "spider91", driveID)})
	if err := cat.UpsertDrive(context.Background(), &catalog.Drive{
		ID: driveID, Kind: Kind, Name: "thumbfail",
	}); err != nil {
		t.Fatalf("upsert drive: %v", err)
	}

	c := NewCrawler(CrawlerConfig{
		Driver:          drv,
		Catalog:         cat,
		PythonPath:      "sh",
		ScriptPath:      scriptPath,
		CommonThumbDir:  filepath.Join(tmp, "previews", "thumbs"),
		SpiderTimeout:   10 * time.Second,
		DownloadTimeout: 10 * time.Second,
	})

	res, err := c.RunOnce(context.Background(), 5)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.NewVideos != 1 {
		t.Fatalf("expected 1 new video, got %d (failed=%d)", res.NewVideos, res.Failed)
	}

	got, err := cat.GetVideo(context.Background(), "spider91-"+driveID+"-120101")
	if err != nil {
		t.Fatalf("get video: %v", err)
	}
	if got.ThumbnailURL != "" {
		t.Errorf("ThumbnailURL = %q, want empty (download failed)", got.ThumbnailURL)
	}

	// 关键断言：CountVideosNeedingThumbnail 应该返回 0。
	// 该函数的 SQL 条件是 `url = '' AND status != 'failed'`；如果 crawler 没把
	// status 标 'failed'（schema DEFAULT 'pending'），count 就会是 1。
	count, err := cat.CountVideosNeedingThumbnail(context.Background(), driveID)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("CountVideosNeedingThumbnail = %d, want 0 (status should be 'failed' to unblock teaser worker)", count)
	}
}

func TestCrawlerUsesCrawlerVideoURLForFirstDownload(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-based fake script only on unix")
	}
	tmp := t.TempDir()

	var detailRequests int32
	var originalRequests int32
	var wrongRequests int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v.php":
			atomic.AddInt32(&detailRequests, 1)
			_, _ = w.Write([]byte(spider91DetailHTML(srv.URL + "/videos/856305.mp4?token=wrong")))
		case r.URL.Path == "/videos/120201.mp4" && r.URL.Query().Get("token") == "original":
			atomic.AddInt32(&originalRequests, 1)
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write([]byte("ORIGINALVIDEO"))
		case r.URL.Path == "/videos/856305.mp4":
			atomic.AddInt32(&wrongRequests, 1)
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write([]byte("WRONGVIDEO"))
		case r.URL.Path == "/thumb/120201.jpg":
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write([]byte("\xff\xd8\xff\xe0thumb"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	entry := map[string]string{
		"title":      "Use Original URL First",
		"thumb_url":  srv.URL + "/thumb/wrong-thumb.jpg",
		"video_url":  srv.URL + "/videos/120201.mp4?token=original",
		"viewkey":    "vk-use-original",
		"detail_url": srv.URL + "/v.php?viewkey=vk-use-original",
	}
	cat, drv, scriptPath := seedCrawlerTestDeps(t, tmp, "use-original-drive", []map[string]string{entry})
	c := NewCrawler(CrawlerConfig{
		Driver:          drv,
		Catalog:         cat,
		PythonPath:      "sh",
		ScriptPath:      scriptPath,
		CommonThumbDir:  filepath.Join(tmp, "previews", "thumbs"),
		SpiderTimeout:   10 * time.Second,
		DownloadTimeout: 10 * time.Second,
	})

	res, err := c.RunOnce(context.Background(), 1)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.NewVideos != 1 || res.Failed != 0 {
		t.Fatalf("result new=%d failed=%d, want 1/0", res.NewVideos, res.Failed)
	}
	if got := atomic.LoadInt32(&detailRequests); got != 0 {
		t.Fatalf("detail requests = %d, want 0 (first download should use crawler URL)", got)
	}
	if got := atomic.LoadInt32(&originalRequests); got != 1 {
		t.Fatalf("original URL requests = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&wrongRequests); got != 0 {
		t.Fatalf("wrong source URL requests = %d, want 0", got)
	}
	info, err := os.Stat(filepath.Join(drv.RootDir(), "videos", "120201.mp4"))
	if err != nil {
		t.Fatalf("original video missing: %v", err)
	}
	if info.Size() != int64(len("ORIGINALVIDEO")) {
		t.Fatalf("original video size = %d, want %d", info.Size(), len("ORIGINALVIDEO"))
	}
}

func TestCrawlerRefreshesVideoURLAfterExpiredDownload(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-based fake script only on unix")
	}
	tmp := t.TempDir()

	var detailRequests int32
	var staleRequests int32
	var freshRequests int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v.php":
			n := atomic.AddInt32(&detailRequests, 1)
			videoURL := srv.URL + "/videos/120202.mp4?token=stale"
			if n > 1 {
				videoURL = srv.URL + "/videos/120202.mp4?token=fresh"
			}
			_, _ = w.Write([]byte(spider91DetailHTML(videoURL)))
		case r.URL.Path == "/videos/120202.mp4" && r.URL.Query().Get("token") == "stale":
			atomic.AddInt32(&staleRequests, 1)
			http.Error(w, "expired", http.StatusForbidden)
		case r.URL.Path == "/videos/120202.mp4" && r.URL.Query().Get("token") == "fresh":
			atomic.AddInt32(&freshRequests, 1)
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write([]byte("REFRESHEDVIDEO"))
		case r.URL.Path == "/thumb/120202.jpg":
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write([]byte("\xff\xd8\xff\xe0thumb"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	entry := map[string]string{
		"title":      "Refresh After Expired Download",
		"thumb_url":  srv.URL + "/thumb/wrong-thumb.jpg",
		"video_url":  srv.URL + "/videos/120202.mp4?token=old",
		"viewkey":    "vk-refresh-after",
		"detail_url": srv.URL + "/v.php?viewkey=vk-refresh-after",
	}
	cat, drv, scriptPath := seedCrawlerTestDeps(t, tmp, "refresh-after-drive", []map[string]string{entry})
	c := NewCrawler(CrawlerConfig{
		Driver:          drv,
		Catalog:         cat,
		PythonPath:      "sh",
		ScriptPath:      scriptPath,
		CommonThumbDir:  filepath.Join(tmp, "previews", "thumbs"),
		SpiderTimeout:   10 * time.Second,
		DownloadTimeout: 10 * time.Second,
	})

	res, err := c.RunOnce(context.Background(), 1)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.NewVideos != 1 || res.Failed != 0 {
		t.Fatalf("result new=%d failed=%d, want 1/0", res.NewVideos, res.Failed)
	}
	if got := atomic.LoadInt32(&detailRequests); got < 2 {
		t.Fatalf("detail requests = %d, want at least 2 (initial refresh + retry refresh)", got)
	}
	if got := atomic.LoadInt32(&staleRequests); got != 1 {
		t.Fatalf("stale URL requests = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&freshRequests); got != 1 {
		t.Fatalf("fresh URL requests = %d, want 1", got)
	}
	info, err := os.Stat(filepath.Join(drv.RootDir(), "videos", "120202.mp4"))
	if err != nil {
		t.Fatalf("refreshed video missing: %v", err)
	}
	if info.Size() != int64(len("REFRESHEDVIDEO")) {
		t.Fatalf("refreshed video size = %d, want %d", info.Size(), len("REFRESHEDVIDEO"))
	}
}

func TestCrawlerRejectsRefreshedSourceIDMismatch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-based fake script only on unix")
	}
	tmp := t.TempDir()

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v.php":
			_, _ = w.Write([]byte(spider91DetailHTML(srv.URL + "/videos/856305.mp4?token=fresh")))
		case r.URL.Path == "/videos/1203058.mp4":
			http.Error(w, "expired", http.StatusForbidden)
		case r.URL.Path == "/videos/856305.mp4":
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write([]byte("WRONGVIDEO"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	entry := map[string]string{
		"title":      "Source ID Mismatch",
		"thumb_url":  srv.URL + "/thumb/1203058.jpg",
		"video_url":  srv.URL + "/videos/1203058.mp4?token=old",
		"viewkey":    "86fd91cce1f2e1a154cc",
		"source_id":  "1203058",
		"detail_url": srv.URL + "/v.php?viewkey=86fd91cce1f2e1a154cc",
	}
	cat, drv, scriptPath := seedCrawlerTestDeps(t, tmp, "mismatch-drive", []map[string]string{entry})
	c := NewCrawler(CrawlerConfig{
		Driver:          drv,
		Catalog:         cat,
		PythonPath:      "sh",
		ScriptPath:      scriptPath,
		CommonThumbDir:  filepath.Join(tmp, "previews", "thumbs"),
		SpiderTimeout:   10 * time.Second,
		DownloadTimeout: 10 * time.Second,
	})

	res, err := c.RunOnce(context.Background(), 1)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.NewVideos != 0 || res.Failed != 1 {
		t.Fatalf("result new=%d failed=%d, want 0/1", res.NewVideos, res.Failed)
	}
	if _, err := os.Stat(filepath.Join(drv.RootDir(), "videos", "1203058.mp4")); !os.IsNotExist(err) {
		t.Fatalf("mismatched source file should not be written, stat err=%v", err)
	}
	if v, _ := cat.GetVideo(context.Background(), BuildVideoID(drv.ID(), "1203058")); v != nil {
		t.Fatalf("mismatched video should not be inserted: %+v", v)
	}
}

func TestSourceIDForItemRequiresNumericSourceID(t *testing.T) {
	if got := sourceIDForItem(spiderVideoEntry{
		Viewkey:  "86fd91cce1f2e1a154cc",
		VideoURL: "https://cdn.example/videos/1203058.mp4?token=x",
	}); got != "1203058" {
		t.Fatalf("sourceIDForItem(video url) = %q, want 1203058", got)
	}
	if got := sourceIDForItem(spiderVideoEntry{
		Viewkey:  "86fd91cce1f2e1a154cc",
		ThumbURL: "https://img.example/thumb/1203058.jpg",
	}); got != "1203058" {
		t.Fatalf("sourceIDForItem(thumb url) = %q, want 1203058", got)
	}
	if got := sourceIDForItem(spiderVideoEntry{
		Viewkey:  "86fd91cce1f2e1a154cc",
		SourceID: "not-numeric",
		VideoURL: "https://cdn.example/videos/video.mp4",
	}); got != "" {
		t.Fatalf("sourceIDForItem(non numeric) = %q, want empty", got)
	}
}

func TestNormalizeThumbURLForSource(t *testing.T) {
	got := normalizeThumbURLForSource("https://img.example/thumb/856305.jpg?x=1#frag", "1203058")
	want := "https://img.example/thumb/1203058.jpg"
	if got != want {
		t.Fatalf("normalizeThumbURLForSource = %q, want %q", got, want)
	}
}

func TestSpider91ListURLForDetail(t *testing.T) {
	got := spider91ListURLForDetail("https://www.91porn.com/view_video.php?viewkey=abc&page=5&c=furum&viewtype=basic&category=top")
	want := "https://www.91porn.com/v.php?category=top&page=5&viewtype=basic"
	if got != want {
		t.Fatalf("spider91ListURLForDetail = %q, want %q", got, want)
	}
	if got := spider91ListURLForDetail("http://127.0.0.1/v.php?viewkey=abc&page=5&viewtype=basic&category=top"); got != "" {
		t.Fatalf("spider91ListURLForDetail(localhost) = %q, want empty", got)
	}
}

func TestSpider91CookieHeader(t *testing.T) {
	got := spider91CookieHeader([]*http.Cookie{
		{Name: "CLIPSHARE", Value: "abc"},
		{Name: "ga", Value: "def"},
		{Name: "mode", Value: "m"},
	})
	want := "mode=d; CLIPSHARE=abc; ga=def"
	if got != want {
		t.Fatalf("spider91CookieHeader = %q, want %q", got, want)
	}
}

func spider91DetailHTML(videoURL string) string {
	fragment := `<video><source src="` + videoURL + `" type="video/mp4"></video>`
	return `document.write(strencode2("` + url.PathEscape(fragment) + `"));`
}

func seedCrawlerTestDeps(t *testing.T, tmp, driveID string, entries []map[string]string) (*catalog.Catalog, *Driver, string) {
	t.Helper()
	scriptPath := filepath.Join(tmp, driveID+"-fake.sh")
	if err := os.WriteFile(scriptPath, []byte(buildFakeSpiderScript(entries)), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	cat, err := catalog.Open(filepath.Join(tmp, driveID+".db"))
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})
	drv := New(Config{ID: driveID, RootDir: filepath.Join(tmp, "spider91", driveID)})
	if err := cat.UpsertDrive(context.Background(), &catalog.Drive{
		ID: driveID, Kind: Kind, Name: driveID,
	}); err != nil {
		t.Fatalf("upsert drive: %v", err)
	}
	return cat, drv, scriptPath
}

// buildFakeSpiderScript 生成一个伪 python 脚本（其实是 sh）。
//
// 行为：
//   - 解析 --output FILE / --stream-output 两个 flag
//   - --stream-output 时：逐行输出每个 entry 的 JSON 到 stdout 并 flush
//   - --output 时：把完整 JSON 数据写到 FILE（向后兼容，且作归档）
//
// 用 sh 来写是为了避免 Python 依赖。每条 entry 的 JSON 用 Go marshal 出来后嵌入。
func buildFakeSpiderScript(entries []map[string]string) string {
	var sb strings.Builder
	sb.WriteString("#!/bin/sh\n")
	sb.WriteString("out=\"\"; stream=0\n")
	sb.WriteString("while [ $# -gt 0 ]; do case \"$1\" in --output) out=\"$2\"; shift 2;; --stream-output) stream=1; shift;; *) shift;; esac; done\n")

	// stream 模式：逐行 echo
	sb.WriteString("if [ \"$stream\" = \"1\" ]; then\n")
	for _, e := range entries {
		raw, _ := json.Marshal(e)
		// 用单引号 here-string 形式确保 JSON 中的双引号原样出来
		sb.WriteString("  cat <<'STREAM_EOF'\n")
		sb.Write(raw)
		sb.WriteString("\nSTREAM_EOF\n")
	}
	sb.WriteString("fi\n")

	// 写 --output 文件（带完整 wrapper）
	sb.WriteString("if [ -n \"$out\" ]; then\n")
	sb.WriteString("  mkdir -p \"$(dirname \"$out\")\" 2>/dev/null\n")
	sb.WriteString("  cat > \"$out\" <<'OUT_EOF'\n")
	wrapper := map[string]any{
		"crawl_time":   "2026-01-01T00:00:00",
		"total_videos": len(entries),
		"videos":       entries,
	}
	wrapped, _ := json.MarshalIndent(wrapper, "", "  ")
	sb.Write(wrapped)
	sb.WriteString("\nOUT_EOF\n")
	sb.WriteString("fi\n")
	return sb.String()
}
