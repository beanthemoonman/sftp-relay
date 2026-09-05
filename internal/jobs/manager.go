package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"sftp-relay/internal/db"
	"sftp-relay/internal/events"
	"sftp-relay/internal/nas"
	"sftp-relay/internal/sftpclient"
)

// logKeep is the per-job ring-buffer depth in job_log.
const logKeep = 200

// pollInterval is how often the destination size is checked once lftp's output
// has gone quiet.
const pollInterval = 5 * time.Second

// Manager owns the worker pool and every running job's cancel function.
type Manager struct {
	store *db.DB
	hub   *events.Hub
	tick  time.Duration

	// Seams, not abstractions: these point at the real NAS and SFTP pool, and
	// tests swap them so the whole lifecycle can be driven without an SSH server.
	transfer   func(context.Context, nas.TransferSpec, func(string)) (int, error)
	remoteSize func(context.Context, db.Server, string) (int64, bool, error)
	destSize   func(context.Context, string) (int64, error)
	killRemote func(context.Context, int64) error
	sweep      func(context.Context) error

	mu       sync.Mutex
	running  map[int64]context.CancelFunc
	stopping bool
	started  bool

	wg   sync.WaitGroup
	stop chan struct{}
	done chan struct{}
}

// New wires the manager to the real remote server pool and the real NAS.
func New(store *db.DB, pool *sftpclient.Pool, nasc *nas.Client, hub *events.Hub) *Manager {
	return &Manager{
		store:      store,
		hub:        hub,
		tick:       time.Second,
		transfer:   nasc.Transfer,
		remoteSize: pool.Size,
		destSize:   nasc.Size,
		killRemote: nasc.Cancel,
		sweep:      nasc.Sweep,
		running:    map[int64]context.CancelFunc{},
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
	}
}

// Start performs restart recovery and then runs the scheduler until Stop.
func (m *Manager) Start(ctx context.Context) error {
	n, err := m.store.InterruptRunningJobs(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		slog.Info("jobs: requeued jobs interrupted by a restart", "count", n)
	}
	m.mu.Lock()
	m.started = true
	m.mu.Unlock()

	// A SIGKILL outruns the workspace cleanup trap, so clear anything stale
	// before the first job runs. Best effort: an offline NAS must not block boot.
	go func() {
		sweepCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err := m.sweep(sweepCtx); err != nil && !errors.Is(err, nas.ErrNotConfigured) {
			slog.Warn("jobs: could not sweep stale workspaces", "err", err)
		}
	}()

	go m.loop(ctx)
	return nil
}

// Stop drains the pool: the scheduler halts, running jobs are cancelled and
// recorded as interrupted, and every worker goroutine is waited for.
func (m *Manager) Stop() {
	m.mu.Lock()
	if m.stopping || !m.started {
		m.mu.Unlock()
		return
	}
	m.stopping = true
	cancels := make([]context.CancelFunc, 0, len(m.running))
	for _, c := range m.running {
		cancels = append(cancels, c)
	}
	m.mu.Unlock()

	close(m.stop)
	<-m.done
	for _, c := range cancels {
		c()
	}
	m.wg.Wait()
}

func (m *Manager) loop(ctx context.Context) {
	defer close(m.done)
	sched := time.NewTicker(m.tick)
	defer sched.Stop()
	retention := time.NewTicker(time.Hour)
	defer retention.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.stop:
			return
		case <-sched.C:
			m.schedule(ctx)
		case <-retention.C:
			m.purge(ctx)
		}
	}
}

// schedule starts as many waiting jobs as the concurrency setting allows. The
// setting is re-read every tick, which is what makes it resizable at runtime.
func (m *Manager) schedule(ctx context.Context) {
	settings, err := m.store.Settings(ctx)
	if err != nil {
		slog.Error("jobs: reading settings", "err", err)
		return
	}
	concurrency := settingInt(settings, "concurrency", 2)

	m.mu.Lock()
	free := concurrency - len(m.running)
	stopping := m.stopping
	m.mu.Unlock()
	if free <= 0 || stopping {
		return
	}

	waiting, err := m.store.RunnableJobs(ctx, free)
	if err != nil {
		slog.Error("jobs: reading the queue", "err", err)
		return
	}
	for _, job := range waiting {
		if err := Transition(job.Status, StatusRunning); err != nil {
			slog.Error("jobs: refusing to start job", "job_id", job.ID, "err", err)
			continue
		}
		if err := m.store.SetJobStatus(ctx, job.ID, StatusRunning, "", nil); err != nil {
			slog.Error("jobs: marking job running", "job_id", job.ID, "err", err)
			continue
		}
		job.Status = StatusRunning

		runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		m.mu.Lock()
		if m.stopping {
			m.mu.Unlock()
			cancel()
			return
		}
		m.running[job.ID] = cancel
		m.mu.Unlock()

		m.wg.Add(1)
		go func(j db.Job) {
			defer m.wg.Done()
			defer cancel()
			defer func() {
				m.mu.Lock()
				delete(m.running, j.ID)
				m.mu.Unlock()
			}()
			m.run(runCtx, j, settings)
		}(job)
	}
}

func (m *Manager) purge(ctx context.Context) {
	settings, err := m.store.Settings(ctx)
	if err != nil {
		slog.Error("jobs: reading settings for retention", "err", err)
		return
	}
	days := settingInt(settings, "history_retention_days", 90)
	if days <= 0 {
		return
	}
	n, err := m.store.PurgeJobs(ctx, days)
	if err != nil {
		slog.Error("jobs: retention sweep", "err", err)
		return
	}
	if n > 0 {
		slog.Info("jobs: retention sweep removed old jobs", "count", n, "older_than_days", days)
	}
}

// run executes one job start to finish and records the outcome.
func (m *Manager) run(ctx context.Context, job db.Job, settings map[string]string) {
	m.publish(ctx, events.TypeProgress, job.ID)

	server, err := m.store.GetServer(ctx, job.ServerID)
	if err != nil {
		m.finish(job, StatusFailed, err.Error(), nil)
		return
	}

	if job.TotalBytes == 0 {
		total, _, err := m.remoteSize(ctx, server, job.RemotePath)
		if err != nil {
			m.finish(job, StatusFailed, err.Error(), nil)
			return
		}
		if err := m.store.SetJobTotal(ctx, job.ID, total); err != nil {
			slog.Error("jobs: recording total size", "job_id", job.ID, "err", err)
		}
		job.TotalBytes = total
	}

	tr := newTracker(job.TotalBytes, func(bytes, speed, eta int64) {
		flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := m.store.SetJobProgress(flushCtx, job.ID, bytes, speed, eta); err != nil {
			slog.Error("jobs: writing progress", "job_id", job.ID, "err", err)
		}
		m.publish(flushCtx, events.TypeProgress, job.ID)
	})

	target := path.Join(job.DestPath, path.Base(job.RemotePath))
	pollDone := make(chan struct{})
	go m.pollDest(ctx, tr, target, pollDone)

	spec := nas.TransferSpec{
		JobID:    job.ID,
		Kind:     job.Kind,
		Remote:   job.RemotePath,
		Dest:     job.DestPath,
		Segments: settingInt(settings, "segments", 4),
		Parallel: settingInt(settings, "concurrency", 2),
		Server:   server,
	}
	code, err := m.transfer(ctx, spec, func(line string) {
		if p, ok := nas.ParseProgress(line); ok {
			tr.observe(p)
			if !p.Final {
				return // progress noise does not belong in the log
			}
		}
		m.log(ctx, job.ID, line)
	})
	close(pollDone)

	switch {
	case err != nil:
		// A cancel or a shutdown both cancel the context; which one it was is
		// recorded in the row by Cancel, so re-read rather than guess.
		if current, gerr := m.store.GetJob(context.WithoutCancel(ctx), job.ID); gerr == nil &&
			current.Status == StatusCancelled {
			m.cleanup(job.ID)
			return
		}
		if ctx.Err() != nil {
			m.finish(job, StatusInterrupted, "interrupted by a relay restart; resuming", nil)
			m.cleanup(job.ID)
			return
		}
		m.finish(job, StatusFailed, err.Error(), nil)
	case code != 0:
		m.finish(job, StatusFailed, fmt.Sprintf("lftp exited with status %d", code), &code)
	default:
		tr.complete()
		m.finish(job, StatusDone, "", &code)
	}
}

// pollDest is the fallback for lftp versions whose output we cannot parse: if
// nothing parseable has arrived recently, measure the destination instead.
func (m *Manager) pollDest(ctx context.Context, tr *tracker, target string, done <-chan struct{}) {
	t := time.NewTicker(pollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case <-t.C:
			if !tr.stale() {
				continue
			}
			sizeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			n, err := m.destSize(sizeCtx, target)
			cancel()
			if err != nil {
				slog.Debug("jobs: destination size poll failed", "path", target, "err", err)
				continue
			}
			tr.observeSize(n)
		}
	}
}

// finish records a terminal (or interrupted) status and announces it.
func (m *Manager) finish(job db.Job, status, errMsg string, code *int) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := Transition(job.Status, status); err != nil {
		slog.Error("jobs: refusing status change", "job_id", job.ID, "err", err)
		return
	}
	if err := m.store.SetJobStatus(ctx, job.ID, status, errMsg, code); err != nil {
		slog.Error("jobs: recording final status", "job_id", job.ID, "status", status, "err", err)
		return
	}
	evt := events.TypeDone
	if status == StatusFailed {
		evt = events.TypeFailed
		slog.Error("jobs: job failed", "job_id", job.ID, "err", errMsg)
	}
	if status == StatusInterrupted {
		evt = events.TypeProgress
	}
	m.publish(ctx, evt, job.ID)
	m.cleanup(job.ID)
}

// cleanup removes the remote workspace when the trap could not. Best effort:
// the startup sweep is the backstop.
func (m *Manager) cleanup(jobID int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := m.killRemote(ctx, jobID); err != nil {
		slog.Debug("jobs: post-run cleanup", "job_id", jobID, "err", err)
	}
}

func (m *Manager) log(ctx context.Context, jobID int64, line string) {
	logCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := m.store.AppendJobLog(logCtx, jobID, line, logKeep); err != nil {
		slog.Error("jobs: appending log line", "job_id", jobID, "err", err)
		return
	}
	m.hub.Publish(events.Event{Type: events.TypeLog, JobID: jobID, Data: line})
}

// publish re-reads the row so every subscriber sees exactly what the API would
// have returned for the same job.
func (m *Manager) publish(ctx context.Context, typ string, jobID int64) {
	job, err := m.store.GetJob(context.WithoutCancel(ctx), jobID)
	if err != nil {
		slog.Error("jobs: reading job for an event", "job_id", jobID, "err", err)
		return
	}
	m.hub.Publish(events.Event{Type: typ, JobID: jobID, Data: ViewOf(job)})
}

// Cancel stops a job wherever it is in its life. A running job is TERMed on the
// NAS first — closing the SSH session does not reliably kill lftp.
func (m *Manager) Cancel(ctx context.Context, id int64) error {
	job, err := m.store.GetJob(ctx, id)
	if err != nil {
		return err
	}
	if err := Transition(job.Status, StatusCancelled); err != nil {
		return err
	}
	if err := m.store.SetJobStatus(ctx, id, StatusCancelled, "cancelled", nil); err != nil {
		return err
	}
	m.mu.Lock()
	cancel, running := m.running[id]
	m.mu.Unlock()
	if running {
		if err := m.killRemote(ctx, id); err != nil {
			slog.Warn("jobs: could not signal lftp on the NAS", "job_id", id, "err", err)
		}
		cancel()
	}
	m.publish(ctx, events.TypeDone, id)
	slog.Info("jobs: cancelled", "job_id", id)
	return nil
}

// Retry clones a finished job. The clone resumes rather than restarts, because
// pget -c and mirror --continue pick up whatever bytes already landed.
func (m *Manager) Retry(ctx context.Context, id int64) (int64, error) {
	job, err := m.store.GetJob(ctx, id)
	if err != nil {
		return 0, err
	}
	if !Terminal(job.Status) {
		return 0, fmt.Errorf("%w: job %d is %s, not finished", ErrIllegalTransition, id, job.Status)
	}
	newID, err := m.store.CreateJob(ctx, db.Job{
		ServerID: job.ServerID, Kind: job.Kind, RemotePath: job.RemotePath,
		DestPath: job.DestPath, Status: StatusQueued, TotalBytes: job.TotalBytes,
	})
	if err != nil {
		return 0, err
	}
	m.publish(ctx, events.TypeCreated, newID)
	slog.Info("jobs: retrying", "job_id", id, "new_job_id", newID)
	return newID, nil
}

// Announce publishes a job.created event for a freshly queued job.
func (m *Manager) Announce(ctx context.Context, id int64) {
	m.publish(ctx, events.TypeCreated, id)
}

func settingInt(s map[string]string, key string, fallback int) int {
	v, err := strconv.Atoi(strings.TrimSpace(s[key]))
	if err != nil || v < 0 {
		return fallback
	}
	return v
}
