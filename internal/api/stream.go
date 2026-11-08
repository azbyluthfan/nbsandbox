// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/logs"
	"github.com/azbyluthfan/nbsandbox/internal/runs"
	"github.com/azbyluthfan/nbsandbox/internal/spaces"
)

// DefaultHeartbeat keeps idle streams alive through proxies.
const DefaultHeartbeat = 15 * time.Second

// sse writes Server-Sent Events, flushing after each one.
type sse struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

func (s sse) send(chunk string) error {
	if _, err := s.w.Write([]byte(chunk)); err != nil {
		return err
	}
	return s.rc.Flush()
}

// line writes one log line without flushing (the caller flushes after a
// batch). The event id is the line number, so a client that reconnects with
// Last-Event-ID continues after it. A carriage return would end an SSE line
// early, so it is removed.
func (s sse) line(ev logs.Event) error {
	text := strings.ReplaceAll(ev.Text, "\r", "")
	_, err := fmt.Fprintf(s.w, "id: %d\nevent: line\ndata: %s\n\n", ev.Seq, text)
	return err
}

// lineBatch is the most log lines written between flushes: a burst goes out
// in a few writes, not one per line, so viewers keep up with it.
const lineBatch = 256

func (s sse) event(name string, v any) error {
	b, _ := json.Marshal(v)
	return s.send(fmt.Sprintf("event: %s\ndata: %s\n\n", name, b))
}

// resumePoint is the last line the client already has: the Last-Event-ID
// header (sent automatically by browsers on reconnect) or ?after=.
func resumePoint(r *http.Request) (int64, error) {
	v := r.Header.Get("Last-Event-ID")
	if v == "" {
		v = r.URL.Query().Get("after")
	}
	if v == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0, &spaces.InvalidError{Msg: "Last-Event-ID / after must be a non-negative line number"}
	}
	return n, nil
}

// openSSE starts a Server-Sent Events response and returns the writer and a
// heartbeat ticker (stop it when done).
func (a *API) openSSE(w http.ResponseWriter, comment string) (sse, *time.Ticker, bool) {
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{}) // streams are long-lived
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no") // tell proxies not to buffer
	w.WriteHeader(http.StatusOK)
	out := sse{w: w, rc: rc}
	every := a.d.StreamHeartbeat
	if every <= 0 {
		every = DefaultHeartbeat
	}
	return out, time.NewTicker(every), out.send(": "+comment+"\n\n") == nil
}

const shutdownMessage = "nbsandboxd is shutting down; reconnect with Last-Event-ID"

// streamRunLog is GET .../log/stream: the run's log lines as "line" events
// (id = line number), then "done" with the final status (runs.FollowLog).
func (a *API) streamRunLog(w http.ResponseWriter, r *http.Request) {
	after, err := resumePoint(r)
	if err != nil {
		writeRunError(w, r, err)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	items, err := a.d.Runs.FollowLog(ctx, user(r), r.PathValue("id"), r.PathValue("run_id"), after)
	if err != nil {
		writeRunError(w, r, err)
		return
	}
	out, heartbeat, ok := a.openSSE(w, "nbsandbox log stream")
	defer heartbeat.Stop()
	if !ok {
		return
	}
	if a.d.Metrics != nil {
		a.d.Metrics.ActiveLogStreams.Inc()
		defer a.d.Metrics.ActiveLogStreams.Dec()
	}
	last := after
	// handle sends one item; false ends the stream. Lines are flushed in
	// batches: after the lines already waiting, at most lineBatch at a time.
	handle := func(it runs.LogItem) bool {
		switch it.Kind {
		case runs.LogLine:
			if out.line(it.Line) != nil {
				return false
			}
			last = it.Line.Seq
			return true
		case runs.LogDone:
			_ = out.event("done", map[string]any{"status": it.Status, "last_event_id": it.LastSeq})
		case runs.LogDropped:
			_ = out.event("dropped", map[string]any{"message": "too far behind; reconnect with Last-Event-ID", "last_event_id": it.LastSeq})
		}
		return false
	}
	for {
		select {
		case it, open := <-items:
			if !open || !handle(it) {
				return
			}
		batch:
			for n := 1; n < lineBatch; n++ {
				select {
				case it, open := <-items:
					if !open || !handle(it) {
						return
					}
				default:
					break batch
				}
			}
			if out.rc.Flush() != nil {
				return
			}
		case <-heartbeat.C:
			if out.send(": heartbeat\n\n") != nil {
				return
			}
		case <-a.d.Shutdown:
			_ = out.event("shutdown", map[string]any{"message": shutdownMessage, "last_event_id": last})
			return
		}
	}
}
