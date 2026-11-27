// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package grpcapi

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	pb "github.com/azbyluthfan/nbsandbox/gen/nbsandbox/v1"
)

// Shared spaces over gRPC: members, roles, and a role too low.
func TestMembersRPC(t *testing.T) {
	ctx := context.Background()
	as := newServer(t)
	alice, bob := as("alice"), as("bob")
	sp, err := alice.spaces.CreateSpace(ctx, connect.NewRequest(&pb.CreateSpaceRequest{Name: "team"}))
	if err != nil {
		t.Fatal(err)
	}
	sid := sp.Msg.Id
	if _, err := bob.spaces.ListMembers(ctx, connect.NewRequest(&pb.ListMembersRequest{SpaceId: sid})); code(err) != connect.CodeNotFound {
		t.Fatalf("bob without a role: %v", err)
	}
	m, err := alice.spaces.PutMember(ctx, connect.NewRequest(&pb.PutMemberRequest{SpaceId: sid, Principal: "bob", Role: "read"}))
	if err != nil || m.Msg.Role != "read" || m.Msg.GrantedBy != "alice" {
		t.Fatalf("grant: %v %v", m, err)
	}
	got, err := bob.spaces.GetSpace(ctx, connect.NewRequest(&pb.GetSpaceRequest{SpaceId: sid}))
	if err != nil || got.Msg.Role != "read" || got.Msg.Owner != "alice" {
		t.Fatalf("bob's view: %v %v", got, err)
	}
	if _, err := bob.spaces.UpdateSpace(ctx, connect.NewRequest(&pb.UpdateSpaceRequest{SpaceId: sid, Name: "x"})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("read renames: %v", err)
	}
	l, err := bob.spaces.ListSpaces(ctx, connect.NewRequest(&pb.ListSpacesRequest{Role: "read"}))
	if err != nil || len(l.Msg.Items) != 1 {
		t.Fatalf("bob's shared spaces: %v %v", l, err)
	}
	ms, err := bob.spaces.ListMembers(ctx, connect.NewRequest(&pb.ListMembersRequest{SpaceId: sid}))
	if err != nil || len(ms.Msg.Items) != 2 || !ms.Msg.Items[0].Creator {
		t.Fatalf("members: %v %v", ms, err)
	}
	if _, err := alice.spaces.TransferSpace(ctx, connect.NewRequest(&pb.TransferSpaceRequest{SpaceId: sid, To: "bob"})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("handed over to a reader: %v", err)
	}
	if _, err := bob.spaces.DeleteMember(ctx, connect.NewRequest(&pb.DeleteMemberRequest{SpaceId: sid, Principal: "bob"})); err != nil {
		t.Fatalf("bob leaves: %v", err)
	}
	if _, err := bob.spaces.GetSpace(ctx, connect.NewRequest(&pb.GetSpaceRequest{SpaceId: sid})); code(err) != connect.CodeNotFound {
		t.Fatalf("after leaving: %v", err)
	}
}
