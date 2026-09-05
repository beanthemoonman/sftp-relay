package jobs

import (
	"sync"
	"testing"
	"time"

	"sftp-relay/internal/nas"
)

type flush struct {
	bytes, speed, eta int64
}

// recorder collects flushes and drives the tracker's clock by hand, so nothing
// in these tests depends on wall-clock timing.
type recorder struct {
	mu      sync.Mutex
	now     time.Time
	flushes []flush
}

func newRecorded(total int64) (*tracker, *recorder) {
	r := &recorder{now: time.Unix(1000, 0)}
	tr := newTracker(total, func(b, s, e int64) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.flushes = append(r.flushes, flush{b, s, e})
	})
	tr.now = func() time.Time {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.now
	}
	return tr, r
}

func (r *recorder) advance(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.now = r.now.Add(d)
}

func (r *recorder) all() []flush {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]flush(nil), r.flushes...)
}

func TestTrackerFlushesAtMostOncePerSecond(t *testing.T) {
	tr, r := newRecorded(1000)

	// The first observation flushes immediately: lastFlush is the zero time.
	tr.observe(nas.Progress{Bytes: 100, Percent: 10, SpeedBPS: 50, ETASeconds: 18})
	for i := range 10 {
		tr.observe(nas.Progress{Bytes: int64(101 + i), Percent: 10, SpeedBPS: 50})
	}
	if got := r.all(); len(got) != 1 {
		t.Fatalf("burst produced %d flushes, want 1: %+v", len(got), got)
	}

	r.advance(time.Second)
	tr.observe(nas.Progress{Bytes: 500, Percent: 50, SpeedBPS: 60, ETASeconds: 8})
	got := r.all()
	if len(got) != 2 {
		t.Fatalf("after a second: %d flushes, want 2", len(got))
	}
	if got[1] != (flush{500, 60, 8}) {
		t.Errorf("second flush = %+v", got[1])
	}
}

func TestTrackerBytesNeverGoBackwards(t *testing.T) {
	tr, r := newRecorded(1000)
	tr.observe(nas.Progress{Bytes: 800, Percent: 80})
	r.advance(time.Second)
	// A mirror's per-file line reports a small count for a different file.
	tr.observe(nas.Progress{Bytes: 12, Percent: 1})
	r.advance(time.Second)
	tr.observeSize(5)

	got := r.all()
	for _, f := range got {
		if f.bytes < 800 {
			t.Fatalf("progress went backwards: %+v", got)
		}
	}
}

func TestTrackerFallsBackToDestinationSize(t *testing.T) {
	tr, r := newRecorded(1000)
	tr.observeSize(300)
	got := r.all()
	if len(got) != 1 || got[0].bytes != 300 {
		t.Fatalf("flushes = %+v, want one at 300 bytes", got)
	}
	if b, _, _, _ := tr.snapshot(); b != 300 {
		t.Errorf("bytes = %d, want 300", b)
	}
}

func TestTrackerPrefersPercentageWhenItImpliesMoreProgress(t *testing.T) {
	tr, _ := newRecorded(1000)
	tr.observe(nas.Progress{Bytes: 10, Percent: 50})
	if b, _, _, _ := tr.snapshot(); b != 500 {
		t.Errorf("bytes = %d, want 500 derived from the percentage", b)
	}
}

func TestTrackerLearnsTheTotalFromOutput(t *testing.T) {
	tr, _ := newRecorded(0)
	tr.observe(nas.Progress{Bytes: 512, Total: 2048, Percent: 25})
	b, total, _, _ := tr.snapshot()
	if total != 2048 || b != 512 {
		t.Errorf("snapshot = %d/%d, want 512/2048", b, total)
	}
}

func TestTrackerIgnoresZeroSpeedAndETA(t *testing.T) {
	tr, r := newRecorded(1000)
	tr.observe(nas.Progress{Bytes: 100, SpeedBPS: 500, ETASeconds: 9})
	r.advance(time.Second)
	tr.observe(nas.Progress{Bytes: 200, SpeedBPS: 0, ETASeconds: 0})
	got := r.all()
	if got[1].speed != 500 || got[1].eta != 9 {
		t.Errorf("a line without speed or eta clobbered the last known values: %+v", got[1])
	}
}

func TestTrackerStaleness(t *testing.T) {
	tr, r := newRecorded(1000)
	if !tr.stale() {
		t.Error("a tracker that has never parsed a line is stale")
	}
	tr.observe(nas.Progress{Bytes: 1})
	if tr.stale() {
		t.Error("a tracker that just parsed a line is not stale")
	}
	r.advance(staleAfter - time.Nanosecond)
	if tr.stale() {
		t.Error("stale one nanosecond early")
	}
	r.advance(time.Nanosecond)
	if !tr.stale() {
		t.Error("not stale at the threshold")
	}
}

func TestTrackerCompleteFlushesUnconditionally(t *testing.T) {
	tr, r := newRecorded(1000)
	tr.observe(nas.Progress{Bytes: 100, SpeedBPS: 5, ETASeconds: 3})
	tr.complete() // inside the one-second window, but forced

	got := r.all()
	last := got[len(got)-1]
	if last != (flush{1000, 0, 0}) {
		t.Errorf("final flush = %+v, want the full total with no speed or eta", last)
	}
}

func TestTrackerSkipsFlushWhenNothingChanged(t *testing.T) {
	tr, r := newRecorded(1000)
	tr.observe(nas.Progress{Bytes: 100})
	before := len(r.all())
	r.advance(time.Minute)
	tr.observeSize(50) // lower than what we have: not a change
	if got := len(r.all()); got != before {
		t.Errorf("flushes = %d, want %d: nothing changed", got, before)
	}
}

func TestTrackerIsSafeUnderConcurrency(t *testing.T) {
	tr, _ := newRecorded(10000)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := range 100 {
				tr.observe(nas.Progress{Bytes: int64(i*100 + j), SpeedBPS: 1})
				tr.observeSize(int64(j))
				tr.stale()
				tr.snapshot()
			}
		}(i)
	}
	wg.Wait()
	if b, _, _, _ := tr.snapshot(); b != 799 {
		t.Errorf("bytes = %d, want the highest observation 799", b)
	}
}
