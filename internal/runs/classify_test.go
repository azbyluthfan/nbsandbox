// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package runs

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/azbyluthfan/nbsandbox/internal/isolation"
)

func status(state string, mod ...func(*WrapperStatus)) *WrapperStatus {
	s := &WrapperStatus{SchemaVersion: 1, State: state, Rendered: true}
	for _, m := range mod {
		m(s)
	}
	return s
}

func ptr[T any](v T) *T { return &v }

func execError(cell int, ename, evalue string) func(*WrapperStatus) {
	return func(s *WrapperStatus) {
		s.Error = &struct {
			CellIndex      *int   `json:"cell_index"`
			ExecutionCount *int   `json:"execution_count"`
			EName          string `json:"ename"`
			EValue         string `json:"evalue"`
		}{CellIndex: ptr(cell), ExecutionCount: ptr(cell + 1), EName: ename, EValue: evalue}
	}
}

// One case per status row and per precedence rule.
func TestClassify(t *testing.T) {
	stopped := isolation.RunResult{Stopped: true, ExitCode: 143}
	cases := []struct {
		name                string
		ev                  Evidence
		status, kind, fault string
		msg                 string
	}{
		{"succeeded", Evidence{Status: status("succeeded")}, StatusSucceeded, "", "", ""},
		{"failed: exception in cell 3", Evidence{Status: status("execution_error", execError(3, "ValueError", "boom"))},
			StatusFailed, KindExecutionError, FaultNotebook, "cell 3 raised ValueError: boom"},
		{"failed: pids limit", Evidence{Status: status("execution_error", execError(0, "BlockingIOError", "[Errno 11] Resource temporarily unavailable"),
			func(s *WrapperStatus) { s.CGroup.PIDsMaxEvents = ptr[int64](2) })}, StatusFailed, KindResourcePIDs, FaultNotebook, "process limit"},
		{"failed: process limit without a pids counter (gVisor)", Evidence{Status: status("execution_error",
			execError(0, "BlockingIOError", "[Errno 11] Resource temporarily unavailable"))},
			StatusFailed, KindResourcePIDs, FaultNotebook, "process limit"},
		{"failed: thread refused without a pids counter (gVisor)", Evidence{Status: status("execution_error",
			execError(0, "RuntimeError", "can't start new thread"))},
			StatusFailed, KindResourcePIDs, FaultNotebook, "process limit"},
		{"failed: EAGAIN with a pids counter that never hit the limit stays the notebook's error", Evidence{Status: status("execution_error",
			execError(0, "BlockingIOError", "[Errno 11] Resource temporarily unavailable"),
			func(s *WrapperStatus) { s.CGroup.PIDsMaxEvents = ptr[int64](0) })},
			StatusFailed, KindExecutionError, FaultNotebook, "Resource temporarily unavailable"},
		{"failed: /tmp full", Evidence{Status: status("execution_error", execError(0, "OSError", "[Errno 28] No space left on device"))},
			StatusFailed, KindResourceTmp, FaultNotebook, "temporary space"},
		{"failed: missing package", Evidence{Status: status("execution_error", execError(1, "ModuleNotFoundError", "No module named 'torch'"))},
			StatusFailed, KindMissingPackage, FaultNotebook, "/v1/runtime"},
		{"oom: container flag", Evidence{Result: isolation.RunResult{OOMKilled: true}, Status: status("dead_kernel"), MemoryLimit: 2 << 30},
			StatusOOM, KindOOM, FaultNotebook, "out of memory"},
		{"oom: cgroup counter only, with peak", Evidence{Status: status("dead_kernel", func(s *WrapperStatus) {
			s.CGroup.OOMKill, s.CGroup.MemoryPeakBytes = ptr[int64](1), ptr[int64](2<<30)
		}), MemoryLimit: 2 << 30}, StatusOOM, KindOOM, FaultNotebook, "peak 2.0 GiB of the 2.0 GiB limit"},
		{"oom: wrapper itself killed, no status", Evidence{Result: isolation.RunResult{OOMKilled: true, ExitCode: 137}}, StatusOOM, KindOOM, FaultNotebook, ""},
		{"kernel died without oom", Evidence{Status: status("dead_kernel")}, StatusKernelDied, KindKernelDied, FaultNotebook, "native code"},
		{"timeout", Evidence{Cause: ErrTimeout, Result: stopped, Status: status("terminated")}, StatusTimeout, KindTimeout, FaultNotebook, ""},
		{"cancelled", Evidence{Cause: ErrCancelled, Result: stopped}, StatusCancelled, KindCancelled, FaultCaller, ""},
		{"shutdown", Evidence{Cause: ErrShutdown, Result: stopped}, StatusInterrupted, KindShutdown, FaultPlatform, "not at fault"},
		{"cancel raced completion: completion wins", Evidence{Cause: ErrCancelled, Status: status("succeeded")}, StatusSucceeded, "", "", ""},
		{"start failed", Evidence{BackendErr: fmt.Errorf("%w: no such image", isolation.ErrStart)}, StatusPlatformError, KindStart, FaultPlatform, "not at fault"},
		{"docker lost", Evidence{BackendErr: fmt.Errorf("%w: EOF", isolation.ErrDockerLost)}, StatusInterrupted, KindDockerLost, FaultPlatform, "not at fault"},
		{"docker lost beats a pending cancel", Evidence{Cause: ErrShutdown, BackendErr: isolation.ErrDockerLost}, StatusInterrupted, KindShutdown, FaultPlatform, ""},
		{"stopped from outside", Evidence{Status: status("terminated")}, StatusInterrupted, KindExternalStop, FaultPlatform, "outside nbsandboxd"},
		{"no status file", Evidence{Result: isolation.RunResult{ExitCode: 1}}, StatusPlatformError, KindNoStatus, FaultPlatform, "code 1"},
		{"wrapper error", Evidence{Status: status("wrapper_error", func(s *WrapperStatus) { s.Message = "kernel failed to start" })},
			StatusPlatformError, KindWrapper, FaultPlatform, "kernel failed to start"},
		{"unknown state", Evidence{Status: status("weird")}, StatusPlatformError, KindNoStatus, FaultPlatform, "weird"},
		{"other backend error", Evidence{BackendErr: errors.New("boom")}, StatusPlatformError, KindWrapper, FaultPlatform, "boom"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := Classify(c.ev)
			if o.Status != c.status || o.ErrorKind != c.kind || o.Fault != c.fault || !strings.Contains(o.Message, c.msg) {
				t.Fatalf("got %s/%s/%s %q; want %s/%s/%s containing %q", o.Status, o.ErrorKind, o.Fault, o.Message, c.status, c.kind, c.fault, c.msg)
			}
			// A platform problem must never be reported as the notebook's fault.
			if (c.ev.BackendErr != nil || c.ev.Status == nil && !c.ev.Result.OOMKilled) && o.Fault == FaultNotebook {
				t.Fatal("notebook blamed for a platform problem")
			}
		})
	}
}

func TestClassifyKeepsCellDetails(t *testing.T) {
	o := Classify(Evidence{Status: status("execution_error", execError(3, "ValueError", "boom"))})
	if o.Cell == nil || *o.Cell.CellIndex != 3 || *o.Cell.ExecutionCount != 4 || o.Cell.ExceptionType != "ValueError" || !o.Rendered {
		t.Fatalf("cell = %+v", o.Cell)
	}
	if o := Classify(Evidence{Status: status("succeeded", func(s *WrapperStatus) { s.Rendered = false })}); o.Rendered {
		t.Fatal("render failure lost")
	}
}
