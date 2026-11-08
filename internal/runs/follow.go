// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package runs

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/logs"
	"github.com/azbyluthfan/nbsandbox/internal/spaces"
	"github.com/azbyluthfan/nbsandbox/internal/store"
)

// The following methods are what the HTTP (SSE) and gRPC handlers stream.
// They decide what to send; the handlers only encode it, add keep-alives,
// and stop at shutdown.

// LogItemKind says what a LogItem carries.
type LogItemKind int

const (
	// LogLine is one numbered line of the run's console output.
	LogLine LogItemKind = iota
	// LogDone ends the stream: the run finished, with Status.
	LogDone
	// LogDropped ends the stream: the reader fell too far behind and should
	// resume after LastSeq. Viewers of a run on this node read at their own
	// pace and never get it; it stays for clients written against it.
	LogDropped
)

// LogItem is one item of a followed log.
type LogItem struct {
	Kind    LogItemKind
	Line    logs.Event // LogLine
	Status  string     // LogDone
	LastSeq int64      // LogDone, LogDropped: the last line sent
}

// How often FollowLog re-checks a run that hasn't opened its log yet: a
// queued run slowly, a running one (its log opens in moments) quickly, so
// the first lines arrive live rather than as a backlog.
const (
	queuedPoll  = 250 * time.Millisecond
	startedPoll = 20 * time.Millisecond
)

// FollowLog streams a run's log after line `after`: earlier lines first,
// then live lines as the run produces them, then LogDone with the final
// status. A run that hasn't started yet is waited for; a finished run's log
// is replayed and the stream ends. The channel closes after the last item or
// when ctx ends. Access is checked before it returns.
func (m *Manager) FollowLog(ctx context.Context, owner, spaceID, runID string, after int64) (<-chan LogItem, error) {
	if _, err := m.RunStatus(ctx, owner, spaceID, runID); err != nil {
		return nil, err
	}
	if after < 0 {
		return nil, &spaces.InvalidError{Msg: "the resume point must be a non-negative line number"}
	}
	out := make(chan LogItem)
	go func() {
		defer close(out)
		send := func(it LogItem) bool {
			select {
			case out <- it:
				return true
			case <-ctx.Done():
				return false
			}
		}
		for {
			// Wake-ups first, so no change slips between them and the reads.
			wake, logWake := m.Wake(spaceID), m.logWake.Wait(runID)
			if hub := m.Logs().Get(runID); hub != nil {
				m.followHub(ctx, hub, after, send) // executing here
				return
			}
			r, err := m.d.DB.GetRun(ctx, spaceID, runID)
			if err != nil {
				return
			}
			if r.Terminal() {
				n, err := m.replayLog(ctx, owner, spaceID, runID, after, send)
				if err == nil {
					send(LogItem{Kind: LogDone, Status: r.Status, LastSeq: n})
				}
				return
			}
			// Queued, or running here but its log isn't open yet, or
			// running on another worker: its lines so far, from the
			// database, then wait for more or for a status change.
			poll := queuedPoll
			if r.Status == StatusRunning {
				if after, err = m.sendChunks(ctx, runID, after, send); err != nil {
					return
				}
				poll = time.Second
				if r.Worker == m.cfg.WorkerID && !m.cfg.NoExecute {
					poll = startedPoll // the log opens in moments
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-wake:
			case <-logWake:
			case <-time.After(poll):
			}
		}
	}()
	return out, nil
}

// sendChunks sends the live log lines after `after` that another worker
// recorded in the database, and returns the last line number sent.
func (m *Manager) sendChunks(ctx context.Context, runID string, after int64, send func(LogItem) bool) (int64, error) {
	chunks, err := m.d.DB.LogChunks(ctx, runID, after)
	if err != nil {
		return after, err
	}
	for _, c := range chunks {
		seq := c.FirstSeq
		for _, line := range c.Lines() {
			if seq > after {
				if !send(LogItem{Kind: LogLine, Line: logs.Event{Seq: seq, Text: line}}) {
					return after, ctx.Err()
				}
				after = seq
			}
			seq++
		}
	}
	return after, nil
}

func terminal(status string) bool { return status != StatusQueued && status != StatusRunning }

func (m *Manager) followHub(ctx context.Context, hub *logs.Hub, after int64, send func(LogItem) bool) {
	for {
		lines, done, status, more := hub.Read(after, followBatch)
		for _, ev := range lines {
			if !send(LogItem{Kind: LogLine, Line: ev}) {
				return
			}
			after = ev.Seq
		}
		switch {
		case len(lines) > 0:
		case done:
			send(LogItem{Kind: LogDone, Status: status, LastSeq: after})
			return
		default:
			select {
			case <-more:
			case <-ctx.Done():
				return
			}
		}
	}
}

// followBatch is how many lines a viewer takes from the hub at a time.
const followBatch = 256

// replayLog sends the lines of a finished run's log after `after` and
// returns the last line number.
func (m *Manager) replayLog(ctx context.Context, owner, spaceID, runID string, after int64, send func(LogItem) bool) (int64, error) {
	f, _, err := m.OpenOutput(ctx, owner, spaceID, runID, spaces.RunLog)
	if err != nil {
		// Never started, or its worker went away before storing the log:
		// whatever lines it shipped.
		n, err := m.sendChunks(ctx, runID, after, send)
		return max(n, after), err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	var n int64
	for sc.Scan() {
		n++
		if n <= after {
			continue
		}
		if !send(LogItem{Kind: LogLine, Line: logs.Event{Seq: n, Text: sc.Text()}}) {
			return n, ctx.Err()
		}
	}
	return max(n, after), sc.Err()
}

// EventItemKind says what an EventItem carries.
type EventItemKind int

const (
	// EventStatus is a status change (Seq > 0), or a finished run's result
	// sent in place of its pruned final event (Seq == 0).
	EventStatus EventItemKind = iota
	// EventPruned warns that events after the resume point were pruned; the
	// reader should re-read the runs it waits for. Sent once.
	EventPruned
)

// EventItem is one item of a followed event stream.
type EventItem struct {
	Kind    EventItemKind
	Seq     int64           // EventStatus; 0 when it isn't a stored event
	Type    string          // EventStatus: status, partial, progress or cell
	Status  string          // EventStatus: the run's status (at the time)
	Data    json.RawMessage // EventStatus: a StatusEvent
	Message string          // EventPruned
}

// FollowEvents streams status events after q.After: the stored ones first,
// then new ones as they are recorded. A per-run stream (q.RunID) ends after
// the run's final event; a space stream runs until ctx ends. Access is
// checked before it returns.
func (m *Manager) FollowEvents(ctx context.Context, owner, spaceID string, q EventsQuery) (<-chan EventItem, error) {
	if q.RunID != "" {
		if err := m.RunExists(ctx, owner, spaceID, q.RunID); err != nil {
			return nil, err
		}
	} else if err := m.d.Spaces.Owned(ctx, owner, spaceID); err != nil {
		return nil, err
	}
	if q.Function != "" && !spaces.ValidFunctionName(q.Function) {
		return nil, &spaces.InvalidError{Msg: "function is not a valid function name"}
	}
	if q.After < 0 {
		return nil, &spaces.InvalidError{Msg: "the resume point must be a non-negative event number"}
	}
	for k := range q.Include {
		if k != store.EventProgress && k != store.EventCell {
			return nil, &spaces.InvalidError{Msg: "include takes progress and cell"}
		}
	}
	out := make(chan EventItem)
	go func() {
		defer close(out)
		send := func(it EventItem) bool {
			select {
			case out <- it:
				return true
			case <-ctx.Done():
				return false
			}
		}
		perRun := q.RunID != ""
		warnedPruned := false
		for {
			evs, wake, err := m.Events(ctx, owner, spaceID, q)
			switch {
			case errors.Is(err, ErrEventsPruned):
				if !warnedPruned {
					warnedPruned = true
					if !send(EventItem{Kind: EventPruned, Message: err.Error() + "; re-read the runs you are waiting for"}) {
						return
					}
				}
			case err != nil:
				return
			}
			for _, ev := range evs {
				if ev.Kind != store.EventStatus && ev.Kind != "" && !q.Include[ev.Kind] {
					q.After = ev.Seq // not asked for
					continue
				}
				if !send(EventItem{Kind: EventStatus, Seq: ev.Seq, Type: ev.Kind, Status: ev.Status, Data: json.RawMessage(ev.Data)}) {
					return
				}
				q.After = ev.Seq
				if perRun && ev.Kind == store.EventStatus && terminal(ev.Status) {
					return
				}
			}
			if perRun && len(evs) == 0 {
				// The run finished but its final event was pruned: send the
				// result instead, and end.
				if res, err := m.GetResult(ctx, owner, spaceID, q.RunID); err == nil {
					b, _ := json.Marshal(StatusEvent{RunID: res.RunID, Status: res.Status, Function: res.Function,
						Version: res.Version, Result: &res})
					send(EventItem{Kind: EventStatus, Type: store.EventStatus, Status: res.Status, Data: b})
					return
				}
			}
			select {
			case <-wake:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// WaitResult waits up to `wait` for a run to finish. It returns the result
// once the run has finished; otherwise, when the wait ends (or stop is
// closed), the run as it is now, with done false. It sleeps on the space's
// status notifications, not a poll.
func (m *Manager) WaitResult(ctx context.Context, owner, spaceID, runID string, wait time.Duration, stop <-chan struct{}) (res CallerResult, now View, done bool, err error) {
	if err := m.RunExists(ctx, owner, spaceID, runID); err != nil {
		return CallerResult{}, View{}, false, err
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		wake := m.Wake(spaceID) // before reading, so no change slips between
		res, err := m.GetResult(ctx, owner, spaceID, runID)
		var sa *ErrStillActive
		switch {
		case err == nil:
			return res, View{}, true, nil
		case !errors.As(err, &sa):
			return CallerResult{}, View{}, false, err
		}
		select {
		case <-wake:
			continue
		case <-ctx.Done():
			return CallerResult{}, View{}, false, ctx.Err()
		case <-deadline.C:
		case <-stop:
		}
		v, err := m.Get(ctx, owner, spaceID, runID)
		return CallerResult{}, v, false, err
	}
}
