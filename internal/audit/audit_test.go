// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/azbyluthfan/nbsandbox/internal/auth"
	"github.com/azbyluthfan/nbsandbox/internal/store"
)

type memStore struct {
	mu      sync.Mutex
	rows    []store.AuditEvent
	fail    bool
	deleted time.Time
}

func (m *memStore) InsertAudit(_ context.Context, e store.AuditEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail {
		return errors.New("database down")
	}
	m.rows = append(m.rows, e)
	return nil
}

func (m *memStore) ListAudit(context.Context, store.AuditFilter, string, int) ([]store.AuditEvent, string, error) {
	return m.rows, "", nil
}

func (m *memStore) DeleteAuditBefore(_ context.Context, t time.Time) (int64, error) {
	m.deleted = t
	return 0, nil
}

func TestAudited(t *testing.T) {
	r := New(Config{Enabled: true}, &memStore{}, nil, nil)
	for op, want := range map[auth.Op]bool{
		auth.OpCreateRun: true, auth.OpDeleteFile: true, auth.OpAdminListSpaces: true, auth.OpAdminListAudit: true,
		auth.OpGetFile:   false, // downloads off
		auth.OpListFiles: false, auth.OpFollowRunLog: false, auth.OpGetRun: false,
	} {
		if got := r.Audited(op); got != want {
			t.Errorf("%s: %t", op, got)
		}
	}
	if !New(Config{Enabled: true, Downloads: true}, &memStore{}, nil, nil).Audited(auth.OpGetArtifact) {
		t.Error("downloads on: GetArtifact")
	}
	if New(Config{Enabled: false, Downloads: true}, &memStore{}, nil, nil).Audited(auth.OpCreateRun) {
		t.Error("off: nothing is audited")
	}
	var none *Recorder
	none.Record(context.Background(), Entry{Action: "x"})
	none.Run(context.Background(), time.Hour)
	if none.Audited(auth.OpCreateRun) {
		t.Error("nil recorder")
	}
	if rows, _, err := none.List(context.Background(), store.AuditFilter{}, "", 10); rows != nil || err != nil {
		t.Error("nil recorder lists nothing")
	}
}

func TestRecord(t *testing.T) {
	db := &memStore{}
	failures := prometheus.NewCounter(prometheus.CounterOpts{Name: "f"})
	r := New(Config{Enabled: true}, db, nil, failures)
	ctx := WithNote(WithVia(auth.WithClientAddr(auth.WithUser(context.Background(), "alice"), "198.51.100.7"), "ui"))
	SetTarget(ctx, "run-1")
	SetDetail(ctx, "notebook a.ipynb")
	SetSpace(ctx, "s1")
	r.Record(ctx, Entry{Action: "CreateRun", Status: 201})
	// The path's target wins over the note's; the given actor over ctx's.
	r.Record(ctx, Entry{Action: ActionSignOut, Actor: "kc:1", Method: "oidc", SpaceID: "s2", Target: "from-path", Status: 200})
	if len(db.rows) != 2 {
		t.Fatalf("%d rows", len(db.rows))
	}
	a, b := db.rows[0], db.rows[1]
	if a.Actor != "alice" || a.Method != "basic" || a.Via != "ui" || a.Address != "198.51.100.7" || a.SpaceID != "s1" ||
		a.Target != "run-1" || a.Detail != "notebook a.ipynb" || a.Status != 201 || a.At.IsZero() {
		t.Errorf("row: %+v", a)
	}
	if b.Actor != "kc:1" || b.Method != "oidc" || b.SpaceID != "s2" || b.Target != "from-path" {
		t.Errorf("row with overrides: %+v", b)
	}
	if ViaOf(context.Background()) != "rest" {
		t.Error("default via")
	}
	SetTarget(context.Background(), "ignored") // no note: nothing happens

	// A row that can't be written doesn't fail anything, and is counted.
	db.fail = true
	r.Record(ctx, Entry{Action: "DeleteRun", Status: 204})
	if n := testutil.ToFloat64(failures); n != 1 {
		t.Errorf("failures: %v", n)
	}
	// Off: nothing written.
	db.fail = false
	New(Config{Enabled: false}, db, nil, nil).Record(ctx, Entry{Action: "DeleteRun"})
	if len(db.rows) != 2 {
		t.Error("recorded while off")
	}
}

func TestRetention(t *testing.T) {
	db := &memStore{}
	r := New(Config{Enabled: true, Retention: 24 * time.Hour}, db, nil, nil)
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.Run(ctx, time.Hour) // one pass, then stops
	if !db.deleted.Equal(now.Add(-24 * time.Hour)) {
		t.Errorf("deleted before %v", db.deleted)
	}
}
