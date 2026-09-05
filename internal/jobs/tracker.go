package jobs

import (
	"sync"
	"time"

	"sftp-relay/internal/nas"
)

// flushInterval keeps both the DB write and the SSE event down to 1 Hz per job.
// modernc's SQLite is slow under write contention, and nobody can read a
// progress bar that updates faster than this anyway.
const flushInterval = time.Second

// staleAfter is how long lftp may say nothing parseable before the destination
// size poll takes over. lftp's output format varies enough by version that this
// fallback is the only thing guaranteed to work everywhere.
const staleAfter = 10 * time.Second

// tracker accumulates progress for one job and calls flush at most once per
// second. Bytes only ever move forwards: a per-file line from a mirror must
// never drag the overall bar backwards.
type tracker struct {
	mu        sync.Mutex
	total     int64
	bytes     int64
	speed     int64
	eta       int64
	lastParse time.Time
	lastFlush time.Time
	dirty     bool

	now   func() time.Time
	flush func(bytes, speed, eta int64)
}

func newTracker(total int64, flush func(bytes, speed, eta int64)) *tracker {
	return &tracker{total: total, now: time.Now, flush: flush}
}

// observe folds in a parsed lftp line.
func (t *tracker) observe(p nas.Progress) {
	t.mu.Lock()
	t.lastParse = t.now()
	if p.Total > 0 {
		t.total = p.Total
	}
	if p.SpeedBPS > 0 {
		t.speed = p.SpeedBPS
	}
	if p.ETASeconds > 0 {
		t.eta = p.ETASeconds
	}
	// A percentage with a known total is more trustworthy than a per-file byte
	// count during a mirror, so prefer it when it implies more progress.
	if p.Percent >= 0 && t.total > 0 {
		if b := t.total * int64(p.Percent) / 100; b > t.bytes {
			t.bytes = b
		}
	}
	if p.Bytes > t.bytes {
		t.bytes = p.Bytes
	}
	t.dirty = true
	t.mu.Unlock()
	t.maybeFlush(false)
}

// observeSize folds in a destination size poll.
func (t *tracker) observeSize(n int64) {
	t.mu.Lock()
	if n > t.bytes {
		t.bytes = n
		t.dirty = true
	}
	t.mu.Unlock()
	t.maybeFlush(false)
}

// stale reports whether lftp has gone quiet long enough to fall back to polling.
func (t *tracker) stale() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lastParse.IsZero() || t.now().Sub(t.lastParse) >= staleAfter
}

// complete marks the job as fully transferred and flushes unconditionally.
func (t *tracker) complete() {
	t.mu.Lock()
	if t.total > t.bytes {
		t.bytes = t.total
	}
	t.speed, t.eta, t.dirty = 0, 0, true
	t.mu.Unlock()
	t.maybeFlush(true)
}

func (t *tracker) maybeFlush(force bool) {
	t.mu.Lock()
	now := t.now()
	if !t.dirty || (!force && now.Sub(t.lastFlush) < flushInterval) {
		t.mu.Unlock()
		return
	}
	t.lastFlush, t.dirty = now, false
	b, s, e := t.bytes, t.speed, t.eta
	t.mu.Unlock()
	t.flush(b, s, e)
}

// snapshot is the current state, for a status payload.
func (t *tracker) snapshot() (bytes, total, speed, eta int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.bytes, t.total, t.speed, t.eta
}
