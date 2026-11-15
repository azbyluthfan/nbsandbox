// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEnvIdentity(t *testing.T) {
	t.Setenv("NBSANDBOX_TOKEN", "tok")
	if id, err := envIdentity(); err != nil || id.Token != "tok" {
		t.Errorf("%+v %v", id, err)
	}
	t.Setenv("NBSANDBOX_TOKEN", "")
	t.Setenv("NBSANDBOX_USER", "alice")
	t.Setenv("NBSANDBOX_PASSWORD", "pw")
	if id, err := envIdentity(); err != nil || id.Name != "alice" || id.Password != "pw" {
		t.Errorf("%+v %v", id, err)
	}
	t.Setenv("NBSANDBOX_TOKEN", "tok") // both: ambiguous
	if _, err := envIdentity(); err == nil {
		t.Error("a token and a password together must be refused")
	}
}

func TestPrivateHosts(t *testing.T) {
	for h, want := range map[string]bool{"localhost": true, "127.0.0.1": true, "10.1.2.3": true, "nbsandbox-api": true,
		"api.nbsandbox.svc": true, "api.nbsandbox.svc.cluster.local": true, "nbsandbox.example.com": false, "8.8.8.8": false} {
		if privateHost(h) != want {
			t.Errorf("%s: %v", h, !want)
		}
	}
}

func TestRequireAuth(t *testing.T) {
	h := requireAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/mcp", nil))
	if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") == "" {
		t.Errorf("without credentials: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/mcp", nil)
	req.Header.Set("Authorization", "Bearer x")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTeapot {
		t.Errorf("with credentials: %d", rec.Code)
	}
}

func TestRunRefusesBadSettings(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	for name, env := range map[string]map[string]string{
		"no url":             {"NBSANDBOX_URL": ""},
		"plain http outside": {"NBSANDBOX_URL": "http://nbsandbox.example.com"},
		"no credentials":     {"NBSANDBOX_URL": "https://nbsandbox.example.com"},
	} {
		t.Run(name, func(t *testing.T) {
			for k, v := range env {
				t.Setenv(k, v)
			}
			t.Setenv("NBSANDBOX_TOKEN", "")
			t.Setenv("NBSANDBOX_PASSWORD", "")
			if err := run("", log); err == nil {
				t.Error("accepted")
			}
		})
	}
	t.Setenv("NBSANDBOX_URL", "https://nbsandbox.example.com")
	t.Setenv("NBSANDBOX_TOKEN", "tok")
	if err := run("127.0.0.1:0", log); err == nil {
		t.Error("HTTP mode with a server-wide token must be refused")
	}
}
