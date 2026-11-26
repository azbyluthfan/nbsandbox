// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package compute

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/isolation"
	"github.com/azbyluthfan/nbsandbox/internal/store"
)

type fakeReader struct{ runs, sessions map[string]isolation.Usage }

func (f *fakeReader) RunUsage(_ context.Context, id string) (isolation.Usage, error) {
	if u, ok := f.runs[id]; ok {
		return u, nil
	}
	return isolation.Usage{}, errors.New("no such container")
}
func (f *fakeReader) SessionUsage(_ context.Context, id string) (isolation.Usage, error) {
	if u, ok := f.sessions[id]; ok {
		return u, nil
	}
	return isolation.Usage{}, errors.New("no such container")
}

type fakeSamples map[string][]store.RunSample

func (f fakeSamples) AddRunSample(_ context.Context, id string, s store.RunSample) error {
	f[id] = append(f[id], s)
	return nil
}

func TestSampler(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	r := &fakeReader{runs: map[string]isolation.Usage{"r1": {MemoryBytes: 100, CPUNanos: 0, WrittenBytes: 0}},
		sessions: map[string]isolation.Usage{"x1": {MemoryBytes: 50, WrittenBytes: -1}}}
	got := fakeSamples{}
	runs := []string{"r1", "r2"} // r2 hasn't a container yet
	s := &Sampler{Reader: r, Runs: func() []string { return runs }, Sessions: func() []string { return []string{"x1"} }, Store: got,
		Limits: isolation.Limits{MemoryBytes: 1000, NanoCPUs: 1e9}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Every: 5 * time.Second, Thin: time.Minute, Now: func() time.Time { return now }}
	ctx := context.Background()

	s.Sample(ctx) // the first readings: no rates yet, nothing stored
	if len(got["r1"]) != 0 || s.Totals().MemoryBytes != 150 || s.Totals().MemoryReservedBytes != 3000 || s.Totals().CPUsReserved != 3 {
		t.Fatalf("first round: %+v %+v", got, s.Totals())
	}
	now = now.Add(5 * time.Second)
	r.runs["r1"] = isolation.Usage{MemoryBytes: 300, CPUNanos: 2.5e9, WrittenBytes: 10 << 20} // half a core; 2 MiB/s
	r.sessions["x1"] = isolation.Usage{MemoryBytes: 60, CPUNanos: 0, WrittenBytes: -1}
	s.Sample(ctx)
	if n := got["r1"]; len(n) != 1 || n[0].MemoryBytes != 300 || n[0].CPUMillicores != 500 || n[0].WrittenBytes == nil || *n[0].WrittenBytes != 10<<20 {
		t.Fatalf("second round: %+v", got["r1"])
	}
	if tot := s.Totals(); tot.MemoryBytes != 360 || tot.CPUs != 0.5 || tot.WriteBytesPerSecond != nil || !tot.SampledAt.Equal(now) {
		t.Fatalf("totals %+v (the session's writes are unknown, so the sum is)", tot)
	}

	// Past Thin, a run is kept once a minute.
	for i := 0; i < 20; i++ {
		now = now.Add(5 * time.Second)
		u := r.runs["r1"]
		u.CPUNanos += 1e9
		r.runs["r1"] = u
		s.Sample(ctx)
	}
	if n := len(got["r1"]); n < 12 || n > 14 { // 12 in its first minute, then one more
		t.Fatalf("thinning: %d samples", n)
	}

	// A run that ended is forgotten.
	runs = nil
	s.Sample(ctx)
	if _, ok := s.prev["run/r1"]; ok || s.Totals().MemoryReservedBytes != 1000 {
		t.Fatalf("after the run: %+v %+v", s.prev, s.Totals())
	}
}
