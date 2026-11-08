// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package runs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/contract"
	"github.com/azbyluthfan/nbsandbox/internal/isolation"
	"github.com/azbyluthfan/nbsandbox/internal/logs"
	"github.com/azbyluthfan/nbsandbox/internal/store"
)

// What a run reports while it works arrives as JSON lines in its events
// file: the items of a streamed output (nbsandbox.emit), progress
// (nbsandbox.progress), and the runner's cell starts and ends. The worker
// reads them as they come, checks them, and records them as events in
// Postgres before anyone sees them, so whatever a caller has received can
// always be given again, whatever happens to the run afterwards.

// RunEventsFile is the file in the run's folder the runner and the notebook
// append to.
const RunEventsFile = isolation.RunEventsFile

// Limits. Items are the run's output: one that breaks a limit fails the run
// (dropping it would corrupt the output). Progress is only information: past
// its rate it is dropped, with one note in the log.
const (
	MaxRunEventBytes  = 64 << 10 // one line: one item, progress update or cell event
	MaxRunItems       = 1000     // items per run
	ItemsPerSec       = 20       // nbsandbox.emit paces itself to 10 a second; this catches a notebook writing the file itself
	ProgressPerSec    = 10
	runEventsPoll     = 250 * time.Millisecond
	runEventsReadSize = 1 << 20
)

// RunEvent is one thing a run reported while it worked: an item of its
// output, progress, or a cell starting or finishing.
type RunEvent struct {
	RunID    string `json:"run_id"`
	Kind     string `json:"kind"` // item | progress | cell
	Function string `json:"function,omitempty"`
	Version  int    `json:"version,omitempty"`
	// item: its number (1, 2, ... in the order emitted), the total if the
	// notebook declared one, and the item itself
	Index int             `json:"index,omitempty"`
	Total *int            `json:"total,omitempty"`
	Item  json.RawMessage `json:"item,omitempty"`
	// progress
	Fraction *float64 `json:"fraction,omitempty"`
	Message  string   `json:"message,omitempty"`
	// cell
	CellIndex       *int     `json:"cell_index,omitempty"`
	State           string   `json:"state,omitempty"`   // started | finished
	Outcome         string   `json:"outcome,omitempty"` // ok | error, when finished
	DurationSeconds *float64 `json:"duration_seconds,omitempty"`
}

// OutputError ends a run whose output broke its contract or a limit.
type OutputError struct {
	Kind string // KindOutputInvalid or KindOutputTooLarge
	Msg  string
}

func (e *OutputError) Error() string { return e.Msg }

// parseRunEvent checks one line of the events file.
func parseRunEvent(line []byte) (RunEvent, error) {
	var raw struct {
		Kind            string          `json:"kind"`
		Item            json.RawMessage `json:"item"`
		Total           *int            `json:"total"`
		Fraction        *float64        `json:"fraction"`
		Message         string          `json:"message"`
		CellIndex       *int            `json:"cell_index"`
		State           string          `json:"state"`
		Outcome         string          `json:"outcome"`
		DurationSeconds *float64        `json:"duration_seconds"`
	}
	if err := json.Unmarshal(line, &raw); err != nil {
		return RunEvent{}, errors.New("not a JSON object")
	}
	ev := RunEvent{Kind: raw.Kind}
	switch raw.Kind {
	case store.EventItem:
		if len(raw.Item) == 0 {
			return RunEvent{}, errors.New("an item event needs the item")
		}
		if raw.Total != nil && *raw.Total < 1 {
			return RunEvent{}, errors.New("an item's total must be at least 1")
		}
		ev.Item, ev.Total = raw.Item, raw.Total
	case store.EventProgress:
		if raw.Fraction == nil || *raw.Fraction < 0 || *raw.Fraction > 1 {
			return RunEvent{}, errors.New("progress needs a fraction from 0 to 1")
		}
		if len(raw.Message) > 1000 {
			raw.Message = raw.Message[:1000]
		}
		ev.Fraction, ev.Message = raw.Fraction, raw.Message
	case store.EventCell:
		if raw.CellIndex == nil || *raw.CellIndex < 0 || (raw.State != "started" && raw.State != "finished") ||
			(raw.State == "finished" && raw.Outcome != "ok" && raw.Outcome != "error") {
			return RunEvent{}, errors.New("a cell event needs cell_index, state and (when finished) outcome")
		}
		ev.CellIndex, ev.State, ev.Outcome, ev.DurationSeconds = raw.CellIndex, raw.State, raw.Outcome, raw.DurationSeconds
	default:
		return RunEvent{}, fmt.Errorf("unknown kind %q", raw.Kind)
	}
	return ev, nil
}

// eventTail follows one run's events file.
type eventTail struct {
	e        *executor
	r        store.Run
	hub      *logs.Hub
	contract *contract.Contract // the output's mode and item schema
	offset   int64
	buf      []byte // an incomplete last line
	skip     bool   // discarding the rest of an over-long line
	items    int
	bytes    int64 // the items' total size, against runs.max_output
	itemTok  float64
	progTok  float64
	last     time.Time
	noted    map[string]bool
	failed   *OutputError // set once an item broke the contract or a limit
}

// tailRunEvents follows the run's events file until stop is called; stop
// reads what is left, records it, and returns the output error that ended
// the run, if any.
func (e *executor) tailRunEvents(ctx context.Context, r store.Run, hub *logs.Hub) (stop func() *OutputError) {
	c, err := contract.Compile(nil, json.RawMessage(r.OutputSchema))
	if err != nil {
		c = &contract.Contract{}
	}
	t := &eventTail{e: e, r: r, hub: hub, contract: c.WithMode(r.OutputMode, true), itemTok: ItemsPerSec,
		progTok: ProgressPerSec, last: time.Now(), noted: map[string]bool{}}
	ctx = context.WithoutCancel(ctx) // recorded even as the run is stopped
	done, finished := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		tick := time.NewTicker(runEventsPoll)
		defer tick.Stop()
		for {
			select {
			case <-done:
				t.read(ctx) // the last lines, written just before the run ended
				return
			case <-tick.C:
				t.read(ctx)
			}
		}
	}()
	return func() *OutputError {
		close(done)
		<-finished
		return t.failed
	}
}

func (t *eventTail) note(reason, msg string) {
	if !t.noted[reason] && t.hub != nil {
		t.noted[reason] = true
		t.hub.Line("[nbsandbox] " + msg)
	}
}

// fail ends the run: its output broke the contract or a limit.
func (t *eventTail) fail(kind, msg string) {
	if t.failed != nil {
		return
	}
	t.failed = &OutputError{Kind: kind, Msg: msg}
	t.note("failed", msg)
	t.e.Cancel(t.r.ID, t.failed)
}

// read records the complete lines written since the last read.
func (t *eventTail) read(ctx context.Context) {
	if t.failed != nil {
		return
	}
	root, err := os.OpenRoot(t.e.d.Spaces.RunPath(t.r.SpaceID, t.r.ID))
	if err != nil {
		return
	}
	defer func() { _ = root.Close() }()
	// Only a regular file: a link the notebook planted is never followed.
	if info, err := root.Lstat(RunEventsFile); err != nil || !info.Mode().IsRegular() {
		return
	}
	f, err := root.Open(RunEventsFile)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Seek(t.offset, io.SeekStart); err != nil {
		return
	}
	chunk := make([]byte, runEventsReadSize)
	for t.failed == nil {
		n, err := f.Read(chunk)
		t.offset += int64(n)
		t.consume(ctx, chunk[:n])
		if n == 0 || err != nil {
			return
		}
	}
}

func (t *eventTail) consume(ctx context.Context, data []byte) {
	tooBig := func() {
		t.fail(KindOutputTooLarge, fmt.Sprintf("the run reported an event over %d KiB; an item may be at most that "+
			"(write large results to an artifact)", MaxRunEventBytes>>10))
	}
	for len(data) > 0 && t.failed == nil {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			if !t.skip {
				t.buf = append(t.buf, data...)
				if len(t.buf) > MaxRunEventBytes {
					t.buf, t.skip = nil, true
				}
			}
			return
		}
		line := data[:i]
		data = data[i+1:]
		if t.skip {
			t.skip = false
			tooBig()
			return
		}
		if len(t.buf) > 0 {
			line = append(t.buf, line...)
			t.buf = nil
		}
		if len(line) > MaxRunEventBytes {
			tooBig()
			return
		}
		t.record(ctx, line)
	}
}

func (t *eventTail) record(ctx context.Context, line []byte) {
	if len(bytes.TrimSpace(line)) == 0 {
		return
	}
	ev, err := parseRunEvent(line)
	if err != nil {
		t.fail(KindOutputInvalid, "the run reported something nbsandbox can't read: "+err.Error())
		return
	}
	now := time.Now()
	elapsed := now.Sub(t.last).Seconds()
	t.last = now
	t.itemTok = min(ItemsPerSec, t.itemTok+elapsed*ItemsPerSec)
	t.progTok = min(ProgressPerSec, t.progTok+elapsed*ProgressPerSec)
	switch ev.Kind {
	case store.EventItem:
		switch {
		case !t.contract.Stream():
			t.fail(KindOutputInvalid, "the notebook returns one value (nbsandbox.output), but it called nbsandbox.emit")
			return
		case t.items >= MaxRunItems:
			t.fail(KindOutputTooLarge, fmt.Sprintf("the run sent more than %d items", MaxRunItems))
			return
		case t.itemTok < 1:
			t.fail(KindOutputInvalid, fmt.Sprintf("the run sent items faster than %d a second", ItemsPerSec))
			return
		case t.bytes+int64(len(ev.Item)) > t.e.cfg.MaxOutput:
			t.fail(KindOutputTooLarge, fmt.Sprintf("the run's items add up to more than %d bytes; "+
				"write large results to an artifact instead", t.e.cfg.MaxOutput))
			return
		}
		if err := t.contract.ValidateItem(ev.Item); err != nil {
			t.fail(KindOutputInvalid, fmt.Sprintf("item %d: %v", t.items+1, err))
			return
		}
		t.itemTok--
		t.items++
		t.bytes += int64(len(ev.Item))
		ev.Index = t.items
	case store.EventProgress:
		if t.progTok < 1 {
			t.note("rate", fmt.Sprintf("the run reported progress more than %d times a second; some updates were dropped", ProgressPerSec))
			return
		}
		t.progTok--
	}
	ev.RunID, ev.Function, ev.Version = t.r.ID, t.r.Function, t.r.Version
	b, _ := json.Marshal(ev)
	write := func() error {
		_, err := t.e.d.DB.AddEvent(ctx, store.Event{SpaceID: t.r.SpaceID, RunID: t.r.ID, Function: t.r.Function,
			Status: StatusRunning, Kind: ev.Kind, Data: string(b), CreatedAt: t.e.d.Now()})
		return err
	}
	if ev.Kind == store.EventItem {
		// An item must not be lost: keep trying through a database blip
		// (the notebook keeps working; later items queue up in the file).
		err = retryDB(ctx, write)
	} else {
		err = write()
	}
	if err != nil {
		t.e.logFor(t.r).Error("record a run event", "error", err)
		if ev.Kind == store.EventItem {
			// An item that can't be recorded would leave a gap in the output.
			t.fail(KindOutputInvalid, "an item couldn't be recorded: "+err.Error())
		}
		return
	}
	t.e.wake.Notify(t.r.SpaceID)
}

// streamedItems returns the items a streamed run has sent so far, in order.
func (c *core) streamedItems(ctx context.Context, r store.Run) ([]json.RawMessage, error) {
	evs, err := c.d.DB.Events(ctx, store.EventFilter{SpaceID: r.SpaceID, RunID: r.ID, Kind: store.EventItem, Limit: MaxRunItems})
	if err != nil {
		return nil, err
	}
	items := make([]json.RawMessage, 0, len(evs))
	for _, ev := range evs {
		var re RunEvent
		if err := json.Unmarshal([]byte(ev.Data), &re); err != nil {
			return nil, err
		}
		items = append(items, re.Item)
	}
	return items, nil
}
