// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package grpcapi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"connectrpc.com/connect"

	pb "github.com/azbyluthfan/nbsandbox/gen/nbsandbox/v1"
)

func TestSessionsOverGRPC(t *testing.T) {
	ctx := context.Background()
	as := newServer(t)
	alice, bob, root := as("alice"), as("bob"), as("root")
	sp, err := alice.spaces.CreateSpace(ctx, connect.NewRequest(&pb.CreateSpaceRequest{Name: "s"}))
	if err != nil {
		t.Fatal(err)
	}
	sid := sp.Msg.Id
	up := alice.spaces.UploadFiles(ctx)
	for _, m := range []*pb.UploadFilesRequest{
		{Part: &pb.UploadFilesRequest_Target{Target: &pb.UploadTarget{SpaceId: sid}}},
		{Part: &pb.UploadFilesRequest_FileName{FileName: "nb.ipynb"}},
		{Part: &pb.UploadFilesRequest_Data{Data: []byte(nbJSON)}},
	} {
		if err := up.Send(m); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := up.CloseAndReceive(); err != nil {
		t.Fatal(err)
	}

	open, err := alice.sess.OpenSession(ctx, connect.NewRequest(&pb.OpenSessionRequest{SpaceId: sid, Notebook: "nb.ipynb"}))
	if err != nil || open.Msg.Mode != "read-write" || open.Msg.Owner != "" {
		t.Fatalf("open: %v %v", open, err)
	}
	id := open.Msg.Id
	if _, err := bob.sess.GetSession(ctx, connect.NewRequest(&pb.GetSessionRequest{SpaceId: sid, SessionId: id})); code(err) != connect.CodeNotFound {
		t.Fatalf("bob: %v", err)
	}
	if _, err := alice.sess.OpenSession(ctx, connect.NewRequest(&pb.OpenSessionRequest{SpaceId: sid, Notebook: "x.txt"})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("not a notebook: %v", err)
	}

	// The channel: the first request names the session and runs a cell.
	ch := alice.sess.SessionChannel(ctx)
	exec := `{"channel":"shell","header":{"msg_id":"m1","msg_type":"execute_request"},"content":{"code":"print(2)"}}`
	if err := ch.Send(&pb.SessionChannelRequest{SpaceId: sid, SessionId: id, Json: exec}); err != nil {
		t.Fatal(err)
	}
	if err := ch.Send(&pb.SessionChannelRequest{Json: `{"channel":"stdin","header":{"msg_id":"m2","msg_type":"input_reply"},"content":{}}`}); err != nil {
		t.Fatal(err)
	}
	var sawHello, sawOutput, sawRefusal bool
	for !sawHello || !sawOutput || !sawRefusal {
		m, err := ch.Receive()
		if err != nil {
			t.Fatalf("receive: %v", err)
		}
		var msg struct {
			Channel string         `json:"channel"`
			Header  map[string]any `json:"header"`
			Content map[string]any `json:"content"`
		}
		_ = json.Unmarshal([]byte(m.Json), &msg)
		switch {
		case msg.Channel == "bridge" && msg.Content["status"] == "attached":
			sawHello = true
		case msg.Header["msg_type"] == "stream" && msg.Content["text"] == "print(2)":
			sawOutput = true
		case msg.Channel == "bridge" && msg.Content["refused"] == true:
			sawRefusal = strings.Contains(m.Json, "input_reply")
		}
	}
	_ = ch.CloseRequest()
	_ = ch.CloseResponse()

	if _, err := alice.sess.InterruptSession(ctx, connect.NewRequest(&pb.GetSessionRequest{SpaceId: sid, SessionId: id})); err != nil {
		t.Fatal(err)
	}
	if r, err := alice.sess.SyncSession(ctx, connect.NewRequest(&pb.GetSessionRequest{SpaceId: sid, SessionId: id})); err != nil || len(r.Msg.Conflicts) != 0 {
		t.Fatalf("sync: %v %v", r, err)
	}
	if l, err := root.admin.AdminListSessions(ctx, connect.NewRequest(&pb.AdminListSessionsRequest{})); err != nil || len(l.Msg.Items) != 1 || l.Msg.Items[0].Owner != "alice" {
		t.Fatalf("admin list: %v %v", l, err)
	}
	if _, err := alice.admin.AdminListSessions(ctx, connect.NewRequest(&pb.AdminListSessionsRequest{})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("admin list as alice: %v", err)
	}
	end, err := alice.sess.EndSession(ctx, connect.NewRequest(&pb.GetSessionRequest{SpaceId: sid, SessionId: id}))
	if err != nil || end.Msg.Status != "ended" || end.Msg.EndedAt == nil {
		t.Fatalf("end: %v %v", end, err)
	}
	if _, err := alice.sess.RestartSession(ctx, connect.NewRequest(&pb.GetSessionRequest{SpaceId: sid, SessionId: id})); code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("restart an ended session: %v", err)
	}
	if l, err := alice.sess.ListSessions(ctx, connect.NewRequest(&pb.ListSessionsRequest{SpaceId: sid})); err != nil || len(l.Msg.Items) != 0 {
		t.Fatalf("list: %v %v", l, err)
	}
	again, err := alice.sess.OpenSession(ctx, connect.NewRequest(&pb.OpenSessionRequest{SpaceId: sid, Notebook: "nb.ipynb", Mode: "read-only"}))
	if err != nil {
		t.Fatal(err)
	}
	if e, err := root.admin.AdminEndSession(ctx, connect.NewRequest(&pb.AdminEndSessionRequest{SessionId: again.Msg.Id, Reason: "maintenance"})); err != nil ||
		!strings.Contains(e.Msg.EndReason, "maintenance") {
		t.Fatalf("admin end: %v %v", e, err)
	}
}
