// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/azbyluthfan/nbsandbox/internal/blob"
	"github.com/azbyluthfan/nbsandbox/internal/store"
	"github.com/azbyluthfan/nbsandbox/internal/store/storetest"
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func writeUsers(t *testing.T, dir string, mode os.FileMode) string {
	t.Helper()
	h, _ := bcrypt.GenerateFromPassword([]byte("alice-password"), bcrypt.MinCost)
	p := filepath.Join(dir, "users.yaml")
	if err := os.WriteFile(p, []byte(fmt.Sprintf("users:\n  - username: alice\n    password_hash: %q\n", h)), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

// The whole service starts with Docker unreachable, serves
// health and auth, reports not-ready, and shuts down cleanly on cancel.
func TestServeStartsWithoutDockerAndShutsDown(t *testing.T) {
	dir := t.TempDir()
	env := map[string]string{
		"NBSANDBOX_DATA_DIR":     filepath.Join(dir, "data"),
		"NBSANDBOX_DATABASE_URL": storetest.URL(t),
		"NBSANDBOX_USERS_FILE":   writeUsers(t, dir, 0o600),
		"NBSANDBOX_LISTEN":       "127.0.0.1:0",
		"NBSANDBOX_DOCKER_HOST":  "unix:///nonexistent/nbsandbox-test.sock", // short: macOS limits socket paths to 104 bytes
	}
	logs := &syncBuffer{}
	addrCh := make(chan string, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, nil, serveOptions{
			getenv: func(k string) string { return env[k] }, logOut: logs, storage: blob.NewMemory(),
			onListen: func(a string) { addrCh <- a },
		})
	}()

	var base string
	select {
	case a := <-addrCh:
		base = "http://" + a
	case err := <-done:
		t.Fatalf("service exited early: %v\n%s", err, logs)
	case <-time.After(10 * time.Second):
		t.Fatalf("service did not start\n%s", logs)
	}

	get := func(path, user, pass string) (int, string) {
		req, _ := http.NewRequest(http.MethodGet, base+path, nil)
		if user != "" {
			req.SetBasicAuth(user, pass)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	if code, _ := get("/healthz", "", ""); code != http.StatusOK {
		t.Fatalf("healthz %d", code)
	}
	code, body := get("/readyz", "", "")
	var rep struct {
		Status string            `json:"status"`
		Checks map[string]string `json:"checks"`
	}
	_ = json.Unmarshal([]byte(body), &rep)
	if code != http.StatusServiceUnavailable || rep.Checks["db"] != "ok" || rep.Checks["data_dir"] != "ok" ||
		!strings.Contains(rep.Checks["docker"], "unreachable") {
		t.Fatalf("readyz without docker: %d %s", code, body)
	}
	if code, _ := get("/v1/spaces", "", ""); code != http.StatusUnauthorized {
		t.Fatalf("v1 without auth: %d", code)
	}
	// The spaces API is wired up and works without Docker.
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/spaces", strings.NewReader(`{"name":"smoke"}`))
	req.SetBasicAuth("alice", "alice-password")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create space: %d", resp.StatusCode)
	}
	if code, body := get("/v1/spaces", "alice", "alice-password"); code != http.StatusOK || !strings.Contains(body, `"name":"smoke"`) {
		t.Fatalf("list spaces: %d %s", code, body)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutdown: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("service did not shut down")
	}
	if !strings.Contains(logs.String(), "nbsandboxd stopped") {
		t.Fatalf("no clean-stop log line:\n%s", logs)
	}
	db, err := store.Open(context.Background(), env["NBSANDBOX_DATABASE_URL"])
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if v, err := db.SchemaVersion(context.Background()); err != nil || v < 1 {
		t.Fatalf("database not migrated: version %d, %v", v, err)
	}
	if strings.Contains(logs.String(), "postgres://") {
		t.Fatalf("the database URL (it carries a password) appeared in the logs:\n%s", logs)
	}
}

func TestServeFailsFast(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]struct {
		env  map[string]string
		want string
	}{
		"invalid config": {map[string]string{"NBSANDBOX_DATA_DIR": "relative"}, "data_dir"},
		"users file readable by others": {map[string]string{
			"NBSANDBOX_DATA_DIR": filepath.Join(dir, "d1"), "NBSANDBOX_USERS_FILE": writeUsers(t, t.TempDir(), 0o644),
			"NBSANDBOX_DATABASE_URL": storetest.URL(t),
		}, "chmod"},
		"missing users file": {map[string]string{
			"NBSANDBOX_DATA_DIR": filepath.Join(dir, "d2"), "NBSANDBOX_USERS_FILE": filepath.Join(dir, "nope.yaml"),
			"NBSANDBOX_DATABASE_URL": storetest.URL(t),
		}, "users file"},
		"database unreachable": {map[string]string{
			"NBSANDBOX_DATA_DIR": filepath.Join(dir, "d3"), "NBSANDBOX_USERS_FILE": writeUsers(t, t.TempDir(), 0o600),
			"NBSANDBOX_DATABASE_URL": "postgres://nobody@127.0.0.1:1/x?connect_timeout=2",
		}, "127.0.0.1:1"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			err := run(context.Background(), nil, serveOptions{getenv: func(k string) string { return c.env[k] }, logOut: io.Discard, storage: blob.NewMemory()})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want error mentioning %q", err, c.want)
			}
		})
	}
}

func TestHashPasswordFromPipe(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.WriteString("piped-password-123\n")
	_ = w.Close()
	var out bytes.Buffer
	if err := hashPassword(r, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	h := strings.TrimSpace(out.String())
	if bcrypt.CompareHashAndPassword([]byte(h), []byte("piped-password-123")) != nil {
		t.Fatalf("hash %q does not verify", h)
	}
}

func TestHealthCommand(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	defer ok.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) }))
	defer bad.Close()
	if err := health([]string{"-url", ok.URL}); err != nil {
		t.Fatal(err)
	}
	if err := health([]string{"-url", bad.URL}); err == nil {
		t.Fatal("503 must fail the healthcheck")
	}

}

// startServe runs the service with env until the test ends and returns its
// base URL.
func startServe(t *testing.T, env map[string]string) (string, *syncBuffer) {
	t.Helper()
	logs := &syncBuffer{}
	addrCh := make(chan string, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, nil, serveOptions{
			getenv: func(k string) string { return env[k] }, logOut: logs, storage: blob.NewMemory(),
			onListen: func(a string) { addrCh <- a },
		})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("shutdown: %v", err)
			}
		case <-time.After(20 * time.Second):
			t.Error("service did not shut down")
		}
	})
	select {
	case a := <-addrCh:
		return "http://" + a, logs
	case err := <-done:
		t.Fatalf("service exited early: %v\n%s", err, logs)
	case <-time.After(10 * time.Second):
		t.Fatalf("service did not start\n%s", logs)
	}
	return "", nil
}

func status(t *testing.T, url, user string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if user != "" {
		req.SetBasicAuth(user, user+"-password")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// An API node needs no Docker: it is ready without one, and refuses runs
// (503) until a worker has said it can take them. A worker serves only
// health and metrics.
func TestServeRoles(t *testing.T) {
	dir := t.TempDir()
	dbURL := storetest.URL(t)
	env := func(role, data string) map[string]string {
		return map[string]string{
			"NBSANDBOX_ROLE": role, "NBSANDBOX_WORKER_ID": role + "-1",
			"NBSANDBOX_DATA_DIR": filepath.Join(dir, data), "NBSANDBOX_DATABASE_URL": dbURL,
			"NBSANDBOX_USERS_FILE": writeUsers(t, dir, 0o600), "NBSANDBOX_LISTEN": "127.0.0.1:0",
			"NBSANDBOX_DOCKER_HOST": "unix:///nonexistent/nbsandbox-test.sock",
		}
	}
	apiURL, apiLogs := startServe(t, env("api", "api"))
	if code, body := status(t, apiURL+"/readyz", ""); code != http.StatusOK || strings.Contains(body, "docker") {
		t.Fatalf("api readyz: %d %s", code, body)
	}
	if code, _ := status(t, apiURL+"/v1/runtime", "alice"); code != http.StatusServiceUnavailable {
		t.Fatalf("api runtime without workers: %d", code)
	}
	workerURL, _ := startServe(t, env("worker", "worker"))
	if code, body := status(t, workerURL+"/readyz", ""); code != http.StatusServiceUnavailable || !strings.Contains(body, "docker") {
		t.Fatalf("worker readyz without docker: %d %s", code, body)
	}
	if code, _ := status(t, workerURL+"/metrics", ""); code != http.StatusOK {
		t.Fatalf("worker metrics: %d", code)
	}
	if code, _ := status(t, workerURL+"/v1/spaces", "alice"); code != http.StatusNotFound {
		t.Fatalf("worker serves the API: %d", code)
	}
	// The worker is known, but can't run anything without Docker.
	db, err := store.Open(context.Background(), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	deadline := time.Now().Add(15 * time.Second)
	for {
		ws, _ := db.LiveWorkers(context.Background(), time.Minute)
		if len(ws) == 1 && ws[0].ID == "worker-1" && !ws[0].Up {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("worker not published: %+v", ws)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !strings.Contains(apiLogs.String(), `"role":"api"`) {
		t.Fatalf("role not logged:\n%s", apiLogs)
	}
}
