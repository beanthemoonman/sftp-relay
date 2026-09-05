package events

import (
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// clock is a hand-cranked time source: coalescing is asserted by advancing it,
// never by sleeping.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newTestHub(c *clock) *Hub {
	h := NewHub()
	h.now = c.now
	return h
}

func drain(ch <-chan Event) []Event {
	var out []Event
	for {
		select {
		case e, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, e)
		default:
			return out
		}
	}
}

func TestSubscribeAndPublish(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	h := newTestHub(c)

	id1, ch1 := h.Subscribe()
	_, ch2 := h.Subscribe()
	if h.Count() != 2 {
		t.Fatalf("Count = %d, want 2", h.Count())
	}

	h.Publish(Event{Type: TypeCreated, JobID: 7})
	for i, ch := range []<-chan Event{ch1, ch2} {
		got := drain(ch)
		if len(got) != 1 || got[0].JobID != 7 {
			t.Errorf("subscriber %d got %+v", i, got)
		}
	}

	h.Unsubscribe(id1)
	if h.Count() != 1 {
		t.Fatalf("Count after unsubscribe = %d, want 1", h.Count())
	}
	if _, open := <-ch1; open {
		t.Error("unsubscribed channel should be closed")
	}
	h.Unsubscribe(id1) // idempotent
	h.Unsubscribe(9999)

	h.Publish(Event{Type: TypeDone, JobID: 8})
	if got := drain(ch2); len(got) != 1 {
		t.Errorf("remaining subscriber got %+v", got)
	}
}

func TestProgressIsCoalescedPerJobPerSecond(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	h := newTestHub(c)
	_, ch := h.Subscribe()

	for range 20 {
		h.Publish(Event{Type: TypeProgress, JobID: 1})
	}
	if got := drain(ch); len(got) != 1 {
		t.Fatalf("burst produced %d events, want 1", len(got))
	}

	// A second job has its own window.
	h.Publish(Event{Type: TypeProgress, JobID: 2})
	if got := drain(ch); len(got) != 1 {
		t.Fatalf("second job produced %d events, want 1", len(got))
	}

	// Just short of a second is still suppressed; a full second is not.
	c.advance(999 * time.Millisecond)
	h.Publish(Event{Type: TypeProgress, JobID: 1})
	if got := drain(ch); len(got) != 0 {
		t.Fatalf("inside the window produced %d events, want 0", len(got))
	}
	c.advance(time.Millisecond)
	h.Publish(Event{Type: TypeProgress, JobID: 1})
	if got := drain(ch); len(got) != 1 {
		t.Fatalf("after the window produced %d events, want 1", len(got))
	}
}

func TestTerminalEventsAreNeverCoalescedAndClearTheWindow(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	h := newTestHub(c)
	_, ch := h.Subscribe()

	h.Publish(Event{Type: TypeProgress, JobID: 1})
	h.Publish(Event{Type: TypeDone, JobID: 1})
	h.Publish(Event{Type: TypeFailed, JobID: 1})
	h.Publish(Event{Type: TypeLog, JobID: 1, Data: "line"})
	if got := drain(ch); len(got) != 4 {
		t.Fatalf("got %d events, want 4 (only progress coalesces)", len(got))
	}
	// job.done cleared the window, so the next progress event is not suppressed.
	h.Publish(Event{Type: TypeProgress, JobID: 1})
	if got := drain(ch); len(got) != 1 {
		t.Fatalf("progress after done produced %d events, want 1", len(got))
	}
}

func TestSlowSubscriberIsDroppedNotBuffered(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	h := newTestHub(c)
	_, slow := h.Subscribe()
	_, fast := h.Subscribe()

	// Non-progress events bypass coalescing, so this really does overrun the buffer.
	for i := range bufferSize + 5 {
		h.Publish(Event{Type: TypeLog, JobID: 1, Data: i})
		drain(fast) // the fast subscriber keeps up
	}
	if h.Count() != 1 {
		t.Fatalf("Count = %d, want 1: the slow subscriber should have been dropped", h.Count())
	}
	got := drain(slow)
	if len(got) != bufferSize {
		t.Errorf("slow subscriber buffered %d events, want exactly %d", len(got), bufferSize)
	}
	if _, open := <-slow; open {
		t.Error("dropped subscriber's channel should be closed")
	}
}

func TestConcurrentSubscribeUnsubscribeAndPublish(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	h := newTestHub(c)

	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				id, ch := h.Subscribe()
				go drain(ch)
				h.Publish(Event{Type: TypeLog, JobID: 1, Data: "x"})
				h.Unsubscribe(id)
			}
		}()
	}
	wg.Wait()
	if h.Count() != 0 {
		t.Errorf("Count = %d, want 0 after every subscriber unsubscribed", h.Count())
	}
}
