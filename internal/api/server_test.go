// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/azbyluthfan/nbsandbox/internal/auth"
	"github.com/azbyluthfan/nbsandbox/internal/auth/authtest"
	"github.com/azbyluthfan/nbsandbox/internal/blob"
	"github.com/azbyluthfan/nbsandbox/internal/isolation"
	"github.com/azbyluthfan/nbsandbox/internal/obs"
	"github.com/azbyluthfan/nbsandbox/internal/runs"
	"github.com/azbyluthfan/nbsandbox/internal/spaces"
	"github.com/azbyluthfan/nbsandbox/internal/store/storetest"
)

type testEnv struct {
	handler  http.Handler
	logs     *bytes.Buffer
	metrics  *obs.Metrics
	ready    *bool
	spaces   *spaces.Service
	deps     Deps
	runs     *runs.Manager
	backend  *fakeBackend
	shutdown chan struct{}
}

// Small limits so the upload-limit tests stay fast: 1 KiB per file, 4 KiB per space.
const (
	testMaxFile  = 1 << 10
	testMaxSpace = 4 << 10
)

func newEnv(t *testing.T) testEnv {
	t.Helper()
	return newEnvWithIDP(t, nil)
}

// newEnvWithIDP also accepts bearer tokens from idp (when not nil).
func newEnvWithIDP(t *testing.T, idp *authtest.IDP) testEnv {
	t.Helper()
	var users strings.Builder
	users.WriteString("users:\n")
	for _, u := range []string{"alice", "bob", "root"} {
		h, _ := bcrypt.GenerateFromPassword([]byte(u+"-password"), bcrypt.MinCost)
		fmt.Fprintf(&users, "  - username: %s\n    password_hash: %q\n    admin: %t\n", u, h, u == "root")
	}
	us, err := auth.ParseUsers([]byte(users.String()))
	if err != nil {
		t.Fatal(err)
	}
	ready := true
	health := obs.NewHealth(time.Second, obs.CheckFunc{CheckName: "docker", Fn: func(context.Context) error {
		if !ready {
			return errors.New("docker unreachable")
		}
		return nil
	}})
	data := t.TempDir()
	db := storetest.Open(t)
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	svc, err := spaces.New(spaces.Config{DataDir: data, MaxFile: testMaxFile, MaxSpace: testMaxSpace}, db, blob.NewMemory(), logger)
	if err != nil {
		t.Fatal(err)
	}
	m := obs.NewMetrics()
	backend := &fakeBackend{}
	mgr := runs.New(runs.Config{Image: "nbsandbox-runner:test", Concurrency: 2, Timeout: 10 * time.Second, LogCap: 1 << 20,
		Retention: 24 * time.Hour, DependencyCheck: "warn",
		Limits: isolation.Limits{MemoryBytes: 2 << 30, NanoCPUs: 1e9, PIDs: 256, TmpfsBytes: 256 << 20, StopGrace: 50 * time.Millisecond}},
		runs.Deps{DB: db, Spaces: svc, Backend: backend, Metrics: m, Log: logger})
	if err := mgr.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	mgr.SetDockerUp(true)
	t.Cleanup(func() { mgr.Shutdown(0) })
	shutdown := make(chan struct{})
	authn := &auth.Authenticator{Users: us}
	if idp != nil {
		authn.OIDC = idp.Verifier(t)
	}
	deps := Deps{Auth: authn, Users: us, Health: health, Metrics: m, Logger: logger, Spaces: svc, MaxUpload: testMaxSpace + 64<<10,
		Runs: mgr, Shutdown: shutdown, StreamHeartbeat: 50 * time.Millisecond, Runtime: func() (RuntimeInfo, error) {
			var ri RuntimeInfo
			ri.Image.Ref, ri.PythonVersion, ri.Packages, ri.Runtime = "nbsandbox-runner:test", "3.13.18", map[string]string{"pandas": "3.1.1"}, "runc"
			return ri, nil
		}}
	return testEnv{handler: checkSpec(t, NewHandler(deps), nil), logs: &logs, metrics: m, ready: &ready, spaces: svc, deps: deps, runs: mgr, backend: backend, shutdown: shutdown}
}

func (e testEnv) do(method, path string, body io.Reader, user, pass string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, body)
	if user != "" {
		req.SetBasicAuth(user, pass)
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

func decodeError(t *testing.T, rec *httptest.ResponseRecorder) ErrorDetail {
	t.Helper()
	var b ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatalf("not a JSON error body: %q", rec.Body.String())
	}
	return b.Error
}

// No or wrong credentials → 401.
func TestV1RequiresAuth(t *testing.T) {
	e := newEnv(t)
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"none":  e.do(http.MethodGet, "/v1/spaces", nil, "", ""),
		"wrong": e.do(http.MethodGet, "/v1/spaces", nil, "alice", "nope"),
	} {
		if rec.Code != http.StatusUnauthorized || decodeError(t, rec).Code != CodeUnauthorized {
			t.Errorf("%s: code %d body %s", name, rec.Code, rec.Body)
		}
	}
	// Authenticated, unknown endpoint → JSON 404 (unauthenticated it is 401,
	// so unknown paths reveal nothing).
	if rec := e.do(http.MethodGet, "/v1/nope", nil, "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown path without auth: %d", rec.Code)
	}
	rec := e.do(http.MethodGet, "/v1/nope", nil, "alice", "alice-password")
	if rec.Code != http.StatusNotFound || decodeError(t, rec).Code != CodeNotFound {
		t.Fatalf("authenticated: code %d body %s", rec.Code, rec.Body)
	}
}

func TestOperationalEndpointsNeedNoAuth(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{"/healthz", "/readyz", "/metrics"} {
		if rec := e.do(http.MethodGet, p, nil, "", ""); rec.Code != http.StatusOK {
			t.Errorf("%s: code %d", p, rec.Code)
		}
	}
}

// Readiness follows the docker check; liveness doesn't.
func TestReadinessFollowsChecks(t *testing.T) {
	e := newEnv(t)
	*e.ready = false
	if rec := e.do(http.MethodGet, "/readyz", nil, "", ""); rec.Code != http.StatusServiceUnavailable ||
		!strings.Contains(rec.Body.String(), "docker unreachable") {
		t.Fatalf("readyz while down: %d %s", rec.Code, rec.Body)
	}
	if rec := e.do(http.MethodGet, "/healthz", nil, "", ""); rec.Code != http.StatusOK {
		t.Fatalf("healthz while down: %d", rec.Code)
	}
	*e.ready = true
	if rec := e.do(http.MethodGet, "/readyz", nil, "", ""); rec.Code != http.StatusOK {
		t.Fatalf("readyz after recovery: %d", rec.Code)
	}
}

func TestUnknownPathIsJSON404(t *testing.T) {
	rec := newEnv(t).do(http.MethodGet, "/nope", nil, "", "")
	if rec.Code != http.StatusNotFound || decodeError(t, rec).Code != CodeNotFound {
		t.Fatalf("code %d body %s", rec.Code, rec.Body)
	}
}

func TestRequestIDAndAccessLog(t *testing.T) {
	e := newEnv(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/nope", nil)
	req.SetBasicAuth("alice", "alice-password")
	req.Header.Set("X-Request-ID", "test-123")
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)

	if rec.Header().Get("X-Request-ID") != "test-123" {
		t.Fatalf("request id not echoed: %q", rec.Header().Get("X-Request-ID"))
	}
	logs := e.logs.String()
	for _, want := range []string{`"request_id":"test-123"`, `"user":"alice"`, `"status":404`, `"path":"/v1/nope"`} {
		if !strings.Contains(logs, want) {
			t.Errorf("access log missing %s: %s", want, logs)
		}
	}
	if strings.Contains(logs, "alice-password") || strings.Contains(logs, "Basic ") {
		t.Fatalf("access log leaked credentials: %s", logs)
	}

	// A generated ID when the client sends none.
	if id := e.do(http.MethodGet, "/healthz", nil, "", "").Header().Get("X-Request-ID"); len(id) != 16 {
		t.Fatalf("generated request id %q", id)
	}
}

func TestRequestMetricUsesRoutePattern(t *testing.T) {
	e := newEnv(t)
	e.do(http.MethodGet, "/healthz", nil, "", "")
	rec := e.do(http.MethodGet, "/metrics", nil, "", "")
	if !strings.Contains(rec.Body.String(), `nbsandbox_http_requests_total{code="200",route="GET /healthz"} 1`) {
		t.Fatalf("metric not labelled by pattern:\n%s", grep(rec.Body.String(), "nbsandbox_http_requests_total"))
	}
}

func TestPanicBecomes500(t *testing.T) {
	var logs bytes.Buffer
	h := recoverPanics(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req = req.WithContext(obs.WithLogger(req.Context(), slog.New(slog.NewJSONHandler(&logs, nil))))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError || decodeError(t, rec).Code != CodeInternal {
		t.Fatalf("code %d body %s", rec.Code, rec.Body)
	}
	if !strings.Contains(logs.String(), "boom") {
		t.Fatal("panic not logged")
	}
}

func TestBodyLimit(t *testing.T) {
	h := limitBody(DefaultMaxBody, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				WriteError(w, http.StatusRequestEntityTooLarge, CodePayloadTooLarge, "too large")
				return
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(strings.Repeat("x", DefaultMaxBody+1))))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("code %d", rec.Code)
	}
}

func grep(s, sub string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, sub) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}
