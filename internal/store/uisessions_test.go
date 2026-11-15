// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestUILoginsAreUsedOnce(t *testing.T) {
	db, ctx := openTest(t), context.Background()
	now := time.UnixMilli(time.Now().UnixMilli())
	l := UILogin{StateHash: []byte("state-1"), Verifier: "v", Nonce: "n", ReturnTo: "/x", ExpiresAt: now.Add(10 * time.Minute)}
	if err := db.CreateUILogin(ctx, l); err != nil {
		t.Fatal(err)
	}
	got, err := db.TakeUILogin(ctx, l.StateHash, now)
	if err != nil || got.Verifier != "v" || got.Nonce != "n" || got.ReturnTo != "/x" || !got.ExpiresAt.Equal(l.ExpiresAt) {
		t.Fatalf("take: %+v, %v", got, err)
	}
	if _, err := db.TakeUILogin(ctx, l.StateHash, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second take: %v", err)
	}
	l.StateHash = []byte("state-2")
	_ = db.CreateUILogin(ctx, l)
	if _, err := db.TakeUILogin(ctx, l.StateHash, now.Add(11*time.Minute)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired: %v", err)
	}
}

func TestUISessions(t *testing.T) {
	db, ctx := openTest(t), context.Background()
	now := time.UnixMilli(time.Now().UnixMilli())
	s := UISession{IDHash: []byte("s1"), Method: "oidc", Principal: "kc:1", DisplayName: "Alice", Tokens: []byte{1, 2},
		AccessExpires: now.Add(5 * time.Minute), CreatedAt: now, LastSeenAt: now}
	if err := db.CreateUISession(ctx, s); err != nil {
		t.Fatal(err)
	}
	b := UISession{IDHash: []byte("s2"), Method: "basic", Principal: "alice", DisplayName: "alice", Tokens: []byte{},
		CreatedAt: now.Add(-13 * time.Hour), LastSeenAt: now}
	if err := db.CreateUISession(ctx, b); err != nil {
		t.Fatal(err)
	}
	got, err := db.UISession(ctx, s.IDHash)
	if err != nil || got.Principal != "kc:1" || got.DisplayName != "Alice" || !got.AccessExpires.Equal(s.AccessExpires) || len(got.Tokens) != 2 {
		t.Fatalf("get: %+v, %v", got, err)
	}
	if got, _ := db.UISession(ctx, b.IDHash); !got.AccessExpires.IsZero() {
		t.Errorf("basic session's access expiry: %v", got.AccessExpires)
	}
	later := now.Add(time.Minute)
	if err := db.TouchUISession(ctx, s.IDHash, later); err != nil {
		t.Fatal(err)
	}
	_ = db.TouchUISession(ctx, s.IDHash, now) // never goes back
	if err := db.UpdateUISessionTokens(ctx, s.IDHash, []byte{3}, now.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	got, _ = db.UISession(ctx, s.IDHash)
	if !got.LastSeenAt.Equal(later) || got.Tokens[0] != 3 || !got.AccessExpires.Equal(now.Add(10*time.Minute)) {
		t.Fatalf("after touch and refresh: %+v", got)
	}
	// b was created 13 h ago: past a 12 h maximum.
	n, err := db.DeleteExpiredUI(ctx, now, now.Add(-time.Hour), now.Add(-12*time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("expired: %d, %v", n, err)
	}
	if _, err := db.UISession(ctx, b.IDHash); !errors.Is(err, ErrNotFound) {
		t.Errorf("b kept: %v", err)
	}
	if err := db.DeleteUISession(ctx, s.IDHash); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UISession(ctx, s.IDHash); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleted session found: %v", err)
	}
}
