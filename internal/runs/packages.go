// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package runs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/deps"
	"github.com/azbyluthfan/nbsandbox/internal/isolation"
	"github.com/azbyluthfan/nbsandbox/internal/packages"
	"github.com/azbyluthfan/nbsandbox/internal/spaces"
	"github.com/azbyluthfan/nbsandbox/internal/store"
)

// KindPackages: the space's packages could not be built (the caller's
// requirements: a package without a wheel, a conflict, a typo).
const KindPackages = "packages"

// PackagesFailedError refuses a run (or a publish) whose package set failed
// to build recently.
type PackagesFailedError struct {
	Reason string
	Log    string
}

func (e *PackagesFailedError) Error() string {
	return "the space's packages could not be built: " + e.Reason + " (GET .../packages shows the build log)"
}

// packageSet asks for the set a submission needs: a published version's
// lock, or the space's requirements.txt. "" when it needs none (or package
// sets are off).
func (m *Manager) packageSet(ctx context.Context, spaceID string, sub submission) (store.PackageSet, error) {
	p := m.d.Packages
	if !p.Enabled() {
		return store.PackageSet{}, nil
	}
	var key string
	var err error
	if sub.function != "" {
		key, err = p.ForLock(ctx, sub.packageLock, m.d.ImageID())
	} else if req, ok := m.d.Spaces.ReadSpaceFileContext(ctx, spaceID, "requirements.txt", packages.MaxRequirementsBytes+1); ok {
		key, err = p.ForSpace(ctx, spaceID, req, m.d.ImageID())
	}
	var invalid *packages.InvalidError
	var failed *packages.FailedError
	switch {
	case errors.As(err, &invalid):
		return store.PackageSet{}, &spaces.InvalidError{Msg: invalid.Msg}
	case errors.As(err, &failed):
		return store.PackageSet{}, &PackagesFailedError{Reason: failed.Reason, Log: failed.Log}
	case err != nil:
		return store.PackageSet{}, err
	case key == "":
		return store.PackageSet{}, nil
	}
	return p.Status(ctx, key)
}

// withoutProvided drops the dependency warnings a package set answers:
// requirements.txt's (the set installs them) and imports of the modules the
// set provides. When the set isn't built yet, imports named like a
// requirement are dropped too, and the rest only warn: what the set will
// provide isn't known yet.
func withoutProvided(ws []deps.Warning, ps store.PackageSet) (kept []deps.Warning, unsure bool) {
	if ps.Key == "" {
		return ws, false
	}
	provided := map[string]bool{}
	for _, mod := range ps.Modules {
		provided[mod] = true
	}
	if ps.Status != store.PackagesReady {
		unsure = true
		if rs, err := packages.Parse([]byte(ps.Requirements)); err == nil {
			for _, n := range rs.Names() {
				provided[strings.ReplaceAll(n, "-", "_")] = true
			}
		}
	}
	for _, w := range ws {
		if w.Source == "requirements.txt" || provided[w.Module] || provided[packages.Normalize(w.Module)] {
			continue
		}
		kept = append(kept, w)
	}
	return kept, unsure
}

// packagesDir is the run's set on this worker's disk ("" if it needs none).
// For a plain run, the space's lock is recorded: the next build of the same
// requirements installs the same versions.
func (e *executor) packagesDir(ctx context.Context, r store.Run) (string, error) {
	if r.PackageKey == "" {
		return "", nil
	}
	if e.d.Packages == nil {
		return "", errors.New("this worker has no package sets")
	}
	dir, err := e.d.Packages.Ensure(ctx, r.PackageKey)
	if err != nil {
		return "", err
	}
	if r.Function == "" {
		if ps, err := e.d.Packages.Status(ctx, r.PackageKey); err == nil {
			e.d.Packages.RecordSpaceLock(ctx, r.SpaceID, ps)
		}
	}
	return dir, nil
}

// packagesFinished is told of every build this worker finishes: a ready set
// wakes the workers (runs waiting for it can start anywhere); a failed one
// fails the runs waiting for it.
func (m *Manager) packagesFinished(key string, ps store.PackageSet) {
	ctx := context.Background()
	if ps.Status == store.PackagesReady {
		if err := m.d.DB.Notify(ctx, store.ChanQueued, ""); err != nil {
			m.d.Log.Warn("notify the workers of a ready package set", "error", err)
		}
		m.ex.signal()
		return
	}
	waiting, err := m.d.DB.QueuedRunsWaitingFor(ctx, key)
	if err != nil {
		m.d.Log.Warn("find runs waiting for a failed package set", "key", key, "error", err)
		return
	}
	for _, r := range waiting {
		m.failForPackages(ctx, r, ps)
	}
}

// failIfPackagesFailed fails the queued run runID if its set has failed
// (the build ended before the run was recorded, so packagesFinished
// didn't see it).
func (m *Manager) failIfPackagesFailed(ctx context.Context, runID, key string) {
	ps, err := m.d.Packages.Status(ctx, key)
	if err != nil || ps.Status != store.PackagesFailed {
		return
	}
	if r, err := m.d.DB.RunByID(ctx, runID); err == nil && r.Status == StatusQueued {
		m.failForPackages(ctx, r, ps)
	}
}

// failForPackages fails a queued run whose package set failed (first
// writer wins: a run already finished stays as it is).
func (m *Manager) failForPackages(ctx context.Context, r store.Run, ps store.PackageSet) {
	out := Outcome{Status: StatusFailed, ErrorKind: KindPackages, Fault: FaultCaller,
		Message: "the space's packages could not be built: " + ps.Error}
	if m.record(ctx, r, out, m.buildResult(ctx, r, out, nil, isolation.RunResult{}, nil, m.d.Now(), false), store.Fence{QueuedOnly: true}) &&
		m.d.Metrics != nil {
		m.d.Metrics.RunsTotal.WithLabelValues(StatusFailed).Inc()
	}
}

// publishLock waits for the set a publish needs and returns its lock ("" if
// the space has no requirements.txt or package sets are off).
func (m *Manager) publishLock(ctx context.Context, spaceID string) (string, error) {
	ps, err := m.packageSet(ctx, spaceID, submission{})
	if err != nil || ps.Key == "" {
		return "", err
	}
	deadline := time.After(m.cfg.PackagesWait)
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		switch ps.Status {
		case store.PackagesReady:
			return ps.Lock, nil
		case store.PackagesFailed:
			return "", &PackagesFailedError{Reason: ps.Error, Log: ps.Log}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-deadline:
			return "", fmt.Errorf("%w: the space's packages are still being built; publish again shortly", ErrPackagesBuilding)
		case <-tick.C:
		}
		if ps, err = m.d.Packages.Status(ctx, ps.Key); err != nil {
			return "", err
		}
	}
}

// ErrPackagesBuilding: a publish gave up waiting for the space's packages.
var ErrPackagesBuilding = errors.New("packages building")

// PackagesView is a space's packages, as GET .../packages shows them.
type PackagesView struct {
	// Enabled: package sets are on (otherwise requirements.txt is only
	// checked against the runner image).
	Enabled bool `json:"enabled"`
	// Status: none (no requirements.txt), not_built, pending, building,
	// ready or failed.
	Status    string             `json:"status"`
	Installed []InstalledPackage `json:"installed,omitempty"` // what the set adds to the runner image
	Lock      string             `json:"lock,omitempty"`      // the same, with hashes (a requirements file)
	SizeBytes int64              `json:"size_bytes,omitempty"`
	BuiltAt   *time.Time         `json:"built_at,omitempty"`
	Error     string             `json:"error,omitempty"`
	BuildLog  string             `json:"build_log,omitempty"`
}

// InstalledPackage is one distribution of a set.
type InstalledPackage struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

func packagesView(ps store.PackageSet) PackagesView {
	v := PackagesView{Enabled: true, Status: ps.Status, Lock: ps.Lock, SizeBytes: ps.SizeBytes, Error: ps.Error, BuildLog: ps.Log}
	if !ps.BuiltAt.IsZero() {
		t := ps.BuiltAt.UTC()
		v.BuiltAt = &t
	}
	for _, line := range strings.Split(ps.Lock, "\n") {
		spec, _, _ := strings.Cut(strings.TrimSpace(line), " ")
		if name, version, ok := strings.Cut(spec, "=="); ok {
			v.Installed = append(v.Installed, InstalledPackage{Name: name, Version: version})
		}
	}
	return v
}

// SpacePackages shows the space's packages: the set its requirements.txt
// needs now, and how far it is. It never starts a build.
func (m *Manager) SpacePackages(ctx context.Context, owner, spaceID string) (PackagesView, error) {
	if err := m.d.Spaces.Allowed(ctx, owner, spaceID, spaces.RoleRead); err != nil {
		return PackagesView{}, err
	}
	if !m.d.Packages.Enabled() {
		return PackagesView{Enabled: false, Status: "disabled"}, nil
	}
	req, ok := m.d.Spaces.ReadSpaceFileContext(ctx, spaceID, "requirements.txt", packages.MaxRequirementsBytes+1)
	if !ok {
		return PackagesView{Enabled: true, Status: "none"}, nil
	}
	key, err := m.d.Packages.KeyForSpace(req, m.d.ImageID())
	var invalid *packages.InvalidError
	switch {
	case errors.As(err, &invalid):
		return PackagesView{Enabled: true, Status: store.PackagesFailed, Error: invalid.Msg}, nil
	case err != nil:
		return PackagesView{}, err
	case key == "":
		return PackagesView{Enabled: true, Status: "none"}, nil
	}
	ps, err := m.d.Packages.Status(ctx, key)
	if errors.Is(err, store.ErrNotFound) {
		return PackagesView{Enabled: true, Status: "not_built"}, nil
	}
	if err != nil {
		return PackagesView{}, err
	}
	return packagesView(ps), nil
}

// BuildSpacePackages asks for the space's set now (runs and publishes then
// find it built). A set that failed recently is shown failed, not rebuilt.
func (m *Manager) BuildSpacePackages(ctx context.Context, owner, spaceID string) (PackagesView, error) {
	if err := m.d.Spaces.Allowed(ctx, owner, spaceID, spaces.RoleWrite); err != nil {
		return PackagesView{}, err
	}
	if !m.d.Packages.Enabled() {
		return PackagesView{}, &spaces.InvalidError{Msg: "custom packages are off on this nbsandbox (packages.enabled)"}
	}
	if _, ok := m.d.Spaces.ReadSpaceFileContext(ctx, spaceID, "requirements.txt", packages.MaxRequirementsBytes+1); !ok {
		return PackagesView{}, &spaces.InvalidError{Msg: "the space has no requirements.txt"}
	}
	ps, err := m.packageSet(ctx, spaceID, submission{})
	var failed *PackagesFailedError
	if errors.As(err, &failed) {
		return m.SpacePackages(ctx, owner, spaceID)
	}
	if err != nil {
		return PackagesView{}, err
	}
	return packagesView(ps), nil
}
