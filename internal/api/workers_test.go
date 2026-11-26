// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/store"
)

// Any signed-in user sees the workers: counts, no one's runs.
func TestListWorkers(t *testing.T) {
	e := newEnv(t)
	if err := e.db.PutWorker(context.Background(), store.Worker{ID: "w1", Up: true, Role: "worker", Version: "0.4.0",
		Runtime: `{"runtime":"runc","limits":{"memory_bytes":1024,"cpus":1}}`, StartedAt: time.Now(), RunSlots: 2, SessionSlots: 4}); err != nil {
		t.Fatal(err)
	}
	rec := e.req("GET", "/v1/workers", "bob", nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var got struct {
		Items []struct {
			ID, State, Runtime string
			Runs               struct{ Active, Capacity int }
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got.Items) != 1 || got.Items[0].ID != "w1" ||
		got.Items[0].State != "up" || got.Items[0].Runtime != "runc" || got.Items[0].Runs.Capacity != 2 {
		t.Fatalf("%s %v", rec.Body, err)
	}
	if rec := e.req("GET", "/v1/workers", "", nil, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("without credentials: %d", rec.Code)
	}
}
