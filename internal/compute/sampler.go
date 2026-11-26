// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package compute

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/isolation"
	"github.com/azbyluthfan/nbsandbox/internal/store"
)

// Totals sum what a worker's runs and sessions use now: published with the
// worker, shown on the compute view.
type Totals struct {
	MemoryBytes         int64   `json:"memory_bytes"`          // their working sets
	MemoryReservedBytes int64   `json:"memory_reserved_bytes"` // their limits
	CPUs                float64 `json:"cpus"`                  // cores used over the last interval
	CPUsReserved        float64 `json:"cpus_reserved"`         // their limits
	// WriteBytesPerSecond is what they wrote to disk over the last
	// interval; nil when the backend can't say (gVisor).
	WriteBytesPerSecond *float64 `json:"write_bytes_per_second,omitempty"`
	// SampledAt is when; zero until the first readings.
	SampledAt time.Time `json:"sampled_at"`
}

// SampleStore keeps run samples (the database).
type SampleStore interface {
	AddRunSample(ctx context.Context, runID string, s store.RunSample) error
}

// Sampler reads the usage of the runs and sessions this worker holds, stores
// each run's samples, and sums them for the worker.
type Sampler struct {
	Reader   isolation.UsageReader
	Runs     func() []string // the runs this worker holds now
	Sessions func() []string // the sessions it holds now
	Store    SampleStore
	Limits   isolation.Limits // each run's and session's
	Log      *slog.Logger
	// Every is how often (5 s); after Thin (an hour) of a run's life its
	// samples are kept once a minute.
	Every, Thin time.Duration
	Now         func() time.Time

	mu     sync.Mutex
	prev   map[string]reading
	totals Totals
}

type reading struct {
	at      time.Time
	u       isolation.Usage
	first   time.Time // the run's first stored sample
	written time.Time // when its last sample was stored
}

// Run samples until ctx ends.
func (s *Sampler) Run(ctx context.Context) {
	t := time.NewTicker(s.Every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Sample(ctx)
		}
	}
}

// Totals returns the latest sums.
func (s *Sampler) Totals() Totals {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.totals
}

// Sample takes one round of readings.
func (s *Sampler) Sample(ctx context.Context) {
	s.mu.Lock()
	if s.prev == nil {
		s.prev = map[string]reading{}
	}
	s.mu.Unlock()
	now := s.Now()
	var tot Totals
	var writes float64
	writesKnown := true
	seen := map[string]bool{}
	read := func(key string, get func() (isolation.Usage, error)) (isolation.Usage, reading, float64, bool) {
		seen[key] = true
		tot.MemoryReservedBytes += s.Limits.MemoryBytes
		tot.CPUsReserved += float64(s.Limits.NanoCPUs) / 1e9
		u, err := get()
		if err != nil { // not started yet, or gone: nothing to show this round
			return u, reading{}, 0, false
		}
		s.mu.Lock()
		p, had := s.prev[key]
		s.prev[key] = reading{at: now, u: u, first: p.first, written: p.written}
		s.mu.Unlock()
		tot.MemoryBytes += u.MemoryBytes
		if !had || !now.After(p.at) || u.CPUNanos < p.u.CPUNanos {
			return u, p, 0, false // a rate needs two readings of the same container
		}
		secs := now.Sub(p.at).Seconds()
		cpus := float64(u.CPUNanos-p.u.CPUNanos) / 1e9 / secs
		tot.CPUs += cpus
		if u.WrittenBytes >= 0 && p.u.WrittenBytes >= 0 && u.WrittenBytes >= p.u.WrittenBytes {
			writes += float64(u.WrittenBytes-p.u.WrittenBytes) / secs
		} else {
			writesKnown = false
		}
		return u, p, cpus, true
	}
	for _, id := range s.Runs() {
		u, p, cpus, ok := read("run/"+id, func() (isolation.Usage, error) { return s.Reader.RunUsage(ctx, id) })
		if !ok {
			continue
		}
		first := p.first
		if first.IsZero() {
			first = now
		}
		if now.Sub(first) > s.Thin && now.Sub(p.written) < time.Minute {
			continue // an old run: once a minute is enough
		}
		smp := store.RunSample{At: now, MemoryBytes: u.MemoryBytes, CPUMillicores: int(cpus * 1000)}
		if u.WrittenBytes >= 0 {
			w := u.WrittenBytes
			smp.WrittenBytes = &w
		}
		if err := s.Store.AddRunSample(ctx, id, smp); err != nil {
			s.Log.Debug("store a run sample", "run_id", id, "error", err)
			continue
		}
		s.mu.Lock()
		r := s.prev["run/"+id]
		r.first, r.written = first, now
		s.prev["run/"+id] = r
		s.mu.Unlock()
	}
	for _, id := range s.Sessions() {
		read("session/"+id, func() (isolation.Usage, error) { return s.Reader.SessionUsage(ctx, id) })
	}
	if writesKnown {
		tot.WriteBytesPerSecond = &writes
	}
	tot.SampledAt = now
	s.mu.Lock()
	for k := range s.prev {
		if !seen[k] {
			delete(s.prev, k)
		}
	}
	s.totals = tot
	s.mu.Unlock()
}
