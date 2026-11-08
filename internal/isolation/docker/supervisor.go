// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package docker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/deps"
	"github.com/azbyluthfan/nbsandbox/internal/isolation"
)

// ErrRuntimeMissing means the configured runtime (e.g. runsc) is not
// registered with Docker. It is a configuration error, never silently
// replaced by the default runtime.
var ErrRuntimeMissing = errors.New("runtime not registered with Docker")

// Status is the supervisor's latest view of Docker.
type Status = isolation.Status

// SupervisorConfig configures a Supervisor.
type SupervisorConfig struct {
	Host    string // for messages only
	Image   string // runner image reference
	Runtime string // "" for Docker's default runtime, or "runsc"

	// Intervals; zero values get production defaults.
	UpInterval  time.Duration // between checks while up (default 5s)
	MinBackoff  time.Duration // first retry while down (default 1s)
	MaxBackoff  time.Duration // longest retry while down (default 30s)
	CallTimeout time.Duration // per Docker call (default 5s)

	// OnChange is called (from the supervisor goroutine) whenever Up changes,
	// e.g. to pause the queue or reconcile orphans when Docker returns.
	OnChange func(up bool)
	// SetUpGauge receives 1 or 0 for the nbsandbox_docker_up metric.
	SetUpGauge func(float64)
	Logger     *slog.Logger
}

// Supervisor watches the Docker connection. Docker being
// unreachable is a normal, recoverable state: the supervisor
// keeps checking with backoff and reports the state to readiness and to the
// run manager.
type Supervisor struct {
	cfg    SupervisorConfig
	client Client

	mu     sync.RWMutex
	status Status
	wake   chan struct{}

	// The completion index, read once per image ID in the background.
	compOf   string // the image ID it was read from ("" before)
	comp     []byte
	compBusy bool
}

// NewSupervisor creates a supervisor. Call Run to start checking.
func NewSupervisor(c Client, cfg SupervisorConfig) *Supervisor {
	if cfg.UpInterval == 0 {
		cfg.UpInterval = 5 * time.Second
	}
	if cfg.MinBackoff == 0 {
		cfg.MinBackoff = time.Second
	}
	if cfg.MaxBackoff == 0 {
		cfg.MaxBackoff = 30 * time.Second
	}
	if cfg.CallTimeout == 0 {
		cfg.CallTimeout = 5 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Supervisor{
		cfg:    cfg,
		client: c,
		status: Status{DockerErr: errors.New("not checked yet")},
		wake:   make(chan struct{}, 1),
	}
}

// Status returns the latest status.
func (s *Supervisor) Status() Status {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.status
}

// CheckStartup runs one check before the service starts serving. It fails
// only for a configuration error that can be detected now: Docker is
// reachable but the configured runtime is missing. An unreachable Docker is
// not an error here; the check is deferred to the background loop.
func (s *Supervisor) CheckStartup(ctx context.Context) error {
	st := s.check(ctx)
	if st.Up && errors.Is(st.RuntimeErr, ErrRuntimeMissing) {
		return st.RuntimeErr
	}
	if !st.Up {
		s.cfg.Logger.Warn("docker unreachable at startup; will keep retrying", "host", s.cfg.Host, "error", st.DockerErr)
	}
	return nil
}

// Recheck asks the loop to check now instead of waiting for the next tick.
func (s *Supervisor) Recheck() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Run checks Docker until ctx is cancelled.
func (s *Supervisor) Run(ctx context.Context) {
	backoff := s.cfg.MinBackoff
	for {
		st := s.check(ctx)
		wait := s.cfg.UpInterval
		if st.Up {
			backoff = s.cfg.MinBackoff
		} else {
			wait = backoff
			backoff = min(backoff*2, s.cfg.MaxBackoff)
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-s.wake:
			t.Stop()
		case <-t.C:
		}
	}
}

// check queries Docker once, stores and returns the new status.
func (s *Supervisor) check(ctx context.Context) Status {
	call := func(fn func(context.Context) error) error {
		cctx, cancel := context.WithTimeout(ctx, s.cfg.CallTimeout)
		defer cancel()
		return fn(cctx)
	}

	var next Status
	if err := call(s.client.Ping); err != nil {
		next.DockerErr = fmt.Errorf("docker unreachable at %s: %w", s.hostLabel(), err)
	} else {
		next.Up = true
		var info Info
		if err := call(func(c context.Context) (err error) { info, err = s.client.Info(c); return err }); err != nil {
			next.RuntimeErr = fmt.Errorf("docker info: %w", err)
		} else {
			next.Info = info
			if s.cfg.Runtime != "" && !slices.Contains(info.Runtimes, s.cfg.Runtime) {
				next.RuntimeErr = fmt.Errorf("%w: %q is configured but Docker only has %v; install and register it, or unset docker.runtime",
					ErrRuntimeMissing, s.cfg.Runtime, info.Runtimes)
			}
		}
		var img Image
		if err := call(func(c context.Context) (err error) { img, err = s.client.ImageInspect(c, s.cfg.Image); return err }); err != nil {
			next.ImageErr = fmt.Errorf("runner image %s not available: %w", s.cfg.Image, err)
		} else {
			next.Image = img
			// The image must say what it provides and speak a status format
			// this nbsandboxd understands.
			if m, err := deps.ParseManifest(img.Labels[deps.ManifestLabel]); err != nil {
				next.ImageErr = fmt.Errorf("runner image %s: %w", s.cfg.Image, err)
			} else {
				next.Manifest = m
			}
		}
	}

	s.mu.Lock()
	if next.Manifest != nil && next.Image.ID != "" {
		if s.compOf == next.Image.ID {
			next.Completions = s.comp
		} else if !s.compBusy {
			s.compBusy = true
			go s.readCompletions(context.WithoutCancel(ctx), next.Image.ID)
		}
	}
	prev := s.status
	changed := prev.Up != next.Up || prev.Since.IsZero()
	if changed {
		next.Since = time.Now()
	} else {
		next.Since = prev.Since
	}
	s.status = next
	s.mu.Unlock()

	if s.cfg.SetUpGauge != nil {
		if next.Up {
			s.cfg.SetUpGauge(1)
		} else {
			s.cfg.SetUpGauge(0)
		}
	}
	if changed {
		firstCheck := prev.Since.IsZero()
		if next.Up {
			s.cfg.Logger.Info("docker is up", "host", s.hostLabel(), "version", next.Info.ServerVersion,
				"os", next.Info.OperatingSystem, "runtimes", next.Info.Runtimes)
		} else if !firstCheck {
			s.cfg.Logger.Warn("docker is down", "host", s.hostLabel(), "error", next.DockerErr)
		}
		// Notify on every change, and on the first check only when Docker is
		// up (so startup reconciliation runs); a first "down" changes nothing.
		if s.cfg.OnChange != nil && (!firstCheck || next.Up) {
			s.cfg.OnChange(next.Up)
		}
	}
	return next
}

// readCompletions reads the image's completion index (once per image ID;
// an image without one is remembered too, so it isn't retried).
func (s *Supervisor) readCompletions(ctx context.Context, imageID string) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	b, err := ReadImageFile(ctx, s.client, s.cfg.Image, s.cfg.Runtime, isolation.CompletionsPath, isolation.MaxCompletions)
	if err != nil {
		s.cfg.Logger.Info("the runner image has no completion index for editors", "image", s.cfg.Image, "error", err)
	} else {
		s.cfg.Logger.Info("read the runner image's completion index", "image", s.cfg.Image, "bytes", len(b))
	}
	s.mu.Lock()
	s.compBusy = false
	if err == nil || ctx.Err() == nil { // a timeout is retried at the next check
		s.compOf, s.comp = imageID, b
		if s.status.Image.ID == imageID {
			s.status.Completions = b
		}
	}
	s.mu.Unlock()
}

func (s *Supervisor) hostLabel() string {
	if s.cfg.Host == "" {
		return "default socket"
	}
	return s.cfg.Host
}

// Readiness checks, one per concern, so /readyz names exactly what is wrong.

// DockerCheck reports whether Docker is reachable.
func (s *Supervisor) DockerCheck(context.Context) error { return s.Status().DockerErr }

// ImageCheck reports whether the runner image is present.
func (s *Supervisor) ImageCheck(context.Context) error {
	st := s.Status()
	if !st.Up {
		return errors.New("unknown: docker unreachable")
	}
	return st.ImageErr
}

// RuntimeCheck reports whether the configured runtime is available.
func (s *Supervisor) RuntimeCheck(context.Context) error {
	st := s.Status()
	if !st.Up {
		return errors.New("unknown: docker unreachable")
	}
	return st.RuntimeErr
}
