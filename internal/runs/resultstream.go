// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package runs

import (
	"context"
	"encoding/json"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/contract"
	"github.com/azbyluthfan/nbsandbox/internal/store"
)

// ResultEvent is one event of a run's result stream: an item of a streamed
// output, progress (when asked for), and last the result.
type ResultEvent struct {
	Kind   string          // item | progress | result
	Index  int             // item: its number, 1, 2, ...
	Data   json.RawMessage // item, progress: the RunEvent
	Result *CallerResult   // result
}

// resultPoll re-reads a run that hasn't finished when no wake-up comes.
const resultPoll = 2 * time.Second

// FollowResult streams a run's result as it is made: each item of a streamed
// output as soon as it is recorded (after item afterIndex, for a client
// resuming), progress if asked, then the result, and the channel closes.
// For a finished run it replays the items from the stored result and ends.
// The items sent are exactly the result's output list, by index.
func (m *Manager) FollowResult(ctx context.Context, owner, spaceID, runID string, afterIndex int, withProgress bool) (<-chan ResultEvent, error) {
	if err := m.RunExists(ctx, owner, spaceID, runID); err != nil {
		return nil, err
	}
	out := make(chan ResultEvent)
	go func() {
		defer close(out)
		send := func(ev ResultEvent) bool {
			select {
			case out <- ev:
				return true
			case <-ctx.Done():
				return false
			}
		}
		var lastSeq int64
		sent := max(afterIndex, 0)
		for {
			wake := m.Wake(spaceID) // before reading, so nothing slips between
			r, err := m.d.DB.GetRun(ctx, spaceID, runID)
			if err != nil {
				return
			}
			if r.Terminal() {
				res, err := m.GetResult(ctx, owner, spaceID, runID)
				if err != nil {
					return
				}
				// The items not sent yet, from the stored output (the same
				// list a JSON client gets).
				if r.OutputMode == contract.ModeStream && len(res.Output) > 0 {
					var items []json.RawMessage
					if json.Unmarshal(res.Output, &items) == nil {
						for i := sent; i < len(items); i++ {
							b, _ := json.Marshal(RunEvent{RunID: r.ID, Kind: store.EventItem, Function: r.Function,
								Version: r.Version, Index: i + 1, Item: items[i]})
							if !send(ResultEvent{Kind: store.EventItem, Index: i + 1, Data: b}) {
								return
							}
						}
					}
				}
				send(ResultEvent{Kind: "result", Result: &res})
				return
			}
			evs, err := m.d.DB.Events(ctx, store.EventFilter{SpaceID: spaceID, RunID: runID, After: lastSeq})
			if err != nil {
				return
			}
			for _, ev := range evs {
				lastSeq = ev.Seq
				switch {
				case ev.Kind == store.EventItem:
					var re RunEvent
					if json.Unmarshal([]byte(ev.Data), &re) != nil || re.Index <= sent {
						continue
					}
					if !send(ResultEvent{Kind: store.EventItem, Index: re.Index, Data: json.RawMessage(ev.Data)}) {
						return
					}
					sent = re.Index
				case ev.Kind == store.EventProgress && withProgress:
					if !send(ResultEvent{Kind: store.EventProgress, Data: json.RawMessage(ev.Data)}) {
						return
					}
				}
			}
			if len(evs) > 0 {
				continue // there may be more
			}
			select {
			case <-ctx.Done():
				return
			case <-wake:
			case <-time.After(resultPoll):
			}
		}
	}()
	return out, nil
}
