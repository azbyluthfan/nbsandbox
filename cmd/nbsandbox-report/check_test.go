// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/report"
)

func TestCheckRefusesBadUsage(t *testing.T) {
	t.Setenv("NBSANDBOX_CHECK_A_PASSWORD", "a")
	t.Setenv("NBSANDBOX_CHECK_B_PASSWORD", "b")
	for name, args := range map[string][]string{
		"no url":              {"-user-a", "x", "-user-b", "y"},
		"same users":          {"-url", "https://n.example", "-user-a", "x", "-user-b", "x"},
		"not http":            {"-url", "ftp://n.example", "-user-a", "x", "-user-b", "y"},
		"credentials in http": {"-url", "http://n.example.com", "-user-a", "x", "-user-b", "y"},
		"unknown flag":        {"-nope"},
	} {
		if code := runCheckCommand(args); code != 2 {
			t.Errorf("%s: exit %d, want 2", name, code)
		}
	}
}

func TestIdentityFromEnvironment(t *testing.T) {
	t.Setenv("NBSANDBOX_CHECK_A_PASSWORD", "pw")
	if id, err := identity("alice", "A"); err != nil || id.Password != "pw" || id.Name != "alice" {
		t.Errorf("%+v %v", id, err)
	}
	t.Setenv("NBSANDBOX_CHECK_A_TOKEN", "tok")
	if _, err := identity("alice", "A"); err == nil {
		t.Error("both a password and a token must be refused")
	}
	t.Setenv("NBSANDBOX_CHECK_A_PASSWORD", "")
	if id, err := identity("alice", "A"); err != nil || id.Token != "tok" {
		t.Errorf("%+v %v", id, err)
	}
	if _, err := identity("bob", "B"); err == nil {
		t.Error("no credentials must be refused")
	}
}

func TestLocalHosts(t *testing.T) {
	for h, want := range map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true, "a.localhost": true, "example.com": false, "10.0.0.1": false} {
		if isLocal(h) != want {
			t.Errorf("%s: %v", h, !want)
		}
	}
}

// The raw findings are written so that nbsandbox-report -in renders them.
func TestWriteFindingsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	run := report.Run{Runtime: "runc", Facts: report.Facts{Backend: "docker", Checked: "https://n.example", RecordedAt: time.Now()},
		Findings: map[string]report.Finding{"proc-list": {ID: "proc-list", Pass: true, Observed: "2 processes"}}}
	if err := writeFindings(dir, run); err != nil {
		t.Fatal(err)
	}
	runs, err := report.Load(dir)
	if err != nil || len(runs) != 1 || runs[0].Facts.Checked != "https://n.example" || !runs[0].Findings["proc-list"].Pass {
		t.Fatalf("%+v %v", runs, err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "runc", "proc-list.json")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("%v %v", fi.Mode(), err)
	}
	if !contains([]string{"a", "b"}, "b") || contains(nil, "a") || checkNames() == "" {
		t.Error("helpers")
	}
}
