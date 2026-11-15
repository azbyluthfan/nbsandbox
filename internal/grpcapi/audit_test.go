// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package grpcapi

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	pb "github.com/azbyluthfan/nbsandbox/gen/nbsandbox/v1"
)

// gRPC calls are recorded like REST ones (via grpc), refusals included;
// administrators list, stop and drain runs.
func TestAuditAndAdminRuns(t *testing.T) {
	ctx := context.Background()
	as := newServer(t)
	alice, root := as("alice"), as("root")
	sp, err := alice.spaces.CreateSpace(ctx, connect.NewRequest(&pb.CreateSpaceRequest{Name: "g"}))
	if err != nil {
		t.Fatal(err)
	}
	sid := sp.Msg.Id
	up := alice.spaces.UploadFiles(ctx)
	for _, m := range []*pb.UploadFilesRequest{
		{Part: &pb.UploadFilesRequest_Target{Target: &pb.UploadTarget{SpaceId: sid}}},
		{Part: &pb.UploadFilesRequest_FileName{FileName: "slow.ipynb"}},
		{Part: &pb.UploadFilesRequest_Data{Data: []byte(nbJSON)}},
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
	slow, err := alice.runs.CreateRun(ctx, connect.NewRequest(&pb.CreateRunRequest{SpaceId: sid, Notebook: "slow.ipynb"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := alice.admin.AdminListAudit(ctx, connect.NewRequest(&pb.AdminListAuditRequest{})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("alice reads the audit log: %v", err)
	}

	// Stop alice's run as an administrator.
	if _, err := root.admin.AdminCancelRun(ctx, connect.NewRequest(&pb.AdminCancelRunRequest{RunId: "nope"})); code(err) != connect.CodeInvalidArgument {
		t.Errorf("bad run id: %v", err)
	}
	c, err := root.admin.AdminCancelRun(ctx, connect.NewRequest(&pb.AdminCancelRunRequest{RunId: slow.Msg.Id, Reason: "runaway"}))
	if err != nil || c.Msg.Owner != "alice" || c.Msg.Run.CancelledBy != "root" {
		t.Fatalf("cancel: %v %v", c, err)
	}
	l, err := root.admin.AdminListRuns(ctx, connect.NewRequest(&pb.AdminListRunsRequest{Owner: "alice", Limit: 10}))
	if err != nil || len(l.Msg.Items) != 1 || l.Msg.Items[0].Run.Id != slow.Msg.Id || l.Msg.Items[0].Run.Result != nil {
		t.Fatalf("list: %v %v", l, err)
	}

	waitStatus := func(id, want string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			r, err := alice.runs.GetRun(ctx, connect.NewRequest(&pb.GetRunRequest{SpaceId: sid, RunId: id}))
			if err == nil && r.Msg.Status == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("run %s not %s: %v %v", id, want, r, err)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	waitStatus(slow.Msg.Id, "cancelled")

	// Drain and resume.
	d, err := root.admin.AdminSetDrain(ctx, connect.NewRequest(&pb.AdminSetDrainRequest{Draining: true, Reason: "upgrade"}))
	if err != nil || !d.Msg.Draining || d.Msg.By != "root" || d.Msg.Since == nil {
		t.Fatalf("drain: %v %v", d, err)
	}
	queued, err := alice.runs.CreateRun(ctx, connect.NewRequest(&pb.CreateRunRequest{SpaceId: sid, Notebook: "nb.ipynb"}))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if g, err := root.admin.AdminGetDrain(ctx, connect.NewRequest(&pb.AdminGetDrainRequest{})); err != nil || g.Msg.QueuedRuns != 1 {
		t.Fatalf("while drained: %v %v", g, err)
	}
	if _, err := root.admin.AdminSetDrain(ctx, connect.NewRequest(&pb.AdminSetDrainRequest{Draining: false})); err != nil {
		t.Fatal(err)
	}
	waitStatus(queued.Msg.Id, "succeeded")

	// The audit log, through gRPC.
	a, err := root.admin.AdminListAudit(ctx, connect.NewRequest(&pb.AdminListAuditRequest{SpaceId: sid, Limit: 50}))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]*pb.AuditEvent{}
	for _, e := range a.Msg.Items {
		if _, ok := seen[e.Action]; !ok {
			seen[e.Action] = e
		}
	}
	for action, check := range map[string]func(*pb.AuditEvent) bool{
		"CreateSpace": func(e *pb.AuditEvent) bool {
			return e.Actor == "alice" && e.Via == "grpc" && e.Status == 200 && e.Detail == "name g"
		},
		"UploadFiles":    func(e *pb.AuditEvent) bool { return e.Detail == "slow.ipynb, nb.ipynb" },
		"CreateRun":      func(e *pb.AuditEvent) bool { return e.Target == queued.Msg.Id && e.Detail == "notebook nb.ipynb" },
		"AdminCancelRun": func(e *pb.AuditEvent) bool { return e.Actor == "root" && e.Target == slow.Msg.Id },
	} {
		if e := seen[action]; e == nil || !check(e) {
			t.Errorf("%s: %v", action, e)
		}
	}
	if r, err := root.admin.AdminListAudit(ctx, connect.NewRequest(&pb.AdminListAuditRequest{Actor: "alice", Action: "AdminListAudit"})); err != nil ||
		len(r.Msg.Items) != 1 || r.Msg.Items[0].Status != 403 {
		t.Errorf("alice's refused read: %v %v", r, err)
	}
	if _, err := root.admin.AdminListAudit(ctx, connect.NewRequest(&pb.AdminListAuditRequest{Cursor: "x"})); code(err) != connect.CodeInvalidArgument {
		t.Errorf("bad cursor: %v", err)
	}
}
