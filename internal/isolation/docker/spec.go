// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package docker

import (
	"strconv"

	"github.com/moby/moby/api/types/container"

	"github.com/azbyluthfan/nbsandbox/internal/isolation"
)

// Labels on every run container. nbsandbox.run finds orphans.
const (
	LabelRun   = "nbsandbox.run"
	LabelSpace = "nbsandbox.space"
)

// ContainerName is the run container's name; one per run.
func ContainerName(runID string) string { return "nbsandbox-run-" + runID }

// buildContainerConfig turns a RunSpec into Docker settings. It is a pure
// function so that every isolation control is pinned by its own unit test.
func buildContainerConfig(spec isolation.RunSpec) (*container.Config, *container.HostConfig) {
	init := true
	pids := spec.Limits.PIDs
	spaceMode := "rw"
	if spec.SpaceReadOnly {
		spaceMode = "ro"
	}

	cfg := &container.Config{
		Image: spec.Image,
		Cmd:   []string{spec.Notebook},
		User:  "1000:1000",
		// Environment allowlist: nothing from nbsandboxd's own environment.
		Env: []string{
			"HOME=/tmp",
			"PYTHONUNBUFFERED=1",
			"NBSANDBOX_RUN_ID=" + spec.RunID,
			"NBSANDBOX_RUN_DIR=/run",
			"NBSANDBOX_OUTPUT_DIR=/run/artifacts",
			"NBSANDBOX_MAX_OUTPUT=" + strconv.FormatInt(spec.MaxOutputBytes, 10),
			"NBSANDBOX_OUTPUT_MODE=" + outputMode(spec.OutputMode),
			"NBSANDBOX_LOG_CAP=" + strconv.FormatInt(spec.Limits.LogBytes, 10),
		},
		WorkingDir:   "/space",
		Labels:       map[string]string{LabelRun: spec.RunID, LabelSpace: spec.SpaceID},
		AttachStdout: false,
		AttachStderr: false,
	}
	host := &container.HostConfig{
		// Files: only this space's files (or a published bundle) at /space,
		// and only this run's output folder at /run. Other spaces, other
		// runs and published versions don't exist inside.
		Binds: runBinds(spec, spaceMode),
		// Network: none at all, only loopback. (Not also NetworkDisabled:
		// under gVisor that removes loopback too, and Jupyter's ZeroMQ then
		// aborts the kernel at start; under runc it adds nothing.)
		NetworkMode: "none",
		// Processes and shared memory: private namespaces (Docker defaults,
		// set explicitly so a daemon default can't change them).
		PidMode: "",
		IpcMode: "private",
		// Init: signals reach the wrapper; no stray processes.
		Init: &init,
		// Privileges.
		Privileged:     false,
		CapDrop:        []string{"ALL"},
		SecurityOpt:    []string{"no-new-privileges"},
		ReadonlyRootfs: true,
		Tmpfs:          map[string]string{"/tmp": "rw,nosuid,nodev,size=" + strconv.FormatInt(spec.Limits.TmpfsBytes, 10)},
		ShmSize:        spec.Limits.ShmBytes,
		// Docker's own log storage, capped (compress=false: Docker refuses to start a container with compression and max-file=1).
		LogConfig: container.LogConfig{Type: "local", Config: map[string]string{
			"max-size": strconv.FormatInt(spec.Limits.LogBytes, 10),
			"max-file": "1",
			"compress": "false",
		}},
		Runtime:    spec.Runtime,
		AutoRemove: false, // removed by nbsandboxd after inspecting
		Resources: container.Resources{
			Memory:     spec.Limits.MemoryBytes,
			MemorySwap: spec.Limits.MemoryBytes, // equal to Memory: no swap
			NanoCPUs:   spec.Limits.NanoCPUs,
			PidsLimit:  &pids,
		},
	}
	if spec.Runtime == "runsc" {
		gvisorPIDs(host, spec.Limits.PIDs)
	}
	return cfg, host
}

// gvisorPIDs: under gVisor the host's pids limit counts the sandbox's own
// threads (its kernel, written in Go, and a host thread or two for each of
// the notebook's), not the notebook's processes. Hitting it refuses a fork
// with ENOMEM at about 45% of the limit, or worse refuses gVisor a thread,
// and the whole sandbox dies. So the notebook's limit is RLIMIT_NPROC, which
// gVisor enforces inside the sandbox (processes and threads; this run's
// only, the sandbox has its own users), failing with EAGAIN as on runc. The
// host limit stays as a backstop, with room for gVisor's threads. (Not with
// runc: there RLIMIT_NPROC counts every process of uid 1000 on the host.)
func gvisorPIDs(host *container.HostConfig, pids int64) {
	backstop := 4*pids + 256
	host.PidsLimit = &backstop
	host.Ulimits = []*container.Ulimit{{Name: "nproc", Soft: pids, Hard: pids}}
}

func outputMode(m string) string {
	if m == "" {
		return "single"
	}
	return m
}

// LabelBuild marks package set build containers (not runs: the run
// reconciler never sees them).
const (
	LabelBuild       = "nbsandbox.build"
	LabelBuildWorker = "nbsandbox.build.worker"
)

// BuildContainerName is a build container's name.
func BuildContainerName(id string) string { return "nbsandbox-build-" + id }

// buildBuilderConfig turns a BuildSpec into Docker settings: a run's
// hardening (non-root, no capabilities, no new privileges, read-only root,
// limits, the configured runtime), but with network, to reach the package
// index, and no space files: only the request (read-only) and the output
// folder.
func buildBuilderConfig(spec isolation.BuildSpec) (*container.Config, *container.HostConfig) {
	init := true
	pids := spec.Limits.PIDs
	cfg := &container.Config{
		Image:  spec.Image,
		User:   "1000:1000",
		Env:    []string{"HOME=/tmp", "PYTHONUNBUFFERED=1", "PIP_NO_CACHE_DIR=1"},
		Labels: map[string]string{LabelBuild: spec.ID, LabelBuildWorker: spec.Worker},
	}
	host := &container.HostConfig{
		Binds:          []string{spec.InDir + ":/in:ro", spec.OutDir + ":/out:rw"},
		NetworkMode:    container.NetworkMode(spec.Network),
		IpcMode:        "private",
		Init:           &init,
		Privileged:     false,
		CapDrop:        []string{"ALL"},
		SecurityOpt:    []string{"no-new-privileges"},
		ReadonlyRootfs: true,
		// pip unpacks into /tmp before copying to /out: exec is needed for
		// the build environment's python.
		Tmpfs: map[string]string{"/tmp": "rw,nosuid,nodev,exec,size=" + strconv.FormatInt(spec.Limits.TmpfsBytes, 10)},
		LogConfig: container.LogConfig{Type: "local", Config: map[string]string{
			"max-size": strconv.FormatInt(spec.Limits.LogBytes, 10), "max-file": "1", "compress": "false",
		}},
		Runtime:    spec.Runtime,
		AutoRemove: false,
		Resources: container.Resources{
			Memory:     spec.Limits.MemoryBytes,
			MemorySwap: spec.Limits.MemoryBytes,
			NanoCPUs:   spec.Limits.NanoCPUs,
			PidsLimit:  &pids,
		},
	}
	if spec.Runtime == "runsc" {
		gvisorPIDs(host, spec.Limits.PIDs)
	}
	return cfg, host
}

// runBinds are a run's mounts: its space (or bundle), its output folder, and
// its space's package set, read-only, if it has one.
func runBinds(spec isolation.RunSpec, spaceMode string) []string {
	b := []string{spec.SpaceDir + ":/space:" + spaceMode, spec.RunDir + ":/run:rw"}
	if spec.PackagesDir != "" {
		b = append(b, spec.PackagesDir+":"+isolation.PackagesPath+":ro")
	}
	return b
}
