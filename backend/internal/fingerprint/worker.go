package fingerprint

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/video-site/backend/internal/catalog"
	"github.com/video-site/backend/internal/drives"
)

const (
	defaultSampleSizeBytes int64 = 512 * 1024
	defaultFullHashMaxSize int64 = 8 * 1024 * 1024
	defaultCooldown              = 5 * time.Minute
	defaultWorkerQueueSize       = 10000
)

type Config struct {
	SampleSizeBytes   int64
	FullHashMaxSize   int64
	RateLimitCooldown time.Duration
	HTTPClient        *http.Client
}

type Worker struct {
	Catalog *catalog.Catalog
	Drive   drives.Drive
	Config  Config

	ch       chan *catalog.Video
	queue    videoQueue
	activity taskActivity
	cooldown cooldownState
	http     *http.Client
}

type TaskStatus struct {
	State         string
	CurrentTitle  string
	QueueLength   int
	CooldownUntil time.Time
}

func NewWorker(cat *catalog.Catalog, drv drives.Drive, cfg Config) *Worker {
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 0}
	}
	if cfg.SampleSizeBytes <= 0 {
		cfg.SampleSizeBytes = defaultSampleSizeBytes
	}
	if cfg.FullHashMaxSize <= 0 {
		cfg.FullHashMaxSize = defaultFullHashMaxSize
	}
	if cfg.RateLimitCooldown <= 0 {
		cfg.RateLimitCooldown = defaultCooldown
	}
	return &Worker{
		Catalog: cat,
		Drive:   drv,
		Config:  cfg,
		ch:      make(chan *catalog.Video, defaultWorkerQueueSize),
		http:    hc,
	}
}

func (w *Worker) Enqueue(v *catalog.Video) bool {
	if v == nil {
		return false
	}
	if !w.queue.reserve(v.ID) {
		return true
	}
	select {
	case w.ch <- v:
		return true
	default:
		w.queue.release(v.ID)
		return false
	}
}

func (w *Worker) EnqueueBlocking(ctx context.Context, v *catalog.Video) bool {
	if v == nil {
		return false
	}
	if !w.queue.reserve(v.ID) {
		return true
	}
	select {
	case w.ch <- v:
		return true
	case <-ctx.Done():
		w.queue.release(v.ID)
		return false
	}
}

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

func (w *Worker) Status() TaskStatus {
	if w == nil {
		return TaskStatus{State: "idle"}
	}
	currentID, currentTitle := w.activity.current()
	status := TaskStatus{
		State:        "idle",
		CurrentTitle: currentTitle,
		QueueLength:  w.queue.lengthExcluding(currentID),
	}
	if until, ok := w.cooldown.active(time.Now()); ok {
		status.State = "cooling"
		status.CooldownUntil = until
		return status
	}
	if currentID != "" {
		status.State = "generating"
		return status
	}
	if status.QueueLength > 0 {
		status.State = "queued"
	}
	return status
}

func (w *Worker) processQueued(ctx context.Context, v *catalog.Video) {
	defer w.queue.release(v.ID)
	if w.Catalog == nil || w.Drive == nil || v == nil || v.ID == "" {
		return
	}
	current, err := w.Catalog.GetVideo(ctx, v.ID)
	if err != nil {
		return
	}
	if current.SampledSHA256 != "" || current.FingerprintStatus == "ready" || current.Hidden {
		return
	}
	w.activity.start(current)
	defer w.activity.done()
	sum, err := Compute(ctx, w.Drive, current, w.Config, w.http)
	if err != nil {
		var rl *drives.RateLimitError
		if errors.As(err, &rl) {
			wait := rl.RetryAfter
			if wait <= 0 {
				wait = w.Config.RateLimitCooldown
			}
			until := time.Now().Add(wait)
			w.cooldown.set(until)
			log.Printf("[fingerprint] drive=%s rate limited; keep video=%s pending and cool down for %s: %v", w.Drive.ID(), current.ID, wait, err)
			sleepContext(ctx, wait)
			w.cooldown.clear(until)
			return
		}
		log.Printf("[fingerprint] video=%s failed: %v", current.ID, err)
		_ = w.Catalog.UpdateVideoFingerprint(ctx, current.ID, "", "failed", err.Error())
		return
	}
	if err := w.Catalog.UpdateVideoFingerprint(ctx, current.ID, sum, "ready", ""); err != nil {
		log.Printf("[fingerprint] update video=%s: %v", current.ID, err)
		return
	}
	log.Printf("[fingerprint] video=%s ready sampled_sha256=%s", current.ID, sum)
}

func Compute(ctx context.Context, drv drives.Drive, v *catalog.Video, cfg Config, hc *http.Client) (string, error) {
	if drv == nil {
		return "", errors.New("fingerprint: nil drive")
	}
	if v == nil {
		return "", errors.New("fingerprint: nil video")
	}
	if v.Size <= 0 {
		return "", errors.New("fingerprint: video size is empty")
	}
	if cfg.SampleSizeBytes <= 0 {
		cfg.SampleSizeBytes = defaultSampleSizeBytes
	}
	if cfg.FullHashMaxSize <= 0 {
		cfg.FullHashMaxSize = defaultFullHashMaxSize
	}
	if hc == nil {
		hc = &http.Client{Timeout: 0}
	}
	link, err := drv.StreamURL(ctx, v.FileID)
	if err != nil {
		return "", fmt.Errorf("fingerprint: stream url: %w", err)
	}
	if link == nil || strings.TrimSpace(link.URL) == "" {
		return "", errors.New("fingerprint: empty stream url")
	}
	ranges := sampleRanges(v.Size, cfg.SampleSizeBytes, cfg.FullHashMaxSize)
	h := sha256.New()
	writeHashHeader(h, v.Size, ranges)
	for _, r := range ranges {
		data, err := readRange(ctx, hc, link, r)
		if err != nil {
			return "", err
		}
		if int64(len(data)) != r.length {
			return "", fmt.Errorf("fingerprint: short sample at %d: got %d want %d", r.start, len(data), r.length)
		}
		_, _ = h.Write([]byte(fmt.Sprintf("offset=%d length=%d\n", r.start, r.length)))
		_, _ = h.Write(data)
		_, _ = h.Write([]byte("\n"))
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

type byteRange struct {
	start  int64
	length int64
}

func sampleRanges(size, sampleSize, fullHashMax int64) []byteRange {
	if size <= fullHashMax {
		return []byteRange{{start: 0, length: size}}
	}
	if sampleSize > size {
		sampleSize = size
	}
	maxStart := size - sampleSize
	percents := []int64{0, 20, 40, 60, 80}
	out := make([]byteRange, 0, len(percents))
	seen := make(map[int64]struct{}, len(percents))
	for _, pct := range percents {
		start := maxStart * pct / 100
		if _, ok := seen[start]; ok {
			continue
		}
		seen[start] = struct{}{}
		out = append(out, byteRange{start: start, length: sampleSize})
	}
	return out
}

func writeHashHeader(w io.Writer, size int64, ranges []byteRange) {
	_, _ = fmt.Fprintf(w, "video-site-sampled-sha256-v1\nsize=%d\nsamples=%d\n", size, len(ranges))
}

func readRange(ctx context.Context, hc *http.Client, link *drives.StreamLink, r byteRange) ([]byte, error) {
	u, err := url.Parse(link.URL)
	if err == nil && (u.Scheme == "http" || u.Scheme == "https") {
		return readHTTPRange(ctx, hc, link, r)
	}
	path := link.URL
	if err == nil && u.Scheme == "file" {
		path = u.Path
	}
	return readLocalRange(path, r)
}

func readLocalRange(path string, r byteRange) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("fingerprint: open local stream: %w", err)
	}
	defer f.Close()
	buf := make([]byte, r.length)
	n, err := f.ReadAt(buf, r.start)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("fingerprint: read local sample: %w", err)
	}
	if int64(n) != r.length {
		return nil, fmt.Errorf("fingerprint: read local sample at %d: got %d want %d", r.start, n, r.length)
	}
	return buf, nil
}

func readHTTPRange(ctx context.Context, hc *http.Client, link *drives.StreamLink, r byteRange) ([]byte, error) {
	end := r.start + r.length - 1
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link.URL, nil)
	if err != nil {
		return nil, err
	}
	for k, vs := range link.Headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", r.start, end))
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fingerprint: read remote sample: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, &drives.RateLimitError{
			Provider:   "fingerprint",
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
			Err:        fmt.Errorf("remote sample rate limited: status=%d", resp.StatusCode),
		}
	}
	if resp.StatusCode != http.StatusPartialContent {
		if resp.StatusCode == http.StatusOK && r.start == 0 {
			data, err := io.ReadAll(io.LimitReader(resp.Body, r.length+1))
			if err != nil {
				return nil, err
			}
			if int64(len(data)) == r.length {
				return data, nil
			}
		}
		return nil, fmt.Errorf("fingerprint: range request got status=%d for bytes=%d-%d", resp.StatusCode, r.start, end)
	}
	return io.ReadAll(io.LimitReader(resp.Body, r.length))
}

func parseRetryAfter(raw string) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(raw); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(raw); err == nil {
		d := time.Until(when)
		if d > 0 {
			return d
		}
	}
	return 0
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
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

type cooldownState struct {
	mu    sync.Mutex
	until time.Time
}

func (s *cooldownState) set(until time.Time) {
	s.mu.Lock()
	s.until = until
	s.mu.Unlock()
}

func (s *cooldownState) clear(until time.Time) {
	s.mu.Lock()
	if s.until.Equal(until) {
		s.until = time.Time{}
	}
	s.mu.Unlock()
}

func (s *cooldownState) active(now time.Time) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.until.IsZero() || !s.until.After(now) {
		return time.Time{}, false
	}
	return s.until, true
}

type videoQueue struct {
	mu  sync.Mutex
	ids map[string]struct{}
}

func (q *videoQueue) reserve(id string) bool {
	if id == "" {
		return true
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.ids == nil {
		q.ids = make(map[string]struct{})
	}
	if _, ok := q.ids[id]; ok {
		return false
	}
	q.ids[id] = struct{}{}
	return true
}

func (q *videoQueue) release(id string) {
	if id == "" {
		return
	}
	q.mu.Lock()
	delete(q.ids, id)
	q.mu.Unlock()
}

func (q *videoQueue) lengthExcluding(currentID string) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := len(q.ids)
	if currentID != "" {
		if _, ok := q.ids[currentID]; ok {
			n--
		}
	}
	if n < 0 {
		return 0
	}
	return n
}
