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
	// Without sign-in, only on loopback, unless explicitly allowed (a
	// container published on the host's loopback).
	beyond := map[string]string{"NBSANDBOX_UI_ENABLED": "true", "NBSANDBOX_UI_LISTEN": "0.0.0.0:9000"}
	if _, err := Load("", envOf(beyond)); err == nil || !strings.Contains(err.Error(), "loopback only") {
		t.Fatalf("no sign-in beyond loopback: %v", err)
	}
	beyond["NBSANDBOX_UI_NO_LOGIN_BEYOND_LOOPBACK"] = "true"
	s, err = Load("", envOf(beyond))
	if err != nil || !s.UI.Enabled || s.UI.Listen != "0.0.0.0:9000" {
		t.Fatalf("env: %+v %v", s.UI, err)
	}
	// With sign-in: anywhere, given the public address.
	s, err = Load("", envOf(map[string]string{"NBSANDBOX_UI_ENABLED": "true", "NBSANDBOX_UI_LISTEN": "0.0.0.0:9000",
		"NBSANDBOX_UI_AUTH": "basic", "NBSANDBOX_UI_PUBLIC_URL": "https://nb.example.com",
		"NBSANDBOX_UI_SESSION_IDLE": "30m", "NBSANDBOX_UI_SESSION_MAX": "8h"}))
	if err != nil || s.UI.Auth != "basic" || s.UI.Session.Idle != 30*time.Minute || s.UI.Session.Max != 8*time.Hour {
		t.Fatalf("basic: %+v %v", s.UI, err)
	}
	for name, env := range map[string]map[string]string{
		"no public url":    {"NBSANDBOX_UI_AUTH": "basic"},
		"http public url":  {"NBSANDBOX_UI_AUTH": "basic", "NBSANDBOX_UI_PUBLIC_URL": "http://nb.example.com"},
		"a path":           {"NBSANDBOX_UI_AUTH": "basic", "NBSANDBOX_UI_PUBLIC_URL": "https://nb.example.com/ui"},
		"unknown mode":     {"NBSANDBOX_UI_AUTH": "magic"},
		"oidc without idp": {"NBSANDBOX_UI_AUTH": "oidc", "NBSANDBOX_UI_PUBLIC_URL": "https://nb.example.com", "NBSANDBOX_UI_OIDC_CLIENT_ID": "ui", "NBSANDBOX_UI_OIDC_CLIENT_SECRET": "s"},
		"oidc no secret": {"NBSANDBOX_UI_AUTH": "oidc", "NBSANDBOX_UI_PUBLIC_URL": "https://nb.example.com", "NBSANDBOX_UI_OIDC_CLIENT_ID": "ui",
			"NBSANDBOX_OIDC_ISSUER": "https://idp.example", "NBSANDBOX_OIDC_AUDIENCE": "nb", "NBSANDBOX_OIDC_ID_PREFIX": "kc", "NBSANDBOX_OIDC_ID_CLAIMS": "sub"},
		"idle over max": {"NBSANDBOX_UI_AUTH": "basic", "NBSANDBOX_UI_PUBLIC_URL": "https://nb.example.com", "NBSANDBOX_UI_SESSION_IDLE": "13h"},
	} {
		env["NBSANDBOX_UI_ENABLED"] = "true"
		if _, err := Load("", envOf(env)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	s, err = Load("", envOf(map[string]string{"NBSANDBOX_UI_ENABLED": "true", "NBSANDBOX_UI_AUTH": "oidc",
		"NBSANDBOX_UI_PUBLIC_URL": "http://127.0.0.1:8090", "NBSANDBOX_UI_OIDC_CLIENT_ID": "nbsandbox-ui",
		"NBSANDBOX_UI_OIDC_CLIENT_SECRET": "s", "NBSANDBOX_UI_OIDC_SCOPES": "openid nbsandbox.read",
		"NBSANDBOX_OIDC_ISSUER": "https://idp.example", "NBSANDBOX_OIDC_AUDIENCE": "nb", "NBSANDBOX_OIDC_ID_PREFIX": "kc", "NBSANDBOX_OIDC_ID_CLAIMS": "sub"}))
	if err != nil || s.UI.OIDC.ClientSecret != "s" || len(s.UI.OIDC.Scopes) != 2 {
		t.Fatalf("oidc: %+v %v", s.UI, err)
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

func TestLoginLimitTrustedProxiesAndIntrospectionFromEnv(t *testing.T) {
	s, err := Load("", envOf(map[string]string{
		"NBSANDBOX_TRUSTED_PROXIES":                  "10.0.0.0/8, 172.18.0.5",
		"NBSANDBOX_LOGIN_LIMIT_FAILURES":             "8",
		"NBSANDBOX_OIDC_ISSUER":                      "https://idp.example/realms/r",
		"NBSANDBOX_OIDC_AUDIENCE":                    "nbsandbox",
		"NBSANDBOX_OIDC_ID_PREFIX":                   "kc",
		"NBSANDBOX_OIDC_ID_CLAIMS":                   "sub",
		"NBSANDBOX_OIDC_INTROSPECTION_CLIENT_ID":     "nbsandbox",
		"NBSANDBOX_OIDC_INTROSPECTION_CLIENT_SECRET": "s3cret",
		"NBSANDBOX_OIDC_INTROSPECTION_CACHE":         "5s",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if f, r := s.Auth.LoginLimit.Allowance(); f != 8 || r != time.Minute {
		t.Errorf("login limit %d per %s", f, r)
	}
	if len(s.Auth.TrustedProxies) != 2 || !s.Auth.ByAddress() {
		t.Errorf("trusted proxies %v (by address: %v, want on with proxies)", s.Auth.TrustedProxies, s.Auth.ByAddress())
	}
	if Default().Auth.ByAddress() {
		t.Error("by address must be off without trusted proxies")
	}
	in := s.Auth.OIDC.Introspection
	if in == nil || in.ClientID != "nbsandbox" || in.ClientSecret != "s3cret" || in.Cache != 5*time.Second {
		t.Errorf("introspection %+v", in)
	}
	if f, _ := Default().Auth.LoginLimit.Allowance(); f != 5 {
		t.Errorf("default allowance %d, want 5", f)
	}
	for name, env := range map[string]map[string]string{
		"bad proxy":            {"NBSANDBOX_TRUSTED_PROXIES": "not an address!"},
		"negative limit":       {"NBSANDBOX_LOGIN_LIMIT_FAILURES": "-1"},
		"introspection, no id": {"NBSANDBOX_OIDC_ISSUER": "https://idp.example", "NBSANDBOX_OIDC_AUDIENCE": "a", "NBSANDBOX_OIDC_ID_PREFIX": "kc", "NBSANDBOX_OIDC_ID_CLAIMS": "sub", "NBSANDBOX_OIDC_INTROSPECTION_CLIENT_SECRET": "x"},
	} {
		if _, err := Load("", envOf(env)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestPackagesSettings(t *testing.T) {
	s, err := Load("", envOf(map[string]string{"NBSANDBOX_PACKAGES_ENABLED": "true", "NBSANDBOX_PACKAGES_ALLOWED": "requests, scikit-learn",
		"NBSANDBOX_PACKAGES_INDEX_URL": "https://mirror.example/simple", "NBSANDBOX_PACKAGES_TIMEOUT": "5m", "NBSANDBOX_PACKAGES_NETWORK": "builds"}))
	if err != nil || !s.Packages.Enabled || len(s.Packages.Allowed) != 2 || s.Packages.IndexURL != "https://mirror.example/simple" ||
		s.Packages.BuildTimeout != 5*time.Minute || s.Packages.Network != "builds" {
		t.Fatalf("env: %+v %v", s.Packages, err)
	}
	for name, env := range map[string]map[string]string{
		"kubernetes":    {"NBSANDBOX_BACKEND": "kubernetes", "NBSANDBOX_K8S_NAMESPACE": "runs"},
		"plain http":    {"NBSANDBOX_PACKAGES_INDEX_URL": "http://mirror.example/simple"},
		"not a name":    {"NBSANDBOX_PACKAGES_ALLOWED": "requests==2"},
		"a short build": {"NBSANDBOX_PACKAGES_TIMEOUT": "5s"},
		"no builder":    {"NBSANDBOX_PACKAGES_BUILDER_IMAGE": " "},
	} {
		env["NBSANDBOX_PACKAGES_ENABLED"] = "true"
		if _, err := Load("", envOf(env)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestSessionsSettings(t *testing.T) {
	d := Default().Sessions
	if !d.Enabled || d.Idle != 30*time.Minute || d.Max != 8*time.Hour || d.PerUser != 2 || d.PerWorker != 4 {
		t.Fatalf("defaults: %+v", d)
	}
	s, err := Load("", envOf(map[string]string{"NBSANDBOX_SESSIONS_IDLE": "10m", "NBSANDBOX_SESSIONS_MAX": "2h",
		"NBSANDBOX_SESSIONS_PER_USER": "0", "NBSANDBOX_SESSIONS_PER_WORKER": "8"}))
	if err != nil || s.Sessions.Idle != 10*time.Minute || s.Sessions.Max != 2*time.Hour || s.Sessions.PerUser != 0 || s.Sessions.PerWorker != 8 {
		t.Fatalf("env: %+v %v", s.Sessions, err)
	}
	if s, err := Load("", envOf(map[string]string{"NBSANDBOX_SESSIONS_ENABLED": "false", "NBSANDBOX_SESSIONS_PER_WORKER": "0"})); err != nil || s.Sessions.Enabled {
		t.Fatalf("off: settings aren't checked: %v", err)
	}
	for name, env := range map[string]map[string]string{
		"short idle":        {"NBSANDBOX_SESSIONS_IDLE": "1s"},
		"max below idle":    {"NBSANDBOX_SESSIONS_IDLE": "1h", "NBSANDBOX_SESSIONS_MAX": "30m"},
		"a week and more":   {"NBSANDBOX_SESSIONS_MAX": "200h"},
		"no worker room":    {"NBSANDBOX_SESSIONS_PER_WORKER": "0"},
		"negative per user": {"NBSANDBOX_SESSIONS_PER_USER": "-1"},
		"not a number":      {"NBSANDBOX_SESSIONS_PER_USER": "two"},
		"a short secret":    {"NBSANDBOX_ROLE": "worker", "NBSANDBOX_SESSIONS_SECRET": "short"},
		"no worker port":    {"NBSANDBOX_ROLE": "worker", "NBSANDBOX_SESSIONS_SECRET": "0123456789abcdef", "NBSANDBOX_SESSIONS_LISTEN": "nope"},
	} {
		if _, err := Load("", envOf(env)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestSessionsInACluster(t *testing.T) {
	base := map[string]string{"NBSANDBOX_DATABASE_URL": "postgres://u:p@db:5432/x"}
	with := func(kv ...string) map[string]string {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	s, err := Load("", envOf(with("NBSANDBOX_ROLE", "worker")))
	if err != nil || s.SessionsOn() {
		t.Fatalf("a worker without the secret starts, with sessions off: %v %v", err, s.SessionsOn())
	}
	s, err = Load("", envOf(with("NBSANDBOX_ROLE", "worker", "NBSANDBOX_SESSIONS_SECRET", "0123456789abcdef")))
	if err != nil || !s.SessionsOn() || s.Sessions.SessionAddress("w1") != "w1:8091" {
		t.Fatalf("worker: %v %v %q", err, s.SessionsOn(), s.Sessions.SessionAddress("w1"))
	}
	s, _ = Load("", envOf(with("NBSANDBOX_SESSIONS_ADDRESS", "10.0.0.5:9000")))
	if !s.SessionsOn() || s.Sessions.SessionAddress("w1") != "10.0.0.5:9000" {
		t.Fatalf("one process: %v %q", s.SessionsOn(), s.Sessions.SessionAddress("w1"))
	}
}

func TestConnectionsSettings(t *testing.T) {
	d := Default().Connections
	if d.Enabled || d.Secrets.Provider != SecretsBuiltin || len(d.Allowed) != 0 {
		t.Fatalf("defaults: %+v", d)
	}
	key := "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=" // 32 bytes
	s, err := Load("", envOf(map[string]string{"NBSANDBOX_CONNECTIONS_ENABLED": "true", "NBSANDBOX_CONNECTIONS_MASTER_KEY": key,
		"NBSANDBOX_GATEWAY_KEY": key, "NBSANDBOX_GATEWAY_ADDRESS": "gw:3128", "NBSANDBOX_FETCHER_IMAGE": "f:1",
		"NBSANDBOX_CONNECTIONS_ALLOWED": "*.internal, 10.0.0.0/8,db.corp.example"}))
	if err != nil || !s.Connections.Enabled || len(s.Connections.Allowed) != 3 || s.Connections.Allowed[1] != "10.0.0.0/8" ||
		s.Connections.Gateway.Address != "gw:3128" || s.Connections.FetcherImage != "f:1" {
		t.Fatalf("env: %+v %v", s.Connections, err)
	}
	if s, err := Load("", envOf(map[string]string{"NBSANDBOX_CONNECTIONS_ALLOWED": "not valid"})); err != nil || s.Connections.Enabled {
		t.Fatalf("off: settings aren't checked: %v", err)
	}
	on := map[string]string{"NBSANDBOX_CONNECTIONS_ENABLED": "true", "NBSANDBOX_CONNECTIONS_MASTER_KEY": key, "NBSANDBOX_GATEWAY_KEY": key}
	for name, extra := range map[string]map[string]string{
		"no master key":                  {"NBSANDBOX_CONNECTIONS_MASTER_KEY": ""},
		"a short master key":             {"NBSANDBOX_CONNECTIONS_MASTER_KEY": "c2hvcnQ="},
		"an unknown provider":            {"NBSANDBOX_CONNECTIONS_PROVIDER": "vault"},
		"kubernetes without its backend": {"NBSANDBOX_CONNECTIONS_PROVIDER": "kubernetes"},
		"a bad CIDR":                     {"NBSANDBOX_CONNECTIONS_ALLOWED": "10.0.0.0/99"},
		"a wildcard in the middle":       {"NBSANDBOX_CONNECTIONS_ALLOWED": "db.*.example"},
		"a URL instead of a host":        {"NBSANDBOX_CONNECTIONS_ALLOWED": "https://db.example"},
		"no gateway key":                 {"NBSANDBOX_GATEWAY_KEY": ""},
		"a short gateway key":            {"NBSANDBOX_GATEWAY_KEY": "c2hvcnQ="},
		"a gateway without a port":       {"NBSANDBOX_GATEWAY_ADDRESS": "gw"},
	} {
		env := map[string]string{}
		for k, v := range on {
			env[k] = v
		}
		for k, v := range extra {
			env[k] = v
		}
		if _, err := Load("", envOf(env)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
