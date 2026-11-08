// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package obs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLoggerRedactsSensitiveAttributes(t *testing.T) {
	var buf bytes.Buffer
	l, err := NewLogger(&buf, "info")
	if err != nil {
		t.Fatal(err)
	}
	l.Info("request", "authorization", "Basic YWxpY2U6c2VjcmV0", "Password", "hunter2",
		"parameters", map[string]any{"token": "abc"}, "run_id", "r1")
	out := buf.String()
	for _, secret := range []string{"YWxpY2U6c2VjcmV0", "hunter2", "abc"} {
		if strings.Contains(out, secret) {
			t.Fatalf("log leaked %q: %s", secret, out)
		}
	}
	if !strings.Contains(out, `"run_id":"r1"`) {
		t.Fatalf("non-sensitive attribute missing: %s", out)
	}
}

func TestLoggerLevel(t *testing.T) {
	var buf bytes.Buffer
	l, err := NewLogger(&buf, "warn")
	if err != nil {
		t.Fatal(err)
	}
	l.Info("hidden")
	l.Warn("shown")
	if strings.Contains(buf.String(), "hidden") || !strings.Contains(buf.String(), "shown") {
		t.Fatalf("level not applied: %s", buf.String())
	}
	if _, err := NewLogger(&buf, "loud"); err == nil {
		t.Fatal("invalid level must fail")
	}
}

func TestReadiness(t *testing.T) {
	ok := CheckFunc{"db", func(context.Context) error { return nil }}
	bad := CheckFunc{"docker", func(context.Context) error { return errors.New("docker unreachable") }}
	slow := CheckFunc{"slow", func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }}

	t.Run("all ok", func(t *testing.T) {
		rec := httptest.NewRecorder()
		NewHealth(time.Second, ok).Readiness(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("code %d", rec.Code)
		}
	})
	t.Run("one failing", func(t *testing.T) {
		rec := httptest.NewRecorder()
		NewHealth(time.Second, ok, bad).Readiness(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		var rep ReadyReport
		_ = json.Unmarshal(rec.Body.Bytes(), &rep)
		if rec.Code != http.StatusServiceUnavailable || rep.Status != "not_ready" ||
			rep.Checks["db"] != "ok" || rep.Checks["docker"] != "docker unreachable" {
			t.Fatalf("code %d report %+v", rec.Code, rep)
		}
	})
	t.Run("check timeout", func(t *testing.T) {
		rep := NewHealth(20*time.Millisecond, slow).Ready(context.Background())
		if rep.Status != "not_ready" || !strings.Contains(rep.Checks["slow"], "deadline") {
			t.Fatalf("report %+v", rep)
		}
	})
}

func TestLiveness(t *testing.T) {
	rec := httptest.NewRecorder()
	NewHealth(time.Second).Liveness(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code %d", rec.Code)
	}
}

func TestMetricsExposeDeclaredNames(t *testing.T) {
	m := NewMetrics()
	m.RunsTotal.WithLabelValues("succeeded").Inc()
	m.HTTPRequestsTotal.WithLabelValues("GET /healthz", "200").Inc()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	for _, name := range []string{
		"nbsandbox_runs_total", "nbsandbox_run_duration_seconds", "nbsandbox_queue_length",
		"nbsandbox_active_runs", "nbsandbox_log_streams_active", "nbsandbox_docker_up",
		"nbsandbox_http_requests_total", "go_goroutines",
	} {
		if !strings.Contains(body, name) {
			t.Errorf("metric %s missing", name)
		}
	}
}
