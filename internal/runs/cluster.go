// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package runs

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/logs"
	"github.com/azbyluthfan/nbsandbox/internal/store"
)

// What makes several nbsandboxd processes work as one: live log lines shared
// through the database, notifications between nodes, and taking over the
// runs of a worker that went away.

// Live log batches: written at most this often, or sooner when this big.
const (
	logShipEvery = 200 * time.Millisecond
	logShipBytes = 64 << 10
)

// shipLog copies a running run's log lines into the database in batches, so
// API nodes without the run can stream them. It ends when the hub closes or
// the lease is lost; the returned channel closes then.
func (e *executor) shipLog(ctx context.Context, r store.Run, hub *logs.Hub) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		var after int64
		var batch strings.Builder
		first := int64(0)
		flush := func() bool {
			if batch.Len() == 0 {
				return true
			}
			err := e.d.DB.AppendLogChunk(ctx, r.ID, r.Token, store.LogChunk{FirstSeq: first, LastSeq: after, Text: batch.String()})
			batch.Reset()
			if errors.Is(err, store.ErrFenced) {
				return false // the run ended (or isn't ours): its log.txt is the source now
			}
			if err != nil {
				e.logFor(r).Warn("ship live log lines", "error", err) // followers catch up from log.txt
			}
			return true
		}
		tick := time.NewTicker(logShipEvery)
		defer tick.Stop()
		for {
			backlog, sub := hub.Subscribe(after)
			ok := func() bool {
				defer hub.Unsubscribe(sub)
				add := func(ev logs.Event) bool {
					if batch.Len() == 0 {
						first = ev.Seq
					}
					batch.WriteString(ev.Text)
					batch.WriteByte('\n')
					after = ev.Seq
					return batch.Len() < logShipBytes || flush()
				}
				for _, ev := range backlog {
					if !add(ev) {
						return false
					}
				}
				for {
					select {
					case ev, open := <-sub.C:
						if !open { // evicted: resubscribe after the last line taken
							return true
						}
						if ev.Done {
							flush()
							return false
						}
						if !add(ev) {
							return false
						}
					case <-tick.C:
						if !flush() {
							return false
						}
					}
				}
			}()
			if !ok {
				return
			}
		}
	}()
	return done
}

// listen turns database notifications into local wake-ups: status streams,
// log followers, idle workers, and cancels of runs executing here. Until it
// connects (and after it reconnects) everything re-reads.
func (m *Manager) listen(ctx context.Context) {
	m.d.DB.Listen(ctx, m.d.Log, func(channel, payload string) {
		switch channel {
		case "":
			m.wake.NotifyAll()
			m.logWake.NotifyAll()
			m.ex.signal()
		case store.ChanEvents:
			m.wake.Notify(payload)
		case store.ChanLogs:
			m.logWake.Notify(payload)
		case store.ChanQueued:
			m.ex.signal()
		case store.ChanCancel:
			m.ex.Cancel(payload, ErrCancelled)
		}
	}, store.ChanEvents, store.ChanLogs, store.ChanQueued, store.ChanCancel)
}

// maintenance runs on every node: it takes over the runs of workers that
// stopped renewing their leases, keeps the queue length metric current, and
// wakes waiters now and then in case a notification was lost.
func (m *Manager) maintenance(ctx context.Context) {
	t := time.NewTicker(m.cfg.QueuePoll)
	defer t.Stop()
	expire := time.NewTicker(m.cfg.Lease / 3)
	defer expire.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if m.d.Metrics != nil {
				if n, err := m.d.DB.QueueLength(ctx); err == nil {
					m.d.Metrics.QueueLength.Set(float64(n))
				}
			}
			m.wake.NotifyAll()
			m.logWake.NotifyAll()
		case <-expire.C:
			m.expireLeases(ctx)
		}
	}
}

// expireLeases handles runs whose worker stopped renewing the lease: a run it
// claimed but never started goes back to the queue; a running one ends as
// interrupted (retryable). Every node does this; the fenced writes make it
// safe to race.
func (m *Manager) expireLeases(ctx context.Context) {
	rs, err := m.d.DB.ExpiredLeases(ctx)
	if err != nil {
		m.d.Log.Warn("list expired run leases", "error", err)
		return
	}
	for _, r := range rs {
		if m.ex.isActive(r.ID) {
			continue // ours: the heartbeat is just late
		}
		if r.Status == StatusQueued {
			if ok, err := m.d.DB.ReleaseRun(ctx, r.ID, r.Token); err == nil && ok {
				m.logFor(r).Warn("a worker claimed the run and went away; queued again", "worker", r.Worker)
			}
			continue
		}
		m.interruptIfRunning(ctx, r.ID, "the worker running it ("+r.Worker+") stopped responding", store.Fence{Token: r.Token})
	}
}

func (e *executor) isActive(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.active[id] != nil
}
