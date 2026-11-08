// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package runs

import (
	"errors"
	"testing"
)

func TestMemWakeups(t *testing.T) {
	mw := newMemWakeups()
	var w Wakeups = mw
	a1, a2, b := w.Wait("a"), w.Wait("a"), w.Wait("b")
	w.Notify("a")
	for i, ch := range []<-chan struct{}{a1, a2} {
		select {
		case <-ch:
		default:
			t.Fatalf("waiter %d on a was not woken", i)
		}
	}
	select {
	case <-b:
		t.Fatal("b must not wake for a's event")
	default:
	}
	select {
	case <-w.Wait("a"):
		t.Fatal("a new wait must wait for the next event")
	default:
	}
	w.Notify("nobody-waiting") // must not panic
	c := w.Wait("c")
	mw.NotifyAll()
	select {
	case <-c:
	default:
		t.Fatal("NotifyAll must wake every waiter")
	}
}

func TestExecutorCancelUnknownRun(t *testing.T) {
	ex := newExecutor(&core{})
	if ex.Cancel("not-running", errors.New("x")) {
		t.Fatal("cancel of a run that isn't executing must report false")
	}
}
