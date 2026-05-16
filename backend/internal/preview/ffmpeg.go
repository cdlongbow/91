package preview

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/video-site/backend/internal/catalog"
	"github.com/video-site/backend/internal/drives"
)

type Config struct {
	FFmpegPath      string
	FFprobePath     string
	DurationSeconds int // 兼容旧配置；当前 teaser 每段固定 3 秒
	Width           int
	Segments        int    // 兼容旧配置；当前 30 秒及以上视频固定使用 4 段
	LocalDir        string // 本地 teaser 和封面目录
	RemoteDir       string // Deprecated: 保留配置兼容性；teaser 不再回写网盘
}

type Generator struct {
	cfg Config
}

type ThumbnailGenerator interface {
	Probe(ctx context.Context, link *drives.StreamLink) (float64, error)
	GenerateThumbnail(ctx context.Context, link *drives.StreamLink, videoID string, duration float64) (string, error)
}

type TeaserGenerator interface {
	Probe(ctx context.Context, link *drives.StreamLink) (float64, error)
	Generate(ctx context.Context, link *drives.StreamLink, duration float64) (string, error)
	MoveToLocal(tmpPath, videoID string) (string, error)
}

type refreshingTeaserGenerator interface {
	GenerateWithLinkProvider(ctx context.Context, first *drives.StreamLink, duration float64, refresh func(context.Context) (*drives.StreamLink, error)) (string, error)
}

func New(cfg Config) *Generator {
	if cfg.FFmpegPath == "" {
		cfg.FFmpegPath = "ffmpeg"
	}
	if cfg.FFprobePath == "" {
		cfg.FFprobePath = "ffprobe"
	}
	if cfg.DurationSeconds != 3 {
		cfg.DurationSeconds = 3
	}
	if cfg.Width == 0 {
		cfg.Width = 480
	}
	if cfg.Segments <= 0 {
		cfg.Segments = 3
	}
	return &Generator{cfg: cfg}
}

// --- 选段策略 ---

type teaserPlan struct {
	starts  []float64
	eachSec float64
}

func buildTeaserPlan(cfg Config, duration float64) teaserPlan {
	if cfg.DurationSeconds != 3 {
		cfg.DurationSeconds = 3
	}
	if cfg.Segments <= 0 {
		cfg.Segments = 3
	}

	segs := 1
	if duration > 0 && duration < 30 {
		segs = 3
	} else if duration >= 30 {
		segs = 4
	}

	eachSec := 3.0
	if duration > 0 && duration < eachSec {
		eachSec = duration
	}

	return teaserPlan{
		starts:  pickSegmentStarts(duration, segs, eachSec),
		eachSec: eachSec,
	}
}

// pickSegmentStarts 根据视频总时长选出 N 段起点秒数（按时间升序）
//
// 规则：
//   - duration < 30s → 最多 3 段；不足 3 秒时用完整短视频作为单段
//   - 30s ≤ duration < 10min → 4 段：前段跳过片头、末段避开片尾
//   - duration ≥ 10min → 固定 4 段，按 20% ~ 80% 等距分布
func pickSegmentStarts(duration float64, n int, eachSec float64) []float64 {
	if n <= 0 {
		n = 1
	}
	if duration <= 0 {
		// 未知时长，用保守默认
		return []float64{10}
	}
	if duration < 30 {
		completeSegments := int(math.Floor(duration / eachSec))
		if completeSegments > n {
			completeSegments = n
		}
		if completeSegments <= 0 {
			return nil
		}
		usable := duration - eachSec
		first := math.Min(duration*0.1, usable)
		if completeSegments == 1 {
			return []float64{math.Max(0, first)}
		}
		starts := make([]float64, 0, completeSegments)
		step := (usable - first) / float64(completeSegments-1)
		for i := 0; i < completeSegments; i++ {
			starts = append(starts, first+step*float64(i))
		}
		return starts
	}

	// 余量：保证最后一段结束前留 1 秒，避免切到文件末尾
	usable := duration - eachSec - 1
	if usable < 0 {
		usable = 0
	}

	if duration < 600 {
		// 30s ~ 10min：20% 起，均匀分段
		starts := make([]float64, 0, n)
		// 保证第一段跳过片头（>= 5% 或 3s）
		firstMin := math.Max(3, duration*0.05)
		// 最后一段结束 <= 85%，避开结尾
		lastMax := duration * 0.85
		if lastMax < firstMin {
			lastMax = firstMin
		}
		if n == 1 {
			return []float64{duration * 0.25}
		}
		step := (lastMax - firstMin) / float64(n-1)
		for i := 0; i < n; i++ {
			s := firstMin + step*float64(i)
			if s > usable {
				s = usable
			}
			starts = append(starts, s)
		}
		return starts
	}

	// 长视频：按 20% / 50% / 80% 布置
	if n == 1 {
		return []float64{duration * 0.3}
	}
	starts := make([]float64, 0, n)
	pct := make([]float64, 0, n)
	// 均匀在 [0.2, 0.8] 区间取 N 个点
	lo, hi := 0.2, 0.8
	if n == 1 {
		pct = append(pct, 0.3)
	} else {
		step := (hi - lo) / float64(n-1)
		for i := 0; i < n; i++ {
			pct = append(pct, lo+step*float64(i))
		}
	}
	for _, p := range pct {
		s := duration * p
		if s > usable {
			s = usable
		}
		starts = append(starts, s)
	}
	return starts
}

// pickThumbnailOffset 选封面抽帧的时间点（秒）。独立于 teaser。
func pickThumbnailOffset(duration float64) float64 {
	if duration <= 0 {
		return 5
	}
	// 短视频从 30% 抽；长视频从 20% 抽，避开片头
	if duration < 60 {
		return math.Max(1, duration*0.3)
	}
	return math.Max(5, math.Min(duration*0.2, 120))
}

// --- 封面 ---

// GenerateThumbnail 抽一张 jpg 封面。偏移点由 duration 决定（独立于 teaser）。
func (g *Generator) GenerateThumbnail(ctx context.Context, link *drives.StreamLink, videoID string, duration float64) (string, error) {
	dir := filepath.Join(g.cfg.LocalDir, "thumbs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	dst := filepath.Join(dir, videoID+".jpg")
	offset := pickThumbnailOffset(duration)

	ctx2, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	ffmpegLink, cleanup, err := prepareFFmpegLink(ctx2, link)
	if err != nil {
		return "", err
	}
	defer cleanup()

	args := []string{
		"-hide_banner",
		"-loglevel", "error",
		"-ss", fmt.Sprintf("%.2f", offset),
	}
	args = append(args, ffmpegHTTPInputOptions(ffmpegLink)...)
	args = append(args,
		"-i", ffmpegLink.URL,
		"-frames:v", "1",
		"-vf", fmt.Sprintf("scale=%d:-2", g.cfg.Width),
		"-q:v", "3",
		"-y", dst,
	)

	cmd := exec.CommandContext(ctx2, g.cfg.FFmpegPath, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		os.Remove(dst)
		return "", ffmpegCommandError("ffmpeg thumb", err, out)
	}
	if info, statErr := os.Stat(dst); statErr != nil || info.Size() == 0 {
		os.Remove(dst)
		return "", fmt.Errorf("ffmpeg thumb produced empty file, stderr: %s", string(out))
	}
	return dst, nil
}

// --- 时长 ---

// Probe 用 ffprobe 拿视频时长（秒，浮点）
func (g *Generator) Probe(ctx context.Context, link *drives.StreamLink) (float64, error) {
	ctx2, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ffmpegLink, cleanup, err := prepareFFmpegLink(ctx2, link)
	if err != nil {
		return 0, err
	}
	defer cleanup()

	args := []string{
		"-hide_banner",
		"-loglevel", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
	}
	args = append(args, ffmpegHTTPInputOptions(ffmpegLink)...)
	args = append(args, ffmpegLink.URL)

	cmd := exec.CommandContext(ctx2, g.cfg.FFprobePath, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return 0, ffmpegCommandError("ffprobe", err, out)
	}
	raw := strings.TrimSpace(string(out))
	if raw == "" || raw == "N/A" {
		return 0, nil
	}
	return strconv.ParseFloat(raw, 64)
}

// --- Teaser ---

// Generate 拉取 teaser 到本地临时文件，返回路径。
// 根据 Config.Segments 和视频时长决定是单段还是多段拼接。
func (g *Generator) Generate(ctx context.Context, link *drives.StreamLink, duration float64) (string, error) {
	return g.generate(ctx, duration, func(int) (*drives.StreamLink, error) {
		return link, nil
	})
}

func (g *Generator) GenerateWithLinkProvider(ctx context.Context, first *drives.StreamLink, duration float64, refresh func(context.Context) (*drives.StreamLink, error)) (string, error) {
	return g.generateSequential(ctx, duration, func(index int) (*drives.StreamLink, error) {
		if index == 0 || refresh == nil {
			return first, nil
		}
		return refresh(ctx)
	})
}

func (g *Generator) generate(ctx context.Context, duration float64, linkForInput func(int) (*drives.StreamLink, error)) (string, error) {
	if err := os.MkdirAll(g.cfg.LocalDir, 0o755); err != nil {
		return "", err
	}

	plan := buildTeaserPlan(g.cfg, duration)
	starts := plan.starts
	eachSec := plan.eachSec
	if len(starts) == 0 {
		return "", fmt.Errorf("video too short for %.0fs teaser segment", eachSec)
	}

	ctx2, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()

	// 用 ffmpeg 的 concat 滤镜一次输出：多个 -ss input 再 concat + fade
	tmp, err := os.CreateTemp(g.cfg.LocalDir, "teaser-*.mp4")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	tmp.Close()

	args := []string{
		"-hide_banner",
		"-loglevel", "error",
	}

	// 每段独立 -ss + -i，精确 seek 重新解码保证拼接帧准
	var cleanups []func()
	defer func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}()
	for i, s := range starts {
		link, err := linkForInput(i)
		if err != nil {
			os.Remove(tmpPath)
			return "", err
		}
		ffmpegLink, cleanup, err := prepareFFmpegLink(ctx2, link)
		if err != nil {
			os.Remove(tmpPath)
			return "", err
		}
		cleanups = append(cleanups, cleanup)
		args = append(args, ffmpegHTTPInputOptions(ffmpegLink)...)
		args = append(args,
			"-ss", fmt.Sprintf("%.2f", s),
			"-t", fmt.Sprintf("%.2f", eachSec),
			"-i", ffmpegLink.URL,
		)
	}

	if len(starts) == 1 {
		// 单段：无需 concat，直接缩放 + 无音
		args = append(args,
			"-an",
			"-vf", fmt.Sprintf("scale=%d:-2,setsar=1", g.cfg.Width),
			"-c:v", "libx264",
			"-preset", "veryfast",
			"-crf", "28",
			"-movflags", "+faststart",
			"-y", tmpPath,
		)
	} else {
		// 多段：各段缩放 + 0.2s 黑场淡入淡出，concat 拼接
		// filter_complex: [0:v]scale,fade=in:0:5,fade=out:start=eachSec-0.2:d=0.2[v0]; ...; [v0][v1][v2]concat=n=3:v=1:a=0[v]
		fadeIn := 0.2
		fadeOutStart := eachSec - 0.2
		if fadeOutStart < 0 {
			fadeOutStart = 0
		}
		var filter strings.Builder
		for i := range starts {
			if i > 0 {
				filter.WriteString(";")
			}
			fmt.Fprintf(&filter,
				"[%d:v]scale=%d:-2,setsar=1,fade=t=in:st=0:d=%.2f,fade=t=out:st=%.2f:d=0.2[v%d]",
				i, g.cfg.Width, fadeIn, fadeOutStart, i)
		}
		filter.WriteString(";")
		for i := range starts {
			fmt.Fprintf(&filter, "[v%d]", i)
		}
		fmt.Fprintf(&filter, "concat=n=%d:v=1:a=0[v]", len(starts))

		args = append(args,
			"-filter_complex", filter.String(),
			"-map", "[v]",
			"-an",
			"-c:v", "libx264",
			"-preset", "veryfast",
			"-crf", "28",
			"-movflags", "+faststart",
			"-y", tmpPath,
		)
	}

	cmd := exec.CommandContext(ctx2, g.cfg.FFmpegPath, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		os.Remove(tmpPath)
		return "", ffmpegCommandError("ffmpeg", err, out)
	}

	if info, statErr := os.Stat(tmpPath); statErr != nil || info.Size() == 0 {
		os.Remove(tmpPath)
		return "", fmt.Errorf("ffmpeg produced empty file, stderr: %s", string(out))
	}
	if err := g.validateGeneratedTeaser(ctx2, tmpPath); err != nil {
		os.Remove(tmpPath)
		return "", err
	}
	return tmpPath, nil
}

func (g *Generator) generateSequential(ctx context.Context, duration float64, linkForInput func(int) (*drives.StreamLink, error)) (string, error) {
	if err := os.MkdirAll(g.cfg.LocalDir, 0o755); err != nil {
		return "", err
	}

	plan := buildTeaserPlan(g.cfg, duration)
	starts := plan.starts
	eachSec := plan.eachSec
	if len(starts) == 0 {
		return "", fmt.Errorf("video too short for %.0fs teaser segment", eachSec)
	}

	ctx2, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()

	segmentPaths := make([]string, 0, len(starts))
	success := false
	defer func() {
		if success {
			return
		}
		for _, p := range segmentPaths {
			_ = os.Remove(p)
		}
	}()

	for i, start := range starts {
		seg, err := g.generateSingleSegment(ctx2, i, start, eachSec, linkForInput)
		if err != nil {
			return "", err
		}
		segmentPaths = append(segmentPaths, seg)
	}

	if len(segmentPaths) == 1 {
		success = true
		return segmentPaths[0], nil
	}

	tmp, err := os.CreateTemp(g.cfg.LocalDir, "teaser-*.mp4")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	tmp.Close()
	_ = os.Remove(tmpPath)

	list, err := os.CreateTemp(g.cfg.LocalDir, "teaser-concat-*.txt")
	if err != nil {
		return "", err
	}
	listPath := list.Name()
	for _, p := range segmentPaths {
		if _, err := fmt.Fprintf(list, "file '%s'\n", escapeConcatPath(p)); err != nil {
			list.Close()
			_ = os.Remove(listPath)
			return "", err
		}
	}
	if err := list.Close(); err != nil {
		_ = os.Remove(listPath)
		return "", err
	}
	defer os.Remove(listPath)

	args := []string{
		"-hide_banner",
		"-loglevel", "error",
		"-f", "concat",
		"-safe", "0",
		"-i", listPath,
		"-c", "copy",
		"-movflags", "+faststart",
		"-y", tmpPath,
	}
	out, err := exec.CommandContext(ctx2, g.cfg.FFmpegPath, args...).CombinedOutput()
	if err != nil {
		_ = os.Remove(tmpPath)
		return "", ffmpegCommandError("ffmpeg concat", err, out)
	}
	if info, statErr := os.Stat(tmpPath); statErr != nil || info.Size() == 0 {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("ffmpeg concat produced empty file, stderr: %s", string(out))
	}
	if err := g.validateGeneratedTeaser(ctx2, tmpPath); err != nil {
		_ = os.Remove(tmpPath)
		return "", err
	}

	for _, p := range segmentPaths {
		_ = os.Remove(p)
	}
	success = true
	return tmpPath, nil
}

func (g *Generator) generateSingleSegment(ctx context.Context, index int, start, eachSec float64, linkForInput func(int) (*drives.StreamLink, error)) (string, error) {
	link, err := linkForInput(index)
	if err != nil {
		return "", err
	}
	ffmpegLink, cleanup, err := prepareFFmpegLink(ctx, link)
	if err != nil {
		return "", err
	}
	defer cleanup()

	seg, err := os.CreateTemp(g.cfg.LocalDir, fmt.Sprintf("teaser-seg-%d-*.mp4", index))
	if err != nil {
		return "", err
	}
	segPath := seg.Name()
	seg.Close()

	fadeIn := 0.2
	fadeOutStart := eachSec - 0.2
	if fadeOutStart < 0 {
		fadeOutStart = 0
	}
	filter := fmt.Sprintf("scale=%d:-2,setsar=1,fade=t=in:st=0:d=%.2f,fade=t=out:st=%.2f:d=0.2", g.cfg.Width, fadeIn, fadeOutStart)

	args := []string{
		"-hide_banner",
		"-loglevel", "error",
	}
	args = append(args, ffmpegHTTPInputOptions(ffmpegLink)...)
	args = append(args,
		"-ss", fmt.Sprintf("%.2f", start),
		"-t", fmt.Sprintf("%.2f", eachSec),
		"-i", ffmpegLink.URL,
		"-an",
		"-vf", filter,
		"-c:v", "libx264",
		"-preset", "veryfast",
		"-crf", "28",
		"-movflags", "+faststart",
		"-y", segPath,
	)
	out, err := exec.CommandContext(ctx, g.cfg.FFmpegPath, args...).CombinedOutput()
	if err != nil {
		_ = os.Remove(segPath)
		return "", ffmpegCommandError("ffmpeg segment", err, out)
	}
	if info, statErr := os.Stat(segPath); statErr != nil || info.Size() == 0 {
		_ = os.Remove(segPath)
		return "", fmt.Errorf("ffmpeg segment produced empty file, stderr: %s", string(out))
	}
	if err := g.validateGeneratedTeaser(ctx, segPath); err != nil {
		_ = os.Remove(segPath)
		return "", err
	}
	return segPath, nil
}

type localMediaProbe struct {
	Streams []struct {
		CodecType string `json:"codec_type"`
		Duration  string `json:"duration"`
	} `json:"streams"`
	Format struct {
		Duration string `json:"duration"`
	} `json:"format"`
}

func (g *Generator) validateGeneratedTeaser(ctx context.Context, path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Size() == 0 {
		return errors.New("generated teaser is empty")
	}

	ctx2, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	args := []string{
		"-v", "error",
		"-show_entries", "stream=codec_type,duration:format=duration",
		"-of", "json",
		path,
	}
	out, err := exec.CommandContext(ctx2, g.cfg.FFprobePath, args...).CombinedOutput()
	if err != nil {
		return ffmpegCommandError("ffprobe teaser", err, out)
	}

	var probe localMediaProbe
	if err := json.Unmarshal(out, &probe); err != nil {
		return fmt.Errorf("ffprobe teaser output: %w", err)
	}

	duration := parseProbeDuration(probe.Format.Duration)
	hasVideo := false
	for _, stream := range probe.Streams {
		if stream.CodecType == "video" {
			hasVideo = true
		}
		if d := parseProbeDuration(stream.Duration); d > duration {
			duration = d
		}
	}
	if !hasVideo {
		return errors.New("generated teaser has no video stream")
	}
	if duration <= 0.01 {
		return fmt.Errorf("generated teaser has invalid duration %.3fs", duration)
	}
	return nil
}

func parseProbeDuration(raw string) float64 {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "N/A" {
		return 0
	}
	d, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0
	}
	return d
}

func escapeConcatPath(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	return strings.ReplaceAll(path, "'", "'\\''")
}

func prepareFFmpegLink(ctx context.Context, link *drives.StreamLink) (*drives.StreamLink, func(), error) {
	if link == nil {
		return nil, func() {}, errors.New("missing stream link")
	}
	if !shouldProxyFFmpegLink(link) {
		return link, func() {}, nil
	}
	return startLocalFFmpegProxy(ctx, link)
}

func shouldProxyFFmpegLink(link *drives.StreamLink) bool {
	if link == nil {
		return false
	}
	raw := strings.ToLower(link.URL)
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		return false
	}
	if strings.Contains(raw, "115cdn") {
		return true
	}
	return strings.Contains(strings.ToLower(link.Headers.Get("User-Agent")), "115")
}

func startLocalFFmpegProxy(ctx context.Context, link *drives.StreamLink) (*drives.StreamLink, func(), error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, nil, err
	}
	client := &http.Client{Timeout: 0}
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/stream" {
				http.NotFound(w, r)
				return
			}
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			req, err := http.NewRequestWithContext(r.Context(), r.Method, link.URL, nil)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			for k, vs := range link.Headers {
				for _, v := range vs {
					req.Header.Add(k, v)
				}
			}
			if rng := r.Header.Get("Range"); rng != "" {
				req.Header.Set("Range", rng)
			}

			resp, err := client.Do(req)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			defer resp.Body.Close()

			for _, k := range []string{
				"Content-Type", "Content-Length", "Content-Range",
				"Accept-Ranges", "Last-Modified", "Etag",
			} {
				if v := resp.Header.Get(k); v != "" {
					w.Header().Set(k, v)
				}
			}
			w.WriteHeader(resp.StatusCode)
			if r.Method != http.MethodHead {
				_, _ = io.Copy(w, resp.Body)
			}
		}),
	}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("[preview] local ffmpeg proxy: %v", err)
		}
	}()

	var once sync.Once
	cleanup := func() {
		once.Do(func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = srv.Shutdown(shutdownCtx)
		})
	}
	go func() {
		<-ctx.Done()
		cleanup()
	}()

	proxied := *link
	proxied.URL = "http://" + ln.Addr().String() + "/stream"
	proxied.Headers = nil
	return &proxied, cleanup, nil
}

func ffmpegHTTPInputOptions(link *drives.StreamLink) []string {
	if link == nil {
		return nil
	}
	var args []string
	if ua := strings.TrimSpace(link.Headers.Get("User-Agent")); ua != "" {
		args = append(args, "-user_agent", ua)
	}
	if h := buildHeaders(link.Headers); h != "" {
		args = append(args, "-headers", h)
	}
	return args
}

func ffmpegCommandError(tool string, err error, output []byte) error {
	msg := fmt.Sprintf("%s: %v, stderr: %s", tool, err, redactURLs(string(output)))
	wrapped := errors.New(msg)
	if ffmpegOutputLooksRateLimited(output) {
		return &drives.RateLimitError{
			Provider: "media source",
			Err:      wrapped,
		}
	}
	return wrapped
}

func redactURLs(text string) string {
	fields := strings.Fields(text)
	for i, field := range fields {
		if strings.HasPrefix(field, "http://") || strings.HasPrefix(field, "https://") {
			suffix := ""
			for len(field) > 0 {
				last := field[len(field)-1]
				if last != '.' && last != ',' && last != ';' && last != ')' {
					break
				}
				suffix = string(last) + suffix
				field = field[:len(field)-1]
			}
			fields[i] = "https://<redacted>" + suffix
		}
	}
	return strings.Join(fields, " ")
}

func ffmpegOutputLooksRateLimited(output []byte) bool {
	text := strings.ToLower(string(output))
	if !strings.Contains(text, "429") {
		return false
	}
	return strings.Contains(text, "too many requests") ||
		strings.Contains(text, "rate limit") ||
		strings.Contains(text, "rate-limit") ||
		strings.Contains(text, "server returned 429")
}

// --- 本地落盘 ---

// MoveToLocal 把临时文件改名到稳定位置，返回最终路径
func (g *Generator) MoveToLocal(tmpPath, videoID string) (string, error) {
	dst := filepath.Join(g.cfg.LocalDir, videoID+".mp4")
	if err := os.Rename(tmpPath, dst); err != nil {
		// 跨盘 rename 可能失败，fallback 到 copy
		if cerr := copyFile(tmpPath, dst); cerr != nil {
			return "", cerr
		}
		_ = os.Remove(tmpPath)
	}
	return dst, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// --- Worker ---

type Worker struct {
	Gen     TeaserGenerator
	Catalog *catalog.Catalog
	Drive   drives.Drive
	ch      chan *catalog.Video

	RateLimitCooldown time.Duration
	BeforeTask        func(context.Context) bool
	rateLimit         rateLimitState
	activity          taskActivity
}

func NewWorker(gen TeaserGenerator, cat *catalog.Catalog, drv drives.Drive, _ string) *Worker {
	return &Worker{
		Gen:     gen,
		Catalog: cat,
		Drive:   drv,
		ch:      make(chan *catalog.Video, 4096),
	}
}

func (w *Worker) Enqueue(v *catalog.Video) bool {
	if v == nil {
		return false
	}
	select {
	case w.ch <- v:
		return true
	default:
		return false
	}
}

func (w *Worker) EnqueueBlocking(ctx context.Context, v *catalog.Video) bool {
	if v == nil {
		return false
	}
	select {
	case w.ch <- v:
		return true
	case <-ctx.Done():
		return false
	}
}

type ThumbWorker struct {
	Gen     ThumbnailGenerator
	Catalog *catalog.Catalog
	Drive   drives.Drive
	ch      chan *catalog.Video

	RateLimitCooldown time.Duration
	rateLimit         rateLimitState
	activity          taskActivity
}

const (
	defaultRateLimitCooldown        = 30 * time.Minute
	maxPreviewTeaserSizeBytes int64 = 5 * 1024 * 1024 * 1024
	previewStatusSkipped            = "skipped"
)

type rateLimitState struct {
	mu          sync.Mutex
	until       time.Time
	lastSkipLog time.Time
}

type TaskStatus struct {
	State         string
	CurrentTitle  string
	QueueLength   int
	CooldownUntil time.Time
}

type taskActivity struct {
	mu           sync.Mutex
	currentID    string
	currentTitle string
}

func (a *taskActivity) start(v *catalog.Video) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if v == nil {
		a.currentID = ""
		a.currentTitle = ""
		return
	}
	a.currentID = v.ID
	a.currentTitle = v.Title
}

func (a *taskActivity) done() {
	a.mu.Lock()
	a.currentID = ""
	a.currentTitle = ""
	a.mu.Unlock()
}

func (a *taskActivity) current() (string, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.currentID, a.currentTitle
}

func (s *rateLimitState) active(now time.Time) (time.Time, bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.until.IsZero() || !now.Before(s.until) {
		return time.Time{}, false, false
	}
	shouldLog := s.lastSkipLog.IsZero() || now.Sub(s.lastSkipLog) >= 5*time.Minute
	if shouldLog {
		s.lastSkipLog = now
	}
	return s.until, true, shouldLog
}

func (s *rateLimitState) pause(now time.Time, d time.Duration) time.Time {
	if d <= 0 {
		d = defaultRateLimitCooldown
	}
	until := now.Add(d)
	s.mu.Lock()
	if until.After(s.until) {
		s.until = until
	} else {
		until = s.until
	}
	s.lastSkipLog = time.Time{}
	s.mu.Unlock()
	return until
}

func (s *rateLimitState) coolingUntil(now time.Time) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.until.IsZero() || !now.Before(s.until) {
		return time.Time{}, false
	}
	return s.until, true
}

func NewThumbWorker(gen ThumbnailGenerator, cat *catalog.Catalog, drv drives.Drive) *ThumbWorker {
	return &ThumbWorker{
		Gen:     gen,
		Catalog: cat,
		Drive:   drv,
		ch:      make(chan *catalog.Video, 4096),
	}
}

func (w *ThumbWorker) Enqueue(v *catalog.Video) bool {
	if v == nil {
		return false
	}
	select {
	case w.ch <- v:
		return true
	default:
		return false
	}
}

func (w *ThumbWorker) EnqueueBlocking(ctx context.Context, v *catalog.Video) bool {
	if v == nil {
		return false
	}
	select {
	case w.ch <- v:
		return true
	case <-ctx.Done():
		return false
	}
}

func (w *Worker) Status() TaskStatus {
	if w == nil {
		return TaskStatus{State: "idle"}
	}
	return taskStatus(&w.activity, &w.rateLimit, len(w.ch))
}

func (w *ThumbWorker) Status() TaskStatus {
	if w == nil {
		return TaskStatus{State: "idle"}
	}
	return taskStatus(&w.activity, &w.rateLimit, len(w.ch))
}

func taskStatus(activity *taskActivity, rateLimit *rateLimitState, queueLength int) TaskStatus {
	if queueLength < 0 {
		queueLength = 0
	}
	status := TaskStatus{
		State:       "idle",
		QueueLength: queueLength,
	}
	if until, ok := rateLimit.coolingUntil(time.Now()); ok {
		status.State = "cooling"
		status.CooldownUntil = until
		return status
	}
	_, title := activity.current()
	if title != "" {
		status.State = "generating"
		status.CurrentTitle = title
		return status
	}
	if queueLength > 0 {
		status.State = "queued"
	}
	return status
}

// Run 阻塞运行直到 ctx 取消
func (w *Worker) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case v := <-w.ch:
			w.processQueued(ctx, v)
			select {
			case <-ctx.Done():
				return
			case <-time.After(500 * time.Millisecond):
			}
		}
	}
}

// Run 阻塞运行直到 ctx 取消
func (w *ThumbWorker) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case v := <-w.ch:
			w.processQueued(ctx, v)
			select {
			case <-ctx.Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
}

func (w *Worker) processQueued(ctx context.Context, v *catalog.Video) {
	if w.BeforeTask != nil && !w.BeforeTask(ctx) {
		return
	}

	w.activity.start(v)
	defer w.activity.done()
	if !waitForRateLimitCooldown(ctx, &w.rateLimit, "preview", w.Drive) {
		return
	}
	w.process(ctx, v)
}

func (w *ThumbWorker) processQueued(ctx context.Context, v *catalog.Video) {
	w.activity.start(v)
	defer w.activity.done()
	if !waitForRateLimitCooldown(ctx, &w.rateLimit, "thumb", w.Drive) {
		return
	}
	w.process(ctx, v)
}

func waitForRateLimitCooldown(ctx context.Context, state *rateLimitState, label string, drive drives.Drive) bool {
	driveID := ""
	if drive != nil {
		driveID = drive.ID()
	}
	for {
		until, ok := state.coolingUntil(time.Now())
		if !ok {
			return true
		}
		wait := time.Until(until)
		if wait <= 0 {
			continue
		}
		log.Printf("[%s] drive=%s cooling down until=%s; wait before next task", label, driveID, until.Format(time.RFC3339))
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
	}
}

func (w *Worker) skipIfRateLimited(v *catalog.Video) bool {
	until, ok, shouldLog := w.rateLimit.active(time.Now())
	if !ok {
		return false
	}
	if shouldLog {
		log.Printf("[preview] drive=%s rate-limited until=%s; skip queued videos and keep them pending", w.Drive.ID(), until.Format(time.RFC3339))
	}
	return true
}

func (w *Worker) pauseForRateLimit(err error, step, title string) bool {
	retryAfter, ok := drives.RateLimitRetryAfter(err)
	if !ok {
		return false
	}
	if retryAfter <= 0 {
		retryAfter = w.RateLimitCooldown
	}
	until := w.rateLimit.pause(time.Now(), retryAfter)
	log.Printf("[preview] drive=%s rate-limited until=%s step=%s video=%s: %v", w.Drive.ID(), until.Format(time.RFC3339), step, title, err)
	return true
}

func (w *Worker) pauseForRecoverableError(err error, step, title string) bool {
	if w.pauseForRateLimit(err, step, title) {
		return true
	}
	if !driveErrorShouldCooldown(w.Drive, err) {
		return false
	}
	until := w.rateLimit.pause(time.Now(), w.RateLimitCooldown)
	log.Printf("[preview] drive=%s transient media source error until=%s step=%s video=%s: %v", w.Drive.ID(), until.Format(time.RFC3339), step, title, err)
	return true
}

func (w *ThumbWorker) skipIfRateLimited(v *catalog.Video) bool {
	until, ok, shouldLog := w.rateLimit.active(time.Now())
	if !ok {
		return false
	}
	if shouldLog {
		log.Printf("[thumb] drive=%s rate-limited until=%s; skip queued thumbnails and keep them pending", w.Drive.ID(), until.Format(time.RFC3339))
	}
	return true
}

func (w *ThumbWorker) pauseForRateLimit(err error, step, title string) bool {
	retryAfter, ok := drives.RateLimitRetryAfter(err)
	if !ok {
		return false
	}
	if retryAfter <= 0 {
		retryAfter = w.RateLimitCooldown
	}
	until := w.rateLimit.pause(time.Now(), retryAfter)
	log.Printf("[thumb] drive=%s rate-limited until=%s step=%s video=%s: %v", w.Drive.ID(), until.Format(time.RFC3339), step, title, err)
	return true
}

func (w *ThumbWorker) pauseForRecoverableError(err error, step, title string) bool {
	if w.pauseForRateLimit(err, step, title) {
		return true
	}
	if !driveErrorShouldCooldown(w.Drive, err) {
		return false
	}
	until := w.rateLimit.pause(time.Now(), w.RateLimitCooldown)
	log.Printf("[thumb] drive=%s transient media source error until=%s step=%s video=%s: %v", w.Drive.ID(), until.Format(time.RFC3339), step, title, err)
	return true
}

func driveErrorShouldCooldown(d drives.Drive, err error) bool {
	if d == nil || err == nil || d.Kind() != "p115" {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "server returned 403") ||
		strings.Contains(text, "403 forbidden") ||
		strings.Contains(text, "server returned 405") ||
		strings.Contains(text, "405 method") ||
		strings.Contains(text, "access denied") ||
		strings.Contains(text, "moov atom not found") ||
		strings.Contains(text, "partial file") ||
		strings.Contains(text, "request has been blocked") ||
		strings.Contains(text, "访问被阻断")
}

func (w *ThumbWorker) process(ctx context.Context, v *catalog.Video) {
	if w.skipIfRateLimited(v) {
		return
	}
	if current, err := w.Catalog.GetVideo(ctx, v.ID); err == nil {
		if current.ThumbnailURL != "" {
			return
		}
	}
	link, err := w.Drive.StreamURL(ctx, v.FileID)
	if err != nil {
		if localLink, ok := localPreviewLink(v); ok {
			link = localLink
		} else {
			if w.pauseForRecoverableError(err, "streamURL", v.Title) {
				return
			}
			log.Printf("[thumb] streamURL %s: %v", v.Title, err)
			return
		}
	}

	if err := w.generateThumbnailFromLink(ctx, v, link); err != nil {
		if localLink, ok := localPreviewLink(v); ok && link.URL != localLink.URL {
			if localErr := w.generateThumbnailFromLink(ctx, v, localLink); localErr == nil {
				return
			}
		}
		if w.pauseForRecoverableError(err, "generate", v.Title) {
			return
		}
		log.Printf("[thumb] generate %s: %v", v.Title, err)
		return
	}
}

func (w *ThumbWorker) generateThumbnailFromLink(ctx context.Context, v *catalog.Video, link *drives.StreamLink) error {
	duration := thumbnailDurationHint(v, link)
	if duration <= 0 {
		if dur, err := w.Gen.Probe(ctx, link); err == nil && dur > 0 {
			duration = dur
			_ = w.Catalog.UpdateVideoMeta(ctx, v.ID, catalog.VideoMetaPatch{
				DurationSeconds: int(dur),
			})
		} else if err != nil {
			return err
		}
	}

	if _, err := w.Gen.GenerateThumbnail(ctx, link, v.ID, duration); err != nil {
		return err
	}
	_ = w.Catalog.UpdateVideoMeta(ctx, v.ID, catalog.VideoMetaPatch{
		ThumbnailURL: "/p/thumb/" + v.ID,
	})
	log.Printf("[thumb] ready %s", v.Title)
	return nil
}

func thumbnailDurationHint(v *catalog.Video, link *drives.StreamLink) float64 {
	if link != nil && v.PreviewLocal != "" && filepath.Clean(link.URL) == filepath.Clean(v.PreviewLocal) {
		return 0
	}
	return float64(v.DurationSeconds)
}

func localPreviewLink(v *catalog.Video) (*drives.StreamLink, bool) {
	if v.PreviewLocal == "" {
		return nil, false
	}
	clean := filepath.Clean(v.PreviewLocal)
	info, err := os.Stat(clean)
	if err != nil || info.IsDir() || info.Size() == 0 {
		return nil, false
	}
	return &drives.StreamLink{URL: clean}, true
}

func (w *Worker) process(ctx context.Context, v *catalog.Video) {
	if shouldSkipTeaser(v) {
		removePreviousLocalTeaser(v.PreviewLocal, "")
		if err := w.Catalog.UpdatePreview(ctx, v.ID, "", "", previewStatusSkipped); err != nil {
			log.Printf("[preview] skip %s: update status: %v", v.Title, err)
			return
		}
		log.Printf("[preview] skip %s: size=%d exceeds 5GiB teaser limit", v.Title, v.Size)
		return
	}
	if w.skipIfRateLimited(v) {
		return
	}
	link, err := w.Drive.StreamURL(ctx, v.FileID)
	if err != nil {
		if w.pauseForRecoverableError(err, "streamURL", v.Title) {
			return
		}
		log.Printf("[preview] streamURL %s: %v", v.Title, err)
		w.Catalog.UpdatePreview(ctx, v.ID, "", "", "failed")
		return
	}

	// 1) 探时长（失败用 0 继续）
	duration := float64(v.DurationSeconds)
	if duration <= 0 {
		if dur, err := w.Gen.Probe(ctx, link); err == nil && dur > 0 {
			duration = dur
			_ = w.Catalog.UpdateVideoMeta(ctx, v.ID, catalog.VideoMetaPatch{
				DurationSeconds: int(dur),
			})
		} else if err != nil && w.pauseForRecoverableError(err, "probe", v.Title) {
			return
		}
	}

	// 2) teaser
	tmp, err := w.generateTeaser(ctx, v, link, duration)
	if err != nil {
		if w.pauseForRecoverableError(err, "generate", v.Title) {
			return
		}
		log.Printf("[preview] generate %s: %v", v.Title, err)
		w.Catalog.UpdatePreview(ctx, v.ID, "", "", "failed")
		return
	}
	local, err := w.Gen.MoveToLocal(tmp, v.ID)
	if err != nil {
		log.Printf("[preview] move %s: %v", v.Title, err)
		w.Catalog.UpdatePreview(ctx, v.ID, "", "", "failed")
		return
	}

	removePreviousLocalTeaser(v.PreviewLocal, local)
	w.Catalog.UpdatePreview(ctx, v.ID, "", local, "ready")
	log.Printf("[preview] ready %s (duration=%.1fs)", v.Title, duration)
}

func shouldSkipTeaser(v *catalog.Video) bool {
	return v != nil && v.Size > maxPreviewTeaserSizeBytes
}

func (w *Worker) generateTeaser(ctx context.Context, v *catalog.Video, link *drives.StreamLink, duration float64) (string, error) {
	gen, ok := w.Gen.(refreshingTeaserGenerator)
	if !ok || w.Drive == nil || w.Drive.Kind() != "p115" {
		return w.Gen.Generate(ctx, link, duration)
	}
	return gen.GenerateWithLinkProvider(ctx, link, duration, func(ctx context.Context) (*drives.StreamLink, error) {
		return w.Drive.StreamURL(ctx, v.FileID)
	})
}

func removePreviousLocalTeaser(previous, current string) {
	if previous == "" {
		return
	}
	if filepath.Clean(previous) == filepath.Clean(current) {
		return
	}
	if err := os.Remove(previous); err != nil && !os.IsNotExist(err) {
		log.Printf("[preview] remove old local teaser %s: %v", previous, err)
	}
}

// --- utils ---

func buildHeaders(h map[string][]string) string {
	if len(h) == 0 {
		return ""
	}
	var sb strings.Builder
	for k, vs := range h {
		if strings.EqualFold(k, "User-Agent") {
			continue
		}
		for _, v := range vs {
			sb.WriteString(k)
			sb.WriteString(": ")
			sb.WriteString(v)
			sb.WriteString("\r\n")
		}
	}
	return sb.String()
}
