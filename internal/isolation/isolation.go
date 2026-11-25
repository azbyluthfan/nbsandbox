// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package isolation

import (
	"context"
	"errors"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/deps"
)

// Backend executes one run in isolation.
//
// Run blocks until the run ends. Cancelling ctx stops the run: the backend
// stops the container (SIGTERM, then SIGKILL after Limits.StopGrace) using a
// fresh context, and reports Stopped. Output lines go to out as they are
// produced. Status classification is not the backend's job; it happens in
// internal/runs so every backend is judged by the same rules.
type Backend interface {
	Name() string
	Run(ctx context.Context, spec RunSpec, out LineSink) (RunResult, error)
	// Orphans lists run containers that exist (running or not).
	Orphans(ctx context.Context) ([]Orphan, error)
	// Kill stops and removes a run's container, if any.
	Kill(ctx context.Context, runID string) error
}

// RunSpec is what to run and with which limits. Limits come from service
// configuration, never from requests.
type RunSpec struct {
	RunID, SpaceID string
	SpaceDir       string // host folder mounted at /space: the space's files, or a published bundle
	SpaceReadOnly  bool   // mount /space read-only (function invocations, or on request)
	RunDir         string // host folder mounted at /run, writable: this run's outputs only
	Notebook       string // shown in logs; the run executes /run/input.ipynb, its snapshot
	Image          string
	Runtime        string // "" (runc) or "runsc"
	Limits         Limits
	MaxOutputBytes int64 // the most nbsandbox.output may write (nbsandboxd checks again)
	// DiskBytes, if set, bounds what the run may hold in /space and /run
	// together, for backends whose files the run manager can't watch on
	// the host (Kubernetes): over it, the run is stopped with DiskExceeded.
	DiskBytes int64
	// OutputMode is how the notebook returns its result: single or stream
	// (NBSANDBOX_OUTPUT_MODE, read by the nbsandbox module).
	OutputMode string
	// PackagesDir, if set, is the space's package set (installed packages),
	// mounted read-only at PackagesPath; the kernel adds it to sys.path
	// after the image's own packages.
	PackagesDir string
	// Fetcher, if set, is the run's reader of external data: a second
	// container the run reaches at FetcherAddress, on a network only the
	// two and the gateway share. Nil: the run reads nothing outside.
	Fetcher *Fetcher
}

// Fetcher is a run's fetcher (docs/plan-data-access.md): it holds the
// run's credentials, which the runner never sees, and reaches the run's
// sources only through the egress gateway.
type Fetcher struct {
	Image string
	// Config is the fetcher's configuration, credentials included: given
	// to the fetcher alone (NBSANDBOX_FETCHER_CONFIG_JSON), never logged.
	Config []byte
	// Token is what the runner presents to the fetcher
	// (NBSANDBOX_FETCHER_TOKEN).
	Token string
	// Gateway is the gateway's container (Docker): joined to the run's
	// network so the fetcher can reach it.
	Gateway string
}

// Where a run reaches its fetcher, and the environment variables that say so.
const (
	FetcherHost     = "fetcher"
	FetcherPort     = 8815
	FetcherAddress  = "grpc://fetcher:8815"
	FetcherEnv      = "NBSANDBOX_FETCHER"
	FetcherTokenEnv = "NBSANDBOX_FETCHER_TOKEN"
)

// PackagesPath is where a run sees its space's package set.
const PackagesPath = "/opt/nbsandbox/packages"

// BuildSpec is one package set build: the builder image, with network, the
// request in InDir (read-only, at /in) and the result written to OutDir
// (at /out). No space files.
type BuildSpec struct {
	ID      string // names the container; unique per build
	Worker  string // the worker building it (labels the container)
	InDir   string
	OutDir  string
	Image   string // the builder image
	Runtime string
	Network string // the Docker network (build containers are the only ones with network)
	Limits  Limits
}

// PackageBuilder is a backend that can build package sets (Docker).
type PackageBuilder interface {
	BuildPackages(ctx context.Context, spec BuildSpec, out LineSink) (RunResult, error)
	// RemoveBuildOrphans removes the build containers worker left behind
	// (an earlier life of the same worker).
	RemoveBuildOrphans(ctx context.Context, worker string) error
}

// Limits are the per-run resource limits.
type Limits struct {
	MemoryBytes int64
	NanoCPUs    int64
	PIDs        int64
	TmpfsBytes  int64
	ShmBytes    int64
	LogBytes    int64 // Docker's own log storage cap
	StopGrace   time.Duration
}

// LineSink receives output lines (without the trailing newline).
type LineSink interface {
	Line(text string)
}

// LineFunc adapts a function to a LineSink.
type LineFunc func(string)

// Line implements LineSink.
func (f LineFunc) Line(s string) { f(s) }

// RunResult is the container-level evidence about how a run ended.
type RunResult struct {
	ContainerID string
	ExitCode    int
	OOMKilled   bool // Docker's flag; any OOM kill in the container's cgroup
	Started     time.Time
	Finished    time.Time
	Stopped     bool // nbsandboxd stopped it because ctx was cancelled
	Removed     bool // the container was removed afterwards
	// DiskExceeded: the run went over its disk allowance and the platform
	// stopped it (Kubernetes evicts a Pod over its ephemeral storage).
	DiskExceeded bool
}

// Orphan is a run container found by its label.
type Orphan struct {
	RunID       string
	SpaceID     string
	ContainerID string
	Running     bool
}

// Errors returned by Run. Both mean the notebook was not at fault.
var (
	// ErrStart: the run never started (image missing, create or start failed,
	// Docker unreachable). Maps to platform_error.
	ErrStart = errors.New("run could not be started")
	// ErrDockerLost: Docker became unreachable while the run was active. The
	// container may still exist; reconciliation cleans it up.
	ErrDockerLost = errors.New("docker became unreachable during the run")
)

// Status is a backend supervisor's latest view of what runs notebooks:
// Docker, or the Kubernetes API.
type Status struct {
	Up         bool
	Since      time.Time // when Up last changed
	DockerErr  error     // why the backend is unreachable or unusable (nil when Up)
	ImageErr   error     // runner image problem (nil when present)
	RuntimeErr error     // configured runtime problem (nil when available)
	Info       Info
	Image      Image
	Manifest   *deps.Manifest // the runner image's manifest; nil if unknown or unsupported
	// Completions is the runner image's completion index for editors
	// (gzipped JSON, CompletionsPath); nil until read, or if the image has none.
	Completions []byte
}

// RunEventsFile is the file in the run's folder the runner and the notebook
// append events to (items, progress, cell starts and ends), one JSON line
// each, read by nbsandboxd while the run works.
const RunEventsFile = ".nbsandbox-events.jsonl"

// CompletionsPath is where a runner image keeps its completion index, and
// MaxCompletions how large it may be.
const (
	CompletionsPath = "/opt/nbsandbox/completions.json.gz"
	MaxCompletions  = 4 << 20 // base64 in a Pod's log stays under the kubelet's 10 MiB rotation
)

// Info describes the backend's platform.
type Info struct {
	ServerVersion   string
	OperatingSystem string
	KernelVersion   string
	CgroupVersion   string
	Runtimes        []string
}

// Image is what nbsandboxd knows about the runner image.
type Image struct {
	ID          string
	RepoDigests []string
	Labels      map[string]string
}
