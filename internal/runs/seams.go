// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package runs

import "sync"

// The run manager has two sides that talk only through the database and the
// interfaces in this file:
//
//   - the submit side (Manager): validates and records runs, asks for
//     cancels, and serves views, results and status streams;
//   - the execute side (executor): claims queued runs with a lease, runs
//     them through the Backend, follows logs, enforces limits and records
//     the outcome.
//
// They may live in different processes (roles "api" and "worker"). The queue
// is the runs table: a worker claims the oldest unclaimed queued run with a
// lease it renews while the run is active, and every write it makes for the
// run is fenced by the lease's token. PostgreSQL notifications wake the
// other side early (a run was queued, a cancel requested, a status event
// recorded, live log lines written); timers catch whatever a lost
// notification would have announced. The one-writer-per-space rule is not
// here: the store enforces it in the same transaction that records the run.

// Wakeups wakes the status streams of a space when one of its events has been
// recorded. Events themselves are read from the database, so a stream that
// misses a wake-up still catches up on its next read.
type Wakeups interface {
	// Wait returns a channel closed at the space's next event.
	Wait(spaceID string) <-chan struct{}
	// Notify wakes everything waiting on the space.
	Notify(spaceID string)
}

// Canceller stops a run that is executing. The submit side has already
// recorded the cancel request in the database.
type Canceller interface {
	// Cancel stops the run with the given cause; false if it isn't executing
	// here (still queued, or already finished).
	Cancel(runID string, cause error) bool
}

// memWakeups is the in-process Wakeups.
type memWakeups struct {
	mu    sync.Mutex
	chans map[string]chan struct{}
}

func newMemWakeups() *memWakeups { return &memWakeups{chans: map[string]chan struct{}{}} }

func (n *memWakeups) Wait(spaceID string) <-chan struct{} {
	n.mu.Lock()
	defer n.mu.Unlock()
	ch, ok := n.chans[spaceID]
	if !ok {
		ch = make(chan struct{})
		n.chans[spaceID] = ch
	}
	return ch
}

// NotifyAll wakes every waiter (after notifications may have been missed).
func (n *memWakeups) NotifyAll() {
	n.mu.Lock()
	defer n.mu.Unlock()
	for id, ch := range n.chans {
		close(ch)
		delete(n.chans, id)
	}
}

func (n *memWakeups) Notify(spaceID string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if ch, ok := n.chans[spaceID]; ok {
		close(ch)
		delete(n.chans, spaceID)
	}
}
