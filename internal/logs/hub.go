// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package logs

import (
	"fmt"
	"io"
	"sync"
)

// SubscriberBuffer is how many lines a subscriber may fall behind before it
// is disconnected. It can then resume. Viewers don't subscribe: they Read at
// their own pace and are never disconnected.
const SubscriberBuffer = 1024

// Event is one line (Seq ≥ 1) or the end of the run (Done).
type Event struct {
	Seq    int64
	Text   string
	Done   bool
	Status string // final run status, with Done
}

// Hub is one run's log: a single writer (the run's output follower), any
// number of viewers.
//
// Every line gets a sequence number (its line number in log.txt), is written
// to log.txt, kept in memory while the run is active (at most the log cap),
// and sent to every subscriber. A slow subscriber never slows the run: sends
// never block, and a subscriber whose buffer is full is dropped.
type Hub struct {
	mu        sync.Mutex
	w         io.WriteCloser
	cap       int64
	bytes     int64
	lines     []string // lines[i] has Seq i+1
	truncated bool
	done      bool
	status    string
	err       error
	subs      map[*Subscription]struct{}
	changed   chan struct{} // closed at the next line or the end (made when a reader waits)
}

// NewHub writes to w (log.txt) up to capBytes.
func NewHub(w io.WriteCloser, capBytes int64) *Hub {
	return &Hub{w: w, cap: capBytes, subs: map[*Subscription]struct{}{}}
}

// Subscription receives events after its backlog.
type Subscription struct {
	C       <-chan Event
	c       chan Event
	hub     *Hub
	after   int64 // deliver only lines after this one
	evicted bool
}

// Evicted reports whether the subscription was dropped for falling behind.
func (s *Subscription) Evicted() bool {
	s.hub.mu.Lock()
	defer s.hub.mu.Unlock()
	return s.evicted
}

// Line appends one output line (implements isolation.LineSink). Past the
// cap, one truncation notice is written and further output is dropped; the
// run itself continues.
func (h *Hub) Line(s string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.done || h.truncated || h.err != nil {
		return
	}
	if h.bytes+int64(len(s))+1 > h.cap {
		h.truncated = true
		h.appendLocked(fmt.Sprintf("[nbsandbox] log truncated at %d bytes; further output was dropped (the run continues)", h.bytes))
		return
	}
	h.appendLocked(s)
}

func (h *Hub) appendLocked(s string) {
	if _, err := h.w.Write([]byte(s + "\n")); err != nil {
		h.err = err
		return
	}
	h.bytes += int64(len(s)) + 1
	h.lines = append(h.lines, s)
	h.wakeLocked()
	ev := Event{Seq: int64(len(h.lines)), Text: s}
	for sub := range h.subs {
		if ev.Seq <= sub.after {
			continue // the viewer already has it (resumed past this point)
		}
		select {
		case sub.c <- ev:
		default: // too slow: drop it rather than wait
			sub.evicted = true
			delete(h.subs, sub)
			close(sub.c)
		}
	}
}

// Subscribe returns the lines after `after` that already exist, and a
// subscription for every later line, with no gap and no duplicate: both are
// taken under the same lock. For a finished run the backlog is
// the rest of the log and the channel delivers only the Done event.
func (h *Hub) Subscribe(after int64) ([]Event, *Subscription) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if after < 0 {
		after = 0
	}
	var backlog []Event
	for i := after; i < int64(len(h.lines)); i++ {
		backlog = append(backlog, Event{Seq: i + 1, Text: h.lines[i]})
	}
	c := make(chan Event, SubscriberBuffer)
	sub := &Subscription{C: c, c: c, hub: h, after: after}
	if h.done {
		c <- Event{Done: true, Status: h.status}
		close(c)
		return backlog, sub
	}
	h.subs[sub] = struct{}{}
	return backlog, sub
}

// Unsubscribe stops delivery (the viewer went away).
func (h *Hub) Unsubscribe(sub *Subscription) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.subs[sub]; ok {
		delete(h.subs, sub)
		close(sub.c)
	}
}

// Close ends the log with the run's final status: every subscriber gets a
// Done event, and log.txt is closed.
func (h *Hub) Close(status string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.done {
		return nil
	}
	h.done, h.status = true, status
	h.wakeLocked()
	for sub := range h.subs {
		select {
		case sub.c <- Event{Done: true, Status: status}:
		default:
			sub.evicted = true // it will resume and read the end from disk
		}
		delete(h.subs, sub)
		close(sub.c)
	}
	// Lines stay available to viewers that already hold the hub (one that
	// subscribes right after the end still gets every line); the registry
	// drops the hub, and later viewers read log.txt.
	return h.w.Close()
}

func (h *Hub) wakeLocked() {
	if h.changed != nil {
		close(h.changed)
		h.changed = nil
	}
}

// Read returns up to limit lines after `after`. With none, it says whether the
// log has ended (and the run's status), or returns a channel closed when
// there is more to read. A reader goes at its own pace: the lines stay in
// memory while the run is active (at most the log cap) and for as long as
// the reader holds the hub, so a slow reader costs nothing and is never
// disconnected, and it never slows the run.
func (h *Hub) Read(after int64, limit int) (lines []Event, done bool, status string, more <-chan struct{}) {
	h.mu.Lock()
	defer h.mu.Unlock()
	after = min(int64(len(h.lines)), max(after, 0))
	end := min(int64(len(h.lines)), after+int64(limit))
	for i := after; i < end; i++ {
		lines = append(lines, Event{Seq: i + 1, Text: h.lines[i]})
	}
	if len(lines) > 0 || h.done {
		return lines, len(lines) == 0 && h.done, h.status, nil
	}
	if h.changed == nil {
		h.changed = make(chan struct{})
	}
	return nil, false, "", h.changed
}

// Stats reports what was written.
func (h *Hub) Stats() (lines, bytes int64, truncated bool, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return int64(len(h.lines)), h.bytes, h.truncated, h.err
}

// Subscribers is the number of live viewers.
func (h *Hub) Subscribers() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// Registry finds the hub of an active run.
type Registry struct {
	mu   sync.Mutex
	hubs map[string]*Hub
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{hubs: map[string]*Hub{}} }

// Open creates and registers the hub for a run.
func (r *Registry) Open(runID string, w io.WriteCloser, capBytes int64) *Hub {
	h := NewHub(w, capBytes)
	r.mu.Lock()
	r.hubs[runID] = h
	r.mu.Unlock()
	return h
}

// Get returns the run's hub, or nil if the run isn't executing.
func (r *Registry) Get(runID string) *Hub {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hubs[runID]
}

// Remove forgets the run's hub (after Close).
func (r *Registry) Remove(runID string) {
	r.mu.Lock()
	delete(r.hubs, runID)
	r.mu.Unlock()
}
