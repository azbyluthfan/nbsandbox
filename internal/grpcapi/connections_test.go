// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package grpcapi

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"

	pb "github.com/azbyluthfan/nbsandbox/gen/nbsandbox/v1"
)

func TestConnectionRPCs(t *testing.T) {
	ctx := context.Background()
	as := newServer(t)
	alice, bob := as("alice"), as("bob")
	settings, _ := structpb.NewStruct(map[string]any{"host": "pg.internal", "database": "crm", "user": "reader"})
	created, err := alice.conns.CreateConnection(ctx, connect.NewRequest(&pb.CreateConnectionRequest{Name: "crm-prod", Type: "postgresql",
		Settings: settings, Secret: &pb.ConnectionSecretInput{Kind: &pb.ConnectionSecretInput_Value{Value: "pg-SECRET"}}}))
	if err != nil {
		t.Fatal(err)
	}
	c := created.Msg
	if c.Secret.Mode != "managed" || c.Settings.AsMap()["port"] != float64(5432) {
		t.Fatalf("created: %v", c)
	}
	if _, err := alice.conns.CreateConnection(ctx, connect.NewRequest(&pb.CreateConnectionRequest{Name: "crm-prod", Type: "postgresql",
		Settings: settings, Secret: &pb.ConnectionSecretInput{Kind: &pb.ConnectionSecretInput_Value{Value: "x"}}})); code(err) != connect.CodeAlreadyExists {
		t.Fatalf("taken name: %v", err)
	}
	bad, _ := structpb.NewStruct(map[string]any{"host": "169.254.169.254", "database": "crm", "user": "reader"})
	if _, err := alice.conns.CreateConnection(ctx, connect.NewRequest(&pb.CreateConnectionRequest{Name: "meta", Type: "postgresql",
		Settings: bad, Secret: &pb.ConnectionSecretInput{Kind: &pb.ConnectionSecretInput_Value{Value: "x"}}})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("metadata address: %v", err)
	}

	sp, err := alice.spaces.CreateSpace(ctx, connect.NewRequest(&pb.CreateSpaceRequest{Name: "forecast"}))
	if err != nil {
		t.Fatal(err)
	}
	set, err := alice.conns.SetSpaceConnections(ctx, connect.NewRequest(&pb.SetSpaceConnectionsRequest{SpaceId: sp.Msg.Id,
		Environments: map[string]*pb.EnvironmentBindings{"prod": {Names: map[string]string{"crm": "crm-prod"}}}}))
	if err != nil || set.Msg.DefaultEnvironment != "prod" || set.Msg.Environments["prod"].Names["crm"].Id != c.Id {
		t.Fatalf("bind: %v %v", set, err)
	}
	if _, err := alice.conns.DeleteConnection(ctx, connect.NewRequest(&pb.GetConnectionRequest{ConnectionId: c.Id})); code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("delete while bound: %v", err)
	}
	got, err := alice.conns.GetConnection(ctx, connect.NewRequest(&pb.GetConnectionRequest{ConnectionId: c.Id}))
	if err != nil || len(got.Msg.UsedBy) != 1 {
		t.Fatalf("get: %v %v", got, err)
	}
	// Bob reaches none of it.
	if _, err := bob.conns.GetConnection(ctx, connect.NewRequest(&pb.GetConnectionRequest{ConnectionId: c.Id})); code(err) != connect.CodeNotFound {
		t.Fatalf("bob gets alice's: %v", err)
	}
	if _, err := bob.conns.GetSpaceConnections(ctx, connect.NewRequest(&pb.GetSpaceConnectionsRequest{SpaceId: sp.Msg.Id})); code(err) != connect.CodeNotFound {
		t.Fatalf("bob reads alice's space: %v", err)
	}
	if _, err := bob.conns.SetSpaceConnections(ctx, connect.NewRequest(&pb.SetSpaceConnectionsRequest{SpaceId: sp.Msg.Id})); code(err) != connect.CodeNotFound {
		t.Fatalf("bob changes alice's space: %v", err)
	}
	// No response carries the secret.
	list, err := alice.conns.ListConnections(ctx, connect.NewRequest(&pb.ListConnectionsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, msg := range []string{protojson.Format(list.Msg), protojson.Format(got.Msg), protojson.Format(c)} {
		if strings.Contains(msg, "SECRET") {
			t.Fatalf("a response shows the secret: %s", msg)
		}
	}
}
