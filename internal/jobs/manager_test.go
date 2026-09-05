package jobs

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	"sftp-relay/internal/db"
	"sftp-relay/internal/events"
	"sftp-relay/internal/nas"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// harness is a Manager with every remote seam replaced. Job lifecycles are
// driven by channels rather than by waiting on the clock.
type harness struct {
	*Manager
	store  *db.DB
	hub    *events.Hub
	events <-chan events.Event

	started  chan int64    // a job id, once its transfer begins
	release  chan struct{} // closed or fed to let a transfer return
	killed   chan int64    // job ids TERMed on the NAS
	exitCode int
	xferErr  error

	mu         sync.Mutex
	concurrent int
	maxSeen    int
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	store, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	hub := events.NewHub()
	_, ch := hub.Subscribe()
	h := &harness{
		store:   store,
		hub:     hub,
		events:  ch,
		started: make(chan int64, 16),
		release: make(chan struct{}),
		killed:  make(chan int64, 16),
	}
	h.Manager = &Manager{
		store:   store,
		hub:     hub,
		tick:    time.Millisecond,
		running: map[int64]context.CancelFunc{},
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
		transfer: func(ctx context.Context, spec nas.TransferSpec, onLine func(string)) (int, error) {
			h.enter()
			defer h.leave()
			h.started <- spec.JobID
			onLine("`f' at 512 (50%) 1.0K/s eta:1s")
			onLine("connecting to the remote server")
			select {
			case <-h.release:
			case <-ctx.Done():
				return -1, ctx.Err()
			}
			return h.exitCode, h.xferErr
		},
		remoteSize: func(context.Context, db.Server, string) (int64, bool, error) {
			return 1024, false, nil
		},
		destSize: func(context.Context, string) (int64, error) { return 0, nil },
		killRemote: func(_ context.Context, id int64) error {
			h.killed <- id
			return nil
		},
		sweep: func(context.Context) error { return nil },
	}
	return h
}

func (h *harness) enter() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.concurrent++
	if h.concurrent > h.maxSeen {
		h.maxSeen = h.concurrent
	}
}

func (h *harness) leave() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.concurrent--
}

func (h *harness) peakConcurrency() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.maxSeen
}

func (h *harness) seedServer(t *testing.T) int64 {
	t.Helper()
	id, err := h.store.CreateServer(context.Background(), db.Server{
		Name: "src", Host: "h", Port: 22, Username: "u", AuthType: "key",
		PrivateKey: "k", HostKey: "hk",
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (h *harness) queue(t *testing.T, serverID int64, status string) int64 {
	t.Helper()
	id, err := h.store.CreateJob(context.Background(), db.Job{
		ServerID: serverID, Kind: "file", RemotePath: "/pub/f",
		DestPath: "/volume1/media", Status: status,
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// waitEvent blocks until an event of the given type for jobID arrives.
func (h *harness) waitEvent(t *testing.T, typ string, jobID int64) events.Event {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case e, ok := <-h.events:
			if !ok {
				t.Fatal("the event hub closed the subscription")
			}
			if e.Type == typ && (jobID == 0 || e.JobID == jobID) {
				return e
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s on job %d", typ, jobID)
		}
	}
}

func (h *harness) status(t *testing.T, id int64) string {
	t.Helper()
	job, err := h.store.GetJob(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return job.Status
}

func (h *harness) setConcurrency(t *testing.T, n int) {
	t.Helper()
	if err := h.store.SetSettings(context.Background(),
		map[string]string{"concurrency": fmt.Sprint(n)}); err != nil {
		t.Fatal(err)
	}
}

func TestJobRunsToCompletion(t *testing.T) {
	h := newHarness(t)
	server := h.seedServer(t)
	id := h.queue(t, server, StatusQueued)
	close(h.release)

	if err := h.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer h.Stop()

	h.waitEvent(t, events.TypeDone, id)
	if got := h.status(t, id); got != StatusDone {
		t.Fatalf("status = %q, want done", got)
	}
	job, err := h.store.GetJob(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if job.TotalBytes != 1024 {
		t.Errorf("total_bytes = %d, want the pre-computed 1024", job.TotalBytes)
	}
	if job.TransferredBytes != 1024 {
		t.Errorf("transferred_bytes = %d, want 1024 at completion", job.TransferredBytes)
	}
	if !job.ExitCode.Valid || job.ExitCode.Int64 != 0 {
		t.Errorf("exit_code = %+v, want 0", job.ExitCode)
	}
	if v := ViewOf(job); v.Percent != 100 {
		t.Errorf("percent = %d, want 100", v.Percent)
	}

	// Progress lines are not log noise; real messages are.
	lines, err := h.store.JobLog(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || lines[0] != "connecting to the remote server" {
		t.Errorf("job log = %q, want only the non-progress line", lines)
	}
}

func TestJobFailsOnANonZeroExitCode(t *testing.T) {
	h := newHarness(t)
	h.exitCode = 1
	server := h.seedServer(t)
	id := h.queue(t, server, StatusQueued)
	close(h.release)

	if err := h.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer h.Stop()

	h.waitEvent(t, events.TypeFailed, id)
	job, err := h.store.GetJob(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != StatusFailed {
		t.Fatalf("status = %q, want failed", job.Status)
	}
	if job.Error == "" {
		t.Error("a failed job must carry a readable error")
	}
}

func TestJobFailsWhenTheTransferErrors(t *testing.T) {
	h := newHarness(t)
	h.xferErr = errors.New("nas: lftp not found on the NAS")
	server := h.seedServer(t)
	id := h.queue(t, server, StatusQueued)
	close(h.release)

	if err := h.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer h.Stop()

	h.waitEvent(t, events.TypeFailed, id)
	job, err := h.store.GetJob(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != StatusFailed || job.Error == "" {
		t.Fatalf("job = %q / %q, want failed with an error", job.Status, job.Error)
	}
}

func TestConcurrencyIsCappedBySettings(t *testing.T) {
	h := newHarness(t)
	h.setConcurrency(t, 1)
	server := h.seedServer(t)
	ids := []int64{
		h.queue(t, server, StatusQueued),
		h.queue(t, server, StatusQueued),
		h.queue(t, server, StatusQueued),
	}

	if err := h.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer h.Stop()

	// Let each job through one at a time, in queue order.
	for _, want := range ids {
		got := <-h.started
		if got != want {
			t.Fatalf("started job %d, want %d: the queue is not FIFO", got, want)
		}
		h.release <- struct{}{}
		h.waitEvent(t, events.TypeDone, want)
	}
	if peak := h.peakConcurrency(); peak != 1 {
		t.Errorf("peak concurrency = %d, want 1", peak)
	}
}

func TestCancelStopsARunningJobAndSignalsTheNAS(t *testing.T) {
	h := newHarness(t)
	server := h.seedServer(t)
	id := h.queue(t, server, StatusQueued)

	if err := h.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer h.Stop()

	<-h.started
	if err := h.Cancel(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if killed := <-h.killed; killed != id {
		t.Errorf("TERMed job %d, want %d", killed, id)
	}
	if got := h.status(t, id); got != StatusCancelled {
		t.Fatalf("status = %q, want cancelled", got)
	}
	// The worker must not overwrite the cancellation with a failure.
	h.waitEvent(t, events.TypeDone, id)
	if got := h.status(t, id); got != StatusCancelled {
		t.Fatalf("status after the worker unwound = %q, want cancelled", got)
	}
}

func TestCancelAQueuedJob(t *testing.T) {
	h := newHarness(t)
	server := h.seedServer(t)
	id := h.queue(t, server, StatusQueued)

	if err := h.Cancel(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if got := h.status(t, id); got != StatusCancelled {
		t.Errorf("status = %q, want cancelled", got)
	}
	// A job that never ran was never on the NAS, so nothing is signalled there.
	select {
	case id := <-h.killed:
		t.Errorf("TERMed job %d, which never started", id)
	default:
	}
}

func TestCancelRejectsAFinishedJob(t *testing.T) {
	h := newHarness(t)
	server := h.seedServer(t)
	id := h.queue(t, server, StatusQueued)
	if err := h.store.SetJobStatus(context.Background(), id, StatusDone, "", nil); err != nil {
		t.Fatal(err)
	}
	if err := h.Cancel(context.Background(), id); !errors.Is(err, ErrIllegalTransition) {
		t.Errorf("err = %v, want ErrIllegalTransition", err)
	}
	if err := h.Cancel(context.Background(), 4040); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestRetryClonesAFinishedJob(t *testing.T) {
	h := newHarness(t)
	server := h.seedServer(t)
	id := h.queue(t, server, StatusQueued)
	if err := h.store.SetJobTotal(context.Background(), id, 4096); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetJobStatus(context.Background(), id, StatusFailed, "boom", nil); err != nil {
		t.Fatal(err)
	}

	newID, err := h.Retry(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	clone, err := h.store.GetJob(context.Background(), newID)
	if err != nil {
		t.Fatal(err)
	}
	if clone.Status != StatusQueued {
		t.Errorf("clone status = %q, want queued", clone.Status)
	}
	if clone.TotalBytes != 4096 {
		t.Errorf("clone total = %d, want the size carried over", clone.TotalBytes)
	}
	if clone.RemotePath != "/pub/f" || clone.DestPath != "/volume1/media" {
		t.Errorf("clone paths = %q → %q", clone.RemotePath, clone.DestPath)
	}
	// The original is left alone as a record of what happened.
	if got := h.status(t, id); got != StatusFailed {
		t.Errorf("original status = %q, want failed", got)
	}
}

func TestRetryRefusesALiveJob(t *testing.T) {
	h := newHarness(t)
	server := h.seedServer(t)
	id := h.queue(t, server, StatusQueued)
	if _, err := h.Retry(context.Background(), id); !errors.Is(err, ErrIllegalTransition) {
		t.Errorf("err = %v, want ErrIllegalTransition", err)
	}
	if _, err := h.Retry(context.Background(), 4040); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestRestartRecoveryRequeuesRunningJobs(t *testing.T) {
	h := newHarness(t)
	server := h.seedServer(t)
	id := h.queue(t, server, StatusRunning)
	// Some bytes already landed; a resume must keep them.
	if err := h.store.SetJobProgress(context.Background(), id, 512, 10, 5); err != nil {
		t.Fatal(err)
	}
	close(h.release)

	if err := h.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer h.Stop()

	if got := <-h.started; got != id {
		t.Fatalf("started job %d, want the interrupted %d", got, id)
	}
	h.waitEvent(t, events.TypeDone, id)
	job, err := h.store.GetJob(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != StatusDone {
		t.Errorf("status = %q, want done", job.Status)
	}
}

func TestStopInterruptsRunningJobs(t *testing.T) {
	h := newHarness(t)
	server := h.seedServer(t)
	id := h.queue(t, server, StatusQueued)

	if err := h.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-h.started
	h.Stop()
	h.Stop() // idempotent

	if got := h.status(t, id); got != StatusInterrupted {
		t.Errorf("status = %q, want interrupted so the next boot resumes it", got)
	}
}

func TestStopBeforeStartIsANoOp(t *testing.T) {
	h := newHarness(t)
	h.Stop()
}

func TestRetentionSweepDropsOldFinishedJobs(t *testing.T) {
	h := newHarness(t)
	server := h.seedServer(t)
	old := h.queue(t, server, StatusDone)
	live := h.queue(t, server, StatusQueued)
	if _, err := h.store.ExecContext(context.Background(),
		`UPDATE jobs SET created_at = datetime('now', '-100 days') WHERE id IN (?, ?)`,
		old, live); err != nil {
		t.Fatal(err)
	}

	h.purge(context.Background())

	if _, err := h.store.GetJob(context.Background(), old); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("the old finished job survived the sweep: %v", err)
	}
	if _, err := h.store.GetJob(context.Background(), live); err != nil {
		t.Errorf("the queued job was swept: %v", err)
	}

	// Retention off means retention off.
	if err := h.store.SetSettings(context.Background(),
		map[string]string{"history_retention_days": "0"}); err != nil {
		t.Fatal(err)
	}
	newDone := h.queue(t, server, StatusDone)
	if _, err := h.store.ExecContext(context.Background(),
		`UPDATE jobs SET created_at = datetime('now', '-100 days') WHERE id = ?`, newDone); err != nil {
		t.Fatal(err)
	}
	h.purge(context.Background())
	if _, err := h.store.GetJob(context.Background(), newDone); err != nil {
		t.Errorf("retention 0 should disable the sweep: %v", err)
	}
}

func TestSettingInt(t *testing.T) {
	s := map[string]string{"good": " 5 ", "bad": "x", "negative": "-1", "empty": ""}
	tests := []struct {
		key  string
		want int
	}{
		{"good", 5}, {"bad", 2}, {"negative", 2}, {"empty", 2}, {"missing", 2},
	}
	for _, tc := range tests {
		if got := settingInt(s, tc.key, 2); got != tc.want {
			t.Errorf("settingInt(%q) = %d, want %d", tc.key, got, tc.want)
		}
	}
}

func TestPercent(t *testing.T) {
	tests := []struct {
		done, total int64
		want        int
	}{
		{0, 0, 0}, {50, 100, 50}, {100, 100, 100}, {150, 100, 100}, {1, 0, 0}, {0, 100, 0},
	}
	for _, tc := range tests {
		if got := percent(tc.done, tc.total); got != tc.want {
			t.Errorf("percent(%d, %d) = %d, want %d", tc.done, tc.total, got, tc.want)
		}
	}
}
