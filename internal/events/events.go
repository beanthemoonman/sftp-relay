// Package events is the SSE fan-out hub. It holds no history: a client that
// reconnects gets a fresh snapshot from the database, not a replay.
package events

import (
	"log/slog"
	"sync"
	"time"
)

// Event types, mirroring the SSE contract in CLAUDE.md.
const (
	TypeCreated  = "job.created"
	TypeProgress = "job.progress"
	TypeDone     = "job.done"
	TypeFailed   = "job.failed"
	TypeLog      = "job.log"

	// TypeSnapshot is sent once on connect: the client resyncs from it rather
	// than from a replayed backlog, so nothing has to be buffered server-side.
	TypeSnapshot = "snapshot"
)

// Event is one message on the stream.
type Event struct {
	Type  string `json:"type"`
	JobID int64  `json:"job_id,omitempty"`
	Data  any    `json:"data,omitempty"`
}

// bufferSize is what a subscriber may fall behind by before it is dropped. A
// browser that cannot keep up with this is gone, not slow.
const bufferSize = 64

// progressInterval is the coalescing window: at most one progress event per
// job per second reaches subscribers, however fast lftp talks.
const progressInterval = time.Second

// Hub fans events out to every connected client.
type Hub struct {
	mu     sync.Mutex
	subs   map[int64]chan Event
	nextID int64
	// lastSent is the per-job coalescing clock. It stays bounded to in-flight
	// jobs: done/failed/cancelled all arrive as done/failed events (which clear
	// the entry), and an interrupted job is resumed and finished, not abandoned.
	lastSent map[int64]time.Time
	now      func() time.Time
}

// NewHub returns an empty hub. It owns no goroutines: publishing happens on the
// caller's goroutine, so there is nothing to shut down.
func NewHub() *Hub {
	return &Hub{
		subs:     map[int64]chan Event{},
		lastSent: map[int64]time.Time{},
		now:      time.Now,
	}
}

// Subscribe registers a client and returns its id and channel. The channel is
// closed when the subscriber is removed, whether by Unsubscribe or by falling
// too far behind.
func (h *Hub) Subscribe() (int64, <-chan Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nextID++
	id := h.nextID
	ch := make(chan Event, bufferSize)
	h.subs[id] = ch
	return id, ch
}

// Unsubscribe removes a client. It is safe to call more than once.
func (h *Hub) Unsubscribe(id int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.drop(id)
}

// drop must be called with the lock held.
func (h *Hub) drop(id int64) {
	if ch, ok := h.subs[id]; ok {
		delete(h.subs, id)
		close(ch)
	}
}

// Count reports the number of live subscribers.
func (h *Hub) Count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// Publish sends e to every subscriber. Progress events are coalesced per job;
// a subscriber whose buffer is full is dropped rather than allowed to make the
// publisher block or the buffer grow.
func (h *Hub) Publish(e Event) {
	h.mu.Lock()
	defer h.mu.Unlock()

	switch e.Type {
	case TypeProgress:
		now := h.now()
		if last, ok := h.lastSent[e.JobID]; ok && now.Sub(last) < progressInterval {
			return
		}
		h.lastSent[e.JobID] = now
	case TypeDone, TypeFailed:
		// The job is over; stop tracking its coalescing window.
		delete(h.lastSent, e.JobID)
	}

	for id, ch := range h.subs {
		select {
		case ch <- e:
		default:
			slog.Warn("events: subscriber too slow, dropping it", "subscriber", id)
			h.drop(id)
		}
	}
}
