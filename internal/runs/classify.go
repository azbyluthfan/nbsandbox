// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package runs

import (
	"errors"
	"fmt"
	"strings"

	"github.com/azbyluthfan/nbsandbox/internal/isolation"
)

// Run statuses.
const (
	StatusQueued        = "queued"
	StatusRunning       = "running"
	StatusSucceeded     = "succeeded"
	StatusFailed        = "failed"
	StatusOOM           = "oom"
	StatusKernelDied    = "kernel_died"
	StatusTimeout       = "timeout"
	StatusCancelled     = "cancelled"
	StatusInterrupted   = "interrupted"
	StatusPlatformError = "platform_error"
)

// Faults: who caused the outcome.
const (
	FaultNotebook = "notebook"
	FaultCaller   = "caller"
	FaultPlatform = "platform"
)

// Error kinds refine a status.
const (
	KindExecutionError = "execution_error"
	KindResourcePIDs   = "resource_pids"
	KindResourceTmp    = "resource_tmp"
	KindResourceDisk   = "resource_disk"   // the space went over its quota while the run wrote
	KindMissingPackage = "missing_package" // an import the image doesn't have
	KindOOM            = "oom"
	KindKernelDied     = "kernel_died"
	KindTimeout        = "timeout"
	KindCancelled      = "cancelled"
	KindShutdown       = "shutdown"
	KindDockerLost     = "docker_lost"
	KindExternalStop   = "external_stop" // stopped by something other than nbsandboxd
	KindOrphan         = "orphaned"
	KindStart          = "start_failed"
	KindNoStatus       = "no_status"
	KindWrapper        = "runner_error"
)

// Causes attached to a run's context when nbsandboxd ends it.
var (
	ErrCancelled = errors.New("cancelled by the caller")
	ErrShutdown  = errors.New("nbsandboxd is shutting down")
	ErrLeaseLost = errors.New("the worker lost the run's lease")
	ErrTimeout   = errors.New("run exceeded its time limit")
	ErrDiskQuota = errors.New("the space went over its quota while the run was writing")
)

// WrapperStatus is runner-image/status.schema.json (version 1).
type WrapperStatus struct {
	SchemaVersion int    `json:"schema_version"`
	State         string `json:"state"`
	Message       string `json:"message"`
	Rendered      bool   `json:"rendered"`
	RenderError   string `json:"render_error"`
	Error         *struct {
		CellIndex      *int   `json:"cell_index"`
		ExecutionCount *int   `json:"execution_count"`
		EName          string `json:"ename"`
		EValue         string `json:"evalue"`
	} `json:"error"`
	CGroup struct {
		OOM             *int64 `json:"oom"`
		OOMKill         *int64 `json:"oom_kill"`
		MemoryPeakBytes *int64 `json:"memory_peak_bytes"`
		PeakSampled     bool   `json:"memory_peak_sampled"`
		MemoryMaxBytes  *int64 `json:"memory_max_bytes"`
		PIDsMaxEvents   *int64 `json:"pids_max_events"`
		CPUUsageUsec    *int64 `json:"cpu_usage_usec"`
	} `json:"cgroup"`
}

// Evidence is everything known when a run ends.
type Evidence struct {
	Cause       error // why nbsandboxd ended the run: ErrCancelled, ErrShutdown, ErrTimeout, or nil
	BackendErr  error // isolation.ErrStart / isolation.ErrDockerLost, or other
	Result      isolation.RunResult
	Status      *WrapperStatus // nil when missing or unreadable
	MemoryLimit int64
}

// CellError is the failing cell, when there is one.
type CellError struct {
	CellIndex      *int   `json:"cell_index"`
	ExecutionCount *int   `json:"execution_count"`
	ExceptionType  string `json:"exception_type"`
	Message        string `json:"exception_message"`
}

// Outcome is a run's classified result.
type Outcome struct {
	Status    string
	ErrorKind string
	Fault     string
	Message   string
	Cell      *CellError
	Rendered  bool
}

func i64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// Classify maps evidence to a status, the same way for every backend
// (precedence: nbsandboxd's own reason, then OOM, then the status file).
func Classify(ev Evidence) Outcome {
	st := ev.Status

	// The platform stopped the run for writing more than its disk allows
	// (a Kubernetes eviction): the notebook's doing, like going over quota.
	if ev.Result.DiskExceeded && ev.Cause == nil {
		return Outcome{Status: StatusFailed, ErrorKind: KindResourceDisk, Fault: FaultNotebook,
			Message: "the run was stopped: it wrote more files than its disk allows"}
	}

	// 1. nbsandboxd stopped the run itself: the reason it did so decides.
	// A cause only counts when the container was actually stopped; a run
	// that finished on its own just before a cancel keeps its real outcome
	// (the first terminal status wins).
	if ev.Cause != nil && (ev.Result.Stopped || ev.BackendErr != nil) {
		switch {
		case errors.Is(ev.Cause, ErrCancelled):
			return Outcome{Status: StatusCancelled, ErrorKind: KindCancelled, Fault: FaultCaller, Message: "cancelled by the caller"}
		case errors.Is(ev.Cause, ErrShutdown):
			return Outcome{Status: StatusInterrupted, ErrorKind: KindShutdown, Fault: FaultPlatform,
				Message: "nbsandboxd shut down while the run was active; the notebook was not at fault"}
		case errors.Is(ev.Cause, ErrLeaseLost):
			return Outcome{Status: StatusInterrupted, ErrorKind: KindOrphan, Fault: FaultPlatform,
				Message: "the worker running it lost contact with the database; the notebook was not at fault"}
		case errors.Is(ev.Cause, ErrTimeout):
			return Outcome{Status: StatusTimeout, ErrorKind: KindTimeout, Fault: FaultNotebook, Message: "the run exceeded its time limit"}
		case errors.Is(ev.Cause, ErrDiskQuota):
			return Outcome{Status: StatusFailed, ErrorKind: KindResourceDisk, Fault: FaultNotebook,
				Message: "the run was stopped: the files it wrote took the space over its quota"}
		}
	}

	// 2. The platform failed.
	if ev.BackendErr != nil {
		if errors.Is(ev.BackendErr, isolation.ErrDockerLost) {
			return Outcome{Status: StatusInterrupted, ErrorKind: KindDockerLost, Fault: FaultPlatform,
				Message: "Docker became unavailable during the run; the notebook was not at fault"}
		}
		kind := KindWrapper
		if errors.Is(ev.BackendErr, isolation.ErrStart) {
			kind = KindStart
		}
		return Outcome{Status: StatusPlatformError, ErrorKind: kind, Fault: FaultPlatform,
			Message: "the run could not be executed (" + ev.BackendErr.Error() + "); the notebook was not at fault"}
	}

	// 3. Memory: the container flag or the cgroup's OOM-kill counter
	// (usually the kernel process is killed, not the wrapper).
	if ev.Result.OOMKilled || (st != nil && i64(st.CGroup.OOMKill) > 0) {
		msg := fmt.Sprintf("the notebook ran out of memory (limit %s)", bytesHuman(ev.MemoryLimit))
		if st != nil && st.CGroup.MemoryPeakBytes != nil {
			msg = fmt.Sprintf("the notebook ran out of memory: peak %s of the %s limit (files in /tmp count toward it)",
				bytesHuman(*st.CGroup.MemoryPeakBytes), bytesHuman(ev.MemoryLimit))
		}
		return Outcome{Status: StatusOOM, ErrorKind: KindOOM, Fault: FaultNotebook, Message: msg, Rendered: st != nil && st.Rendered}
	}

	// 4. Without the wrapper's report we can't blame the notebook.
	if st == nil {
		return Outcome{Status: StatusPlatformError, ErrorKind: KindNoStatus, Fault: FaultPlatform,
			Message: fmt.Sprintf("the runner exited (code %d) without reporting a status; the notebook was not at fault", ev.Result.ExitCode)}
	}

	switch st.State {
	case "succeeded":
		return Outcome{Status: StatusSucceeded, Fault: "", Rendered: st.Rendered}
	case "execution_error":
		o := Outcome{Status: StatusFailed, ErrorKind: KindExecutionError, Fault: FaultNotebook, Rendered: st.Rendered}
		if st.Error != nil {
			o.Cell = &CellError{CellIndex: st.Error.CellIndex, ExecutionCount: st.Error.ExecutionCount,
				ExceptionType: st.Error.EName, Message: st.Error.EValue}
		}
		ename, evalue := "", ""
		if o.Cell != nil {
			ename, evalue = o.Cell.ExceptionType, o.Cell.Message
		}
		switch {
		case i64(st.CGroup.PIDsMaxEvents) > 0:
			o.ErrorKind = KindResourcePIDs
			o.Message = "the notebook hit the process limit and could not create more processes"
		case st.CGroup.PIDsMaxEvents == nil && refusedProcess(evalue):
			o.ErrorKind = KindResourcePIDs
			o.Message = "the notebook hit the process limit and could not create more processes or threads"
		case strings.Contains(evalue, "No space left on device"):
			o.ErrorKind = KindResourceTmp
			o.Message = "the notebook ran out of temporary space (/tmp is limited)"
		case ename == "ModuleNotFoundError":
			o.ErrorKind = KindMissingPackage
			o.Message = evalue + " — this package is not in the runner image; see GET /v1/runtime for what is installed"
		default:
			o.Message = cellMessage(o.Cell)
		}
		return o
	case "dead_kernel":
		return Outcome{Status: StatusKernelDied, ErrorKind: KindKernelDied, Fault: FaultNotebook,
			Message: "the notebook's kernel died without running out of memory (for example, a crash in native code)", Rendered: st.Rendered}
	case "terminated":
		// The runner got SIGTERM that nbsandboxd didn't send: Docker or the
		// host stopped it.
		return Outcome{Status: StatusInterrupted, ErrorKind: KindExternalStop, Fault: FaultPlatform,
			Message: "the run was stopped from outside nbsandboxd (for example, Docker restarted); the notebook was not at fault"}
	case "wrapper_error":
		return Outcome{Status: StatusPlatformError, ErrorKind: KindWrapper, Fault: FaultPlatform,
			Message: "the runner failed before executing the notebook: " + st.Message}
	}
	return Outcome{Status: StatusPlatformError, ErrorKind: KindNoStatus, Fault: FaultPlatform,
		Message: fmt.Sprintf("the runner reported an unknown state %q", st.State)}
}

// refusedProcess: where the cgroup keeps no pids.events counter (gVisor,
// whose limit is RLIMIT_NPROC), a process or thread refused at the limit
// fails with EAGAIN.
func refusedProcess(evalue string) bool {
	return strings.Contains(evalue, "Resource temporarily unavailable") || strings.Contains(evalue, "can't start new thread")
}

func cellMessage(c *CellError) string {
	if c == nil {
		return "a cell raised an exception"
	}
	where := "a cell"
	if c.CellIndex != nil {
		where = fmt.Sprintf("cell %d", *c.CellIndex)
	}
	return fmt.Sprintf("%s raised %s: %s", where, c.ExceptionType, c.Message)
}

func bytesHuman(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MiB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%d bytes", n)
}
