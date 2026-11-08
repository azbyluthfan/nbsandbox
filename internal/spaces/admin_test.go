// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package spaces

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The space cap, an owner's own limits, and the administrator's view.
func TestOwnerLimitsAndAdmin(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, 1<<10, 4<<10)
	e.svc.cfg.MaxSpaces = 2
	e.svc.cfg.Owners = map[string]OwnerLimits{"vip": {MaxSpaces: 3, MaxSpaceBytes: 8 << 10}}

	for range 2 {
		if _, err := e.svc.Create(ctx, "alice", ""); err != nil {
			t.Fatal(err)
		}
	}
	var le *LimitError
	if _, err := e.svc.Create(ctx, "alice", ""); !errors.As(err, &le) || !strings.Contains(le.Msg, "2 spaces") {
		t.Fatalf("third space: %v", err)
	}
	var vip Space
	for range 3 {
		sp, err := e.svc.Create(ctx, "vip", "")
		if err != nil {
			t.Fatal(err)
		}
		vip = sp
	}
	if vip.Usage.QuotaBytes != 8<<10 {
		t.Fatalf("vip quota %d", vip.Usage.QuotaBytes)
	}
	if u, err := e.svc.Usage(ctx, vip.ID); err != nil || u.QuotaBytes != 8<<10 {
		t.Fatalf("vip usage %+v %v", u, err)
	}
	// The bigger quota holds: 6 KiB fits for vip, not for alice.
	if _, err := e.svc.Upload(ctx, "vip", vip.ID, "", parts("a.txt", strings.Repeat("x", 1000), "b.txt", strings.Repeat("x", 1000),
		"c.txt", strings.Repeat("x", 1000), "d.txt", strings.Repeat("x", 1000), "e.txt", strings.Repeat("x", 1000))); err != nil {
		t.Fatalf("vip upload: %v", err)
	}

	all, next, err := e.svc.AdminList(ctx, "", "", 50)
	if err != nil || next != "" || len(all) != 5 {
		t.Fatalf("admin list %d %q %v", len(all), next, err)
	}
	for _, sp := range all {
		if sp.Owner != "alice" && sp.Owner != "vip" {
			t.Fatalf("owner %q", sp.Owner)
		}
	}
	if mine, _, _ := e.svc.AdminList(ctx, "alice", "", 50); len(mine) != 2 {
		t.Fatalf("alice's: %d", len(mine))
	}
	if _, _, err := e.svc.AdminList(ctx, "", "garbage", 50); err == nil {
		t.Fatal("a bad cursor was accepted")
	}

	sp, err := e.svc.AdminSetOwner(ctx, vip.ID, "bob")
	if err != nil || sp.Owner != "bob" || sp.Usage.FileCount != 5 {
		t.Fatalf("set owner %+v %v", sp, err)
	}
	if _, err := e.svc.Get(ctx, "vip", vip.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the old owner still sees it: %v", err)
	}
	if _, err := e.svc.Get(ctx, "bob", vip.ID); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", strings.Repeat("x", 513)} {
		if _, err := e.svc.AdminSetOwner(ctx, vip.ID, bad); err == nil {
			t.Fatalf("owner %q accepted", bad)
		}
	}
	if _, err := e.svc.AdminSetOwner(ctx, "0d0f8a8e-1111-4111-8111-111111111111", "bob"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing space: %v", err)
	}
	if err := e.svc.AdminDelete(ctx, vip.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Get(ctx, "bob", vip.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
	if err := e.svc.AdminDelete(ctx, vip.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleting twice: %v", err)
	}
}
