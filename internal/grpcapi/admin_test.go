// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package grpcapi

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	pb "github.com/azbyluthfan/nbsandbox/gen/nbsandbox/v1"
)

func TestAdminService(t *testing.T) {
	ctx := context.Background()
	as := newServer(t)
	alice, bob, root := as("alice"), as("bob"), as("root")
	sp, err := alice.spaces.CreateSpace(ctx, connect.NewRequest(&pb.CreateSpaceRequest{Name: "a"}))
	if err != nil {
		t.Fatal(err)
	}
	id := sp.Msg.Id
	if _, err := alice.admin.AdminListSpaces(ctx, connect.NewRequest(&pb.AdminListSpacesRequest{})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("alice lists every space: %v", err)
	}
	l, err := root.admin.AdminListSpaces(ctx, connect.NewRequest(&pb.AdminListSpacesRequest{Owner: "alice", Limit: 10}))
	if err != nil || len(l.Msg.Items) != 1 || l.Msg.Items[0].Owner != "alice" {
		t.Fatalf("list %v %v", l, err)
	}
	if _, err := root.admin.AdminListSpaces(ctx, connect.NewRequest(&pb.AdminListSpacesRequest{Cursor: "garbage"})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("bad cursor: %v", err)
	}
	got, err := root.admin.AdminUpdateSpace(ctx, connect.NewRequest(&pb.AdminUpdateSpaceRequest{SpaceId: id, Owner: "bob"}))
	if err != nil || got.Msg.Owner != "bob" || got.Msg.Usage == nil {
		t.Fatalf("update %v %v", got, err)
	}
	if _, err := bob.spaces.GetSpace(ctx, connect.NewRequest(&pb.GetSpaceRequest{SpaceId: id})); err != nil {
		t.Fatalf("bob can't see it: %v", err)
	}
	if _, err := root.admin.AdminUpdateSpace(ctx, connect.NewRequest(&pb.AdminUpdateSpaceRequest{SpaceId: "nope", Owner: "bob"})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("bad id: %v", err)
	}
	if _, err := root.admin.AdminUpdateSpace(ctx, connect.NewRequest(&pb.AdminUpdateSpaceRequest{SpaceId: id})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("empty owner: %v", err)
	}
	if _, err := root.admin.AdminDeleteSpace(ctx, connect.NewRequest(&pb.AdminDeleteSpaceRequest{SpaceId: "nope"})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("bad id: %v", err)
	}
	if _, err := root.admin.AdminDeleteSpace(ctx, connect.NewRequest(&pb.AdminDeleteSpaceRequest{SpaceId: id})); err != nil {
		t.Fatal(err)
	}
	if _, err := root.admin.AdminDeleteSpace(ctx, connect.NewRequest(&pb.AdminDeleteSpaceRequest{SpaceId: id})); code(err) != connect.CodeNotFound {
		t.Fatalf("delete twice: %v", err)
	}
}
