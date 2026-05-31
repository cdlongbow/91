// Package nightly orchestrates the single nightly maintenance pipeline that
// replaces the legacy scanLoop / crawlerLoop / spider91 migrator periodic loop.
//
// Pipeline (fired once per day at cron_hour, also via TriggerNow for admin
// "扫描所有网盘"):
//
//	Phase 1: for each non-spider91 cloud drive
//	           scan + delete-detection + enqueue thumb + enqueue teaser
//	         wait until all thumb / teaser queues are idle
//	Phase 2: if any spider91 drive configured
//	           crawl + enqueue teaser for new videos
//	         wait until teaser queues are idle
//	Phase 3: spider91 → cloud migration (single sweep, captcha cooldown still
//	         honored within this call)
//	Phase 4: cleanup duplicate local preview/thumbnail assets after sampled
//	         fingerprints have identified canonical videos
//
// A 6h soft deadline guards each pipeline run; phases check deadline at their
// boundaries and exit cleanly if exceeded (no in-flight ffmpeg / upload is
// killed mid-task).
// 已废弃：这条软超时机制已在 2026-05 移除。流水线现在会一直跑到所有 phase
// 完成或进程被停止；yaml 里的 nightly.max_duration 字段被忽略。理由：单条
// phase 里的网盘风控冷却可能长达数十分钟（115 列目录 10min × N），强制 6h
// 切换会让被打断的子任务延后到下一晚，体感上反而更糟。
//
// State persistence: the date string of the most recent successfully started
// run is stored in catalog.settings under the key "nightly.last_run_date".
// This survives restarts so a quick crash inside cron_hour won't trigger a
// duplicate pipeline.
package nightly

import (
	"context"
	"log"
	"sync"
	"time"
)

const (
	// settingLastRunDate stores the YYYY-MM-DD of the last natural cron-triggered
	// pipeline run. Manual TriggerNow() also updates this to keep behavior consistent.
	settingLastRunDate = "nightly.last_run_date"
	// dateLayout matches catalog.GetSetting string semantics; using ISO-8601 date.
	dateLayout = "2006-01-02"
	// pollInterval is the heartbeat for the natural cron decision loop.
	pollInterval = time.Minute
)

// SettingStore is the minimal catalog.Catalog surface we rely on.
type SettingStore interface {
	GetSetting(ctx context.Context, key, defaultValue string) (string, error)
	SetSetting(ctx context.Context, key, value string) error
}

// Config wires the runner to its dependencies. The function-callback shape
// avoids importing main / drives / preview from this package, keeping the
// dependency graph clean.
type Config struct {
	Settings SettingStore
	CronHour int // 0-23; default 1 (01:00)
	// MaxDuration 已废弃。早期作为流水线总耗时软超时（默认 6h），到点不再启动后
	// 续 phase。当前实现忽略此字段 —— 流水线一直跑到所有 phase 完成或 ctx 取消。
	// 字段保留是为了让旧 config.yaml 加载时不报 "unknown field"。
	MaxDuration time.Duration

	// ListScanTargets returns the drive IDs to run Phase 1 on, in deterministic
	// order. Should exclude spider91 and localupload drives.
	ListScanTargets func(ctx context.Context) []string

	// RunScan synchronously runs scan + cleanup + enqueueDriveGeneration for
	// one drive. Errors are expected to be logged inside, not surfaced.
	RunScan func(ctx context.Context, driveID string)

	// ListSpider91Drives returns spider91 drive IDs to crawl in Phase 2.
	// Returns empty slice when no spider91 drive is configured.
	ListSpider91Drives func(ctx context.Context) []string

	// RunSpider91Crawl synchronously runs one crawl cycle (downloads + thumbs +
	// teaser enqueue) for a single spider91 drive.
	RunSpider91Crawl func(ctx context.Context, driveID string)

	// WaitPreviewQueuesIdle blocks until both the thumbnail and teaser queues
	// across all drives are drained (queue empty + no in-flight task). It must
	// honor ctx cancellation.
	WaitPreviewQueuesIdle func(ctx context.Context) error

	// RunMigration runs spider91migrate.Migrator.RunOnce for Phase 3.
	RunMigration func(ctx context.Context) error

	// RunDedupeAssetCleanup removes generated local assets from non-canonical
	// videos in size+sampled_sha256 duplicate groups. It must not delete cloud
	// files or catalog rows.
	RunDedupeAssetCleanup func(ctx context.Context) error

	// Now is injected for tests; nil → time.Now.
	Now func() time.Time
}

type Status struct {
	State          string
	Running        bool
	Queued         bool
	StartedAt      time.Time
	LastFinishedAt time.Time
}

// Runner drives the nightly pipeline.
type Runner struct {
	cfg     Config
	trigger chan struct{} // buffered(1); manual "run now"
	runMu   sync.Mutex    // prevents overlapping pipeline runs

	stateMu        sync.Mutex
	running        bool
	queued         bool
	startedAt      time.Time
	lastFinishedAt time.Time
}

// New constructs a Runner. cfg is shallow-copied; defaults are applied.
func New(cfg Config) *Runner {
	if cfg.CronHour < 0 || cfg.CronHour > 23 {
		cfg.CronHour = 1
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Runner{
		cfg:     cfg,
		trigger: make(chan struct{}, 1),
	}
}

// Run is a blocking loop until ctx is done. It wakes up once per minute and
// either fires the natural cron-driven pipeline (when cron_hour matches and
// today hasn't run) or honors a manual TriggerNow() request.
func (r *Runner) Run(ctx context.Context) {
	t := time.NewTicker(pollInterval)
	defer t.Stop()
	log.Printf("[nightly] runner started; cron_hour=%d", r.cfg.CronHour)
	for {
		select {
		case <-ctx.Done():
			log.Printf("[nightly] runner stopping: %v", ctx.Err())
			return
		case <-t.C:
			r.tryNaturalRun(ctx)
		case <-r.trigger:
			log.Printf("[nightly] manual trigger received")
			r.runPipelineLocked(ctx, true)
		}
	}
}

// TriggerNow asks the running loop to fire a pipeline ASAP. Only one manual
// trigger can be active at a time: if a pipeline is already running or waiting
// in the trigger channel, the request is ignored and returns false.
func (r *Runner) TriggerNow() bool {
	r.stateMu.Lock()
	if r.running || r.queued {
		r.stateMu.Unlock()
		return false
	}
	r.queued = true
	r.stateMu.Unlock()

	select {
	case r.trigger <- struct{}{}:
		return true
	default:
		r.stateMu.Lock()
		r.queued = false
		r.stateMu.Unlock()
		return false
	}
}

func (r *Runner) Status() Status {
	r.stateMu.Lock()
	running := r.running
	queued := r.queued
	startedAt := r.startedAt
	lastFinishedAt := r.lastFinishedAt
	r.stateMu.Unlock()

	state := "idle"
	switch {
	case running && queued:
		state = "running_queued"
	case running:
		state = "running"
	case queued:
		state = "queued"
	}

	return Status{
		State:          state,
		Running:        running,
		Queued:         queued,
		StartedAt:      startedAt,
		LastFinishedAt: lastFinishedAt,
	}
}

// tryNaturalRun checks the cron decision and runs the pipeline if due today.
func (r *Runner) tryNaturalRun(ctx context.Context) {
	now := r.cfg.Now()
	if now.Hour() != r.cfg.CronHour {
		return
	}
	last, err := r.readLastRunDate(ctx)
	if err != nil {
		log.Printf("[nightly] read last_run_date: %v", err)
		return
	}
	if !shouldRun(now, last) {
		return
	}
	log.Printf("[nightly] natural cron trigger at %s", now.Format(time.RFC3339))
	r.runPipelineLocked(ctx, false)
}

// shouldRun returns true when "today" (per now) hasn't already been processed.
func shouldRun(now time.Time, lastRunDate string) bool {
	return lastRunDate != now.Format(dateLayout)
}

// runPipelineLocked guards against overlapping runs. If another pipeline is
// in progress, the call returns immediately (logged once). After completion
// (regardless of success), today's date is recorded so subsequent triggers
// the same calendar day are skipped.
//
// 流水线没有总耗时上限：一直跑到 ctx 取消（进程退出）或所有 phase 完成。
func (r *Runner) runPipelineLocked(ctx context.Context, manual bool) {
	if !r.runMu.TryLock() {
		log.Printf("[nightly] another pipeline is already running, skipping this trigger")
		return
	}

	started := r.cfg.Now()
	r.markStarted(started)
	defer func() {
		r.markFinished(r.cfg.Now())
		r.runMu.Unlock()
	}()

	mode := "scheduled"
	if manual {
		mode = "manual"
	}
	log.Printf("[nightly] pipeline (%s) start", mode)

	r.runPipeline(ctx)

	finished := r.cfg.Now()
	log.Printf("[nightly] pipeline (%s) finish; took=%s", mode, finished.Sub(started).Round(time.Second))

	// Mark today as processed regardless of success/error. This is intentional:
	// a partial / failing pipeline shouldn't trigger again the same day, the
	// admin can inspect logs and click "扫描所有网盘" to retry explicitly.
	dateStr := started.Format(dateLayout)
	if err := r.cfg.Settings.SetSetting(ctx, settingLastRunDate, dateStr); err != nil {
		log.Printf("[nightly] persist last_run_date: %v", err)
	}
}

func (r *Runner) markStarted(started time.Time) {
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	r.running = true
	r.queued = false
	r.startedAt = started
}

func (r *Runner) markFinished(finished time.Time) {
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	r.running = false
	r.startedAt = time.Time{}
	r.lastFinishedAt = finished
}

// runPipeline executes the three phases. It returns when the pipeline finishes
// OR ctx is done (deadline / cancel). Errors are logged but not propagated —
// each phase is best-effort; downstream phases still attempt to run unless ctx
// is dead.
func (r *Runner) runPipeline(ctx context.Context) {
	// ---------- Phase 1 ----------
	if r.checkDeadline(ctx, "phase 1") {
		return
	}
	scanIDs := []string{}
	if r.cfg.ListScanTargets != nil {
		scanIDs = r.cfg.ListScanTargets(ctx)
	}
	if len(scanIDs) == 0 {
		log.Printf("[nightly] phase 1 skipped: no cloud drives to scan")
	} else {
		log.Printf("[nightly] phase 1: scanning %d drive(s)", len(scanIDs))
		for _, id := range scanIDs {
			if ctx.Err() != nil {
				log.Printf("[nightly] phase 1 aborted by ctx: %v", ctx.Err())
				return
			}
			log.Printf("[nightly] phase 1: scanning drive=%s", id)
			r.cfg.RunScan(ctx, id)
		}
		log.Printf("[nightly] phase 1: waiting for preview queues to drain")
		if err := r.waitIdle(ctx, "phase 1"); err != nil {
			return
		}
	}

	// ---------- Phase 2 ----------
	if r.checkDeadline(ctx, "phase 2") {
		return
	}
	spiderIDs := []string{}
	if r.cfg.ListSpider91Drives != nil {
		spiderIDs = r.cfg.ListSpider91Drives(ctx)
	}
	if len(spiderIDs) == 0 {
		log.Printf("[nightly] phase 2/3 skipped: no spider91 drive configured")
		r.runDedupeAssetCleanupPhase(ctx)
		return
	}
	log.Printf("[nightly] phase 2: crawling %d spider91 drive(s)", len(spiderIDs))
	for _, id := range spiderIDs {
		if ctx.Err() != nil {
			log.Printf("[nightly] phase 2 aborted by ctx: %v", ctx.Err())
			return
		}
		log.Printf("[nightly] phase 2: crawling drive=%s", id)
		r.cfg.RunSpider91Crawl(ctx, id)
	}
	log.Printf("[nightly] phase 2: waiting for teaser queue to drain")
	if err := r.waitIdle(ctx, "phase 2"); err != nil {
		return
	}

	// ---------- Phase 3 ----------
	if r.checkDeadline(ctx, "phase 3") {
		return
	}
	log.Printf("[nightly] phase 3: spider91 migration")
	if r.cfg.RunMigration != nil {
		if err := r.cfg.RunMigration(ctx); err != nil {
			log.Printf("[nightly] phase 3 migration: %v", err)
		}
	}

	r.runDedupeAssetCleanupPhase(ctx)
}

// checkDeadline returns true when ctx is already done (runner shutting down or
// upstream cancel) and the caller should bail. 已不再有"流水线总耗时上限"语义；
// 函数名保留是为了改动最小，仅作 ctx 取消检测。
func (r *Runner) checkDeadline(ctx context.Context, phase string) bool {
	if err := ctx.Err(); err != nil {
		log.Printf("[nightly] %s: ctx done (%v), bailing out", phase, err)
		return true
	}
	return false
}

// waitIdle calls the configured WaitPreviewQueuesIdle, logging the outcome.
func (r *Runner) waitIdle(ctx context.Context, phase string) error {
	if r.cfg.WaitPreviewQueuesIdle == nil {
		return nil
	}
	if err := r.cfg.WaitPreviewQueuesIdle(ctx); err != nil {
		log.Printf("[nightly] %s: wait preview queues: %v", phase, err)
		return err
	}
	return nil
}

func (r *Runner) runDedupeAssetCleanupPhase(ctx context.Context) {
	if r.checkDeadline(ctx, "phase 4") {
		return
	}
	if r.cfg.RunDedupeAssetCleanup == nil {
		return
	}
	log.Printf("[nightly] phase 4: duplicate asset cleanup")
	if err := r.cfg.RunDedupeAssetCleanup(ctx); err != nil {
		log.Printf("[nightly] phase 4 duplicate asset cleanup: %v", err)
	}
}

// readLastRunDate reads the persisted last_run_date or returns "" when unset.
func (r *Runner) readLastRunDate(ctx context.Context) (string, error) {
	if r.cfg.Settings == nil {
		return "", nil
	}
	return r.cfg.Settings.GetSetting(ctx, settingLastRunDate, "")
}
