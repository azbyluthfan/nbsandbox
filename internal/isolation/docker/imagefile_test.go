// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package docker

import (
	"context"
	"encoding/base64"
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestReadImageFile(t *testing.T) {
	ctx := context.Background()
	f := newFakeDocker()
	f.logs = multiplex(base64.StdEncoding.EncodeToString([]byte("the index")), "")
	f.exit <- 0
	b, err := ReadImageFile(ctx, f, "runner:test", "", "/opt/x", 1<<20)
	if err != nil || string(b) != "the index" {
		t.Fatalf("%q %v", b, err)
	}
	// Hardened like a run, no mounts, and removed afterwards.
	cfg, host := f.created.cfg, f.created.host
	if host.NetworkMode != "none" || !host.ReadonlyRootfs || len(host.Binds) != 0 || !slices.Equal(host.CapDrop, []string{"ALL"}) ||
		cfg.User != "1000:1000" || cfg.Labels[LabelExport] != "/opt/x" || cfg.Labels[LabelRun] != "" || !strings.HasPrefix(f.created.name, "nbsandbox-export-") {
		t.Fatalf("container %+v %+v", cfg, host)
	}
	if !slices.Contains(f.calls, "remove") {
		t.Fatalf("not removed: %v", f.calls)
	}

	// Failures: the file is missing (non-zero exit), too large, not base64.
	f = newFakeDocker()
	f.logs = multiplex("", "")
	f.inspect.ExitCode = 1
	f.exit <- 1
	if _, err := ReadImageFile(ctx, f, "runner:test", "", "/opt/x", 1<<20); err == nil {
		t.Fatal("a failed export was accepted")
	}
	f = newFakeDocker()
	f.logs = multiplex(base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 100))), "")
	f.exit <- 0
	if _, err := ReadImageFile(ctx, f, "runner:test", "", "/opt/x", 10); err == nil {
		t.Fatal("an oversized file was accepted")
	}
	f = newFakeDocker()
	f.logs = multiplex("not base64!", "")
	f.exit <- 0
	if _, err := ReadImageFile(ctx, f, "runner:test", "", "/opt/x", 1<<20); err == nil {
		t.Fatal("garbage was accepted")
	}
	f = newFakeDocker()
	f.createErr = errors.New("proxy says no")
	if _, err := ReadImageFile(ctx, f, "runner:test", "", "/opt/x", 1<<20); err == nil {
		t.Fatal("a create error was ignored")
	}
}
