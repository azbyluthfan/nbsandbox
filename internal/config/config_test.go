// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func noEnv(string) string { return "" }

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func writeFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "settings.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDefaultsAreValid(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Fatalf("defaults must validate: %v", err)
	}
}

func TestLoadNoFileUsesDefaults(t *testing.T) {
	s, err := Load("", noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if s.Limits.Memory != 2*GiB || s.Runs.Concurrency != 2 || s.Limits.Timeout != 10*time.Minute {
		t.Fatalf("unexpected defaults: %+v", s)
	}
}

func TestLoadEmptyFileUsesDefaults(t *testing.T) {
	if _, err := Load(writeFile(t, ""), noEnv); err != nil {
		t.Fatal(err)
	}
}

func TestLoadFileOverridesDefaults(t *testing.T) {
	s, err := Load(writeFile(t, `
data_dir: /data/nbsandbox
docker:
  runtime: runsc
  image: nbsandbox-runner:1.0.0
limits:
  memory: 4GiB
  tmp_size: 512MiB
  timeout: 30m
runs:
  concurrency: 4
  dependency_check: reject
`), noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if s.DataDir != "/data/nbsandbox" || s.Docker.Runtime != "runsc" || s.Limits.Memory != 4*GiB ||
		s.Limits.TmpSize != 512*MiB || s.Limits.Timeout != 30*time.Minute ||
		s.Runs.Concurrency != 4 || s.Runs.DependencyCheck != "reject" {
		t.Fatalf("file values not applied: %+v", s)
	}
	if s.Limits.PIDs != 256 {
		t.Fatalf("unset keys must keep defaults, pids = %d", s.Limits.PIDs)
	}
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	_, err := Load(writeFile(t, "limits:\n  memroy: 4GiB\n"), noEnv)
	if err == nil || !strings.Contains(err.Error(), "memroy") {
		t.Fatalf("want error naming the unknown key, got %v", err)
	}
}

func TestEnvOverridesFile(t *testing.T) {
	s, err := Load(writeFile(t, "data_dir: /from/file\n"), envOf(map[string]string{
		"NBSANDBOX_DATA_DIR":        "/from/env",
		"NBSANDBOX_RUN_CONCURRENCY": "3",
		"DOCKER_HOST":               "unix:///from/docker_host.sock",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if s.DataDir != "/from/env" || s.Runs.Concurrency != 3 || s.Docker.Host != "unix:///from/docker_host.sock" {
		t.Fatalf("env not applied: %+v", s)
	}
}

func TestNbsandboxDockerHostWinsOverDockerHost(t *testing.T) {
	s, err := Load("", envOf(map[string]string{
		"NBSANDBOX_DOCKER_HOST": "tcp://proxy:2375",
		"DOCKER_HOST":           "unix:///var/run/docker.sock",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if s.Docker.Host != "tcp://proxy:2375" {
		t.Fatalf("got %q", s.Docker.Host)
	}
}

func TestValidateReportsEveryProblem(t *testing.T) {
	cases := map[string]func(*Settings){
		"data_dir":                func(s *Settings) { s.DataDir = "relative/path" },
		"log_level":               func(s *Settings) { s.LogLevel = "loud" },
		"docker.runtime":          func(s *Settings) { s.Docker.Runtime = "kata" },
		"docker.image":            func(s *Settings) { s.Docker.Image = "" },
		"docker.host":             func(s *Settings) { s.Docker.Host = "http://example" },
		"limits.memory":           func(s *Settings) { s.Limits.Memory = 64 * MiB },
		"limits.cpus":             func(s *Settings) { s.Limits.CPUs = 0 },
		"limits.pids":             func(s *Settings) { s.Limits.PIDs = 1 },
		"limits.tmp_size":         func(s *Settings) { s.Limits.TmpSize = 4 * GiB },
		"limits.log_cap":          func(s *Settings) { s.Limits.LogCap = 1 },
		"limits.timeout":          func(s *Settings) { s.Limits.Timeout = 0 },
		"uploads":                 func(s *Settings) { s.Uploads.MaxSpace = 1 },
		"runs.concurrency":        func(s *Settings) { s.Runs.Concurrency = 0 },
		"quotas.max_spaces":       func(s *Settings) { s.Quotas.MaxSpaces = -1 },
		"quotas.owners[1]":        func(s *Settings) { s.Quotas.Owners = []OwnerQuota{{Owner: "a"}, {Owner: "a"}} },
		"runs.retention":          func(s *Settings) { s.Runs.Retention = 30 * time.Second },
		"runs.idempotency_window": func(s *Settings) { s.Runs.IdempotencyWindow = time.Second },
		"runs.cleanup_every":      func(s *Settings) { s.Runs.CleanupEvery = time.Second },
		"runs.stop_grace":         func(s *Settings) { s.Runs.StopGrace = time.Hour },
		"runs.dependency_check":   func(s *Settings) { s.Runs.DependencyCheck = "maybe" },
		"shutdown_grace":          func(s *Settings) { s.ShutdownGrace = -time.Second },
		"listen":                  func(s *Settings) { s.Listen = "" },
		"users_file":              func(s *Settings) { s.UsersFile = "" },
		"limits.shm_size":         func(s *Settings) { s.Limits.ShmSize = 0 },
	}
	all := Default()
	for key, breakIt := range cases {
		t.Run(key, func(t *testing.T) {
			s := Default()
			breakIt(&s)
			err := s.Validate()
			if err == nil || !strings.Contains(err.Error(), key) {
				t.Fatalf("want error mentioning %q, got %v", key, err)
			}
		})
		breakIt(&all)
	}
	// Everything broken at once: every problem is reported, not just the first.
	err := all.Validate()
	for key := range cases {
		if err == nil || !strings.Contains(err.Error(), key+":") {
			t.Errorf("combined error is missing %q", key)
		}
	}
}

func TestUISettings(t *testing.T) {
	s, err := Load("", noEnv)
	if err != nil || s.UI.Enabled || s.UI.Listen != "127.0.0.1:8090" {
		t.Fatalf("UI must be off by default on loopback: %+v %v", s.UI, err)
	}
	s, err = Load("", envOf(map[string]string{"NBSANDBOX_UI_ENABLED": "true", "NBSANDBOX_UI_LISTEN": "0.0.0.0:9000"}))
	if err != nil || !s.UI.Enabled || s.UI.Listen != "0.0.0.0:9000" {
		t.Fatalf("env: %+v %v", s.UI, err)
	}
	if _, err := Load("", envOf(map[string]string{"NBSANDBOX_UI_ENABLED": "maybe"})); err == nil {
		t.Fatal("bad bool accepted")
	}
	bad := Default()
	bad.UI = UI{Enabled: true, Listen: bad.Listen}
	if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "ui.listen") {
		t.Fatalf("same port: %v", err)
	}
}

func TestParseSize(t *testing.T) {
	good := map[string]Size{
		"2GiB": 2 * GiB, "256MiB": 256 * MiB, "64KiB": 64 * KiB, "10MB": 10_000_000,
		"1.5GiB": GiB + 512*MiB, "1048576": MiB, " 3 GiB ": 3 * GiB, "0": 0,
	}
	for in, want := range good {
		got, err := ParseSize(in)
		if err != nil || got != want {
			t.Errorf("ParseSize(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "lots", "-1", "-5MiB", "1TiBx"} {
		if _, err := ParseSize(in); err == nil {
			t.Errorf("ParseSize(%q) should fail", in)
		}
	}
}

func TestSizeString(t *testing.T) {
	for z, want := range map[Size]string{2 * GiB: "2GiB", 256 * MiB: "256MiB", 3 * KiB: "3KiB", 1000: "1000B", 0: "0B"} {
		if z.String() != want {
			t.Errorf("%d.String() = %q, want %q", int64(z), z.String(), want)
		}
	}
}

// The repository's settings file documents every key with its default: it
// loads, and it changes nothing (only a database URL must be supplied).
func TestRepositorySettingsFileIsTheDefaults(t *testing.T) {
	env := envOf(map[string]string{"NBSANDBOX_DATABASE_URL": "postgres://u:p@db:5432/nbsandbox"})
	got, err := Load("../../config/settings.yaml", env)
	if err != nil {
		t.Fatalf("config/settings.yaml: %v", err)
	}
	want, err := Load("", env)
	if err != nil {
		t.Fatal(err)
	}
	// auth.basic is unset by default and written out as true: the same.
	if got.Auth.BasicEnabled() != want.Auth.BasicEnabled() {
		t.Fatalf("auth.basic: file %v, default %v", got.Auth.BasicEnabled(), want.Auth.BasicEnabled())
	}
	got.Auth.Basic, want.Auth.Basic = nil, nil
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("config/settings.yaml differs from the defaults:\n file %+v\n defaults %+v", got, want)
	}
}

// The data folder's and users file's paths are cleaned, as Docker cleans a bind mount's path.
func TestDataDirIsCleaned(t *testing.T) {
	s, err := Load("", envOf(map[string]string{
		"NBSANDBOX_DATABASE_URL": "postgres://u:p@db:5432/nbsandbox",
		"NBSANDBOX_DATA_DIR":     "/home/runner/work/nbsandbox/../nbsandbox-data/",
		"NBSANDBOX_USERS_FILE":   "/home/runner/work/nbsandbox/../nbsandbox-data/users.yaml",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if s.DataDir != "/home/runner/work/nbsandbox-data" || s.UsersFile != "/home/runner/work/nbsandbox-data/users.yaml" {
		t.Fatalf("data_dir %q, users_file %q", s.DataDir, s.UsersFile)
	}
}
