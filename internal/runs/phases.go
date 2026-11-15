// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package runs

import (
	"math"
	"time"
)

// Timeline is what nbsandboxd itself saw of a run, for Phases.
type Timeline struct {
	Created      time.Time // the request was accepted
	Claimed      time.Time // a worker took it (marked running)
	BackendStart time.Time // the run's files were ready; the backend was asked to start it
	BackendDone  time.Time // the backend returned (container gone)
}

// Phases says where a run's time went, in seconds, from nbsandboxd's
// timestamps, the backend's container start and exit, and the runner's own
// marks (its status file's timings):
//
//	queued           created → claimed by a worker
//	prepare          claimed → files ready (a function's bundle checked out, the run's folder made)
//	container_start  → the container (or Pod) started
//	runner_start     → the runner's Python and its imports done
//	kernel_start     → the kernel ready (the first cell starting)
//	execute          → the last cell done
//	render           → output.html written
//	exit             → the container exited
//	teardown         → the backend returned (output copied out, container removed)
//
// The time to record the result afterwards ("record") is measured by the
// caller. A phase whose ends aren't known, or that the clocks of different
// machines make negative, is left out.
func Phases(t Timeline, containerStarted, containerFinished time.Time, ws *WrapperStatus) map[string]float64 {
	marks := map[string]float64{}
	if ws != nil {
		marks = ws.Timings
	}
	epoch := func(v float64, ok bool) time.Time {
		if !ok || v <= 0 {
			return time.Time{}
		}
		sec, frac := math.Modf(v)
		return time.Unix(int64(sec), int64(frac*1e9))
	}
	mark := func(name string) time.Time { v, ok := marks[name]; return epoch(v, ok) }

	points := []struct {
		phase string
		at    time.Time
	}{
		{"", t.Created},
		{"queued", t.Claimed},
		{"prepare", t.BackendStart},
		{"container_start", containerStarted},
		{"runner_start", mark("imports_done")},
		{"kernel_start", mark("kernel_ready")},
		{"execute", mark("executed")},
		{"render", mark("rendered")},
		{"exit", containerFinished},
		{"teardown", t.BackendDone},
	}
	out := map[string]float64{}
	for i := 1; i < len(points); i++ {
		from, to := points[i-1].at, points[i].at
		if from.IsZero() || to.IsZero() {
			continue
		}
		d := to.Sub(from).Seconds()
		if d < -0.05 { // clocks of different machines (Docker Desktop's VM, a node)
			continue
		}
		out[points[i].phase] = math.Round(max(d, 0)*1000) / 1000
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
