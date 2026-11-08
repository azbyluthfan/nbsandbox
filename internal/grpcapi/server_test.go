// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package grpcapi

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/crypto/bcrypt"
	"google.golang.org/protobuf/types/known/structpb"

	pb "github.com/azbyluthfan/nbsandbox/gen/nbsandbox/v1"
	"github.com/azbyluthfan/nbsandbox/gen/nbsandbox/v1/nbsandboxv1connect"
	"github.com/azbyluthfan/nbsandbox/internal/api"
	"github.com/azbyluthfan/nbsandbox/internal/auth"
	"github.com/azbyluthfan/nbsandbox/internal/blob"
	"github.com/azbyluthfan/nbsandbox/internal/isolation"
	"github.com/azbyluthfan/nbsandbox/internal/runs"
	"github.com/azbyluthfan/nbsandbox/internal/spaces"
	"github.com/azbyluthfan/nbsandbox/internal/store/storetest"
)

const nbJSON = `{"nbformat": 4, "nbformat_minor": 5, "metadata": {}, "cells": []}`

// fakeBackend writes what the runner would. A notebook named slow* waits to
// be cancelled; lines* prints ten lines.
type fakeBackend struct{}

func (fakeBackend) Name() string                                        { return "fake" }
func (fakeBackend) Orphans(context.Context) ([]isolation.Orphan, error) { return nil, nil }
func (fakeBackend) Kill(context.Context, string) error                  { return nil }
func (fakeBackend) Run(ctx context.Context, spec isolation.RunSpec, out isolation.LineSink) (isolation.RunResult, error) {
	dir := spec.RunDir
	out.Line("running " + spec.Notebook)
	if strings.HasPrefix(spec.Notebook, "lines") {
		for i := 1; i <= 10; i++ {
			out.Line(fmt.Sprintf("tick %d", i))
			time.Sleep(10 * time.Millisecond)
		}
	}
	if strings.HasPrefix(spec.Notebook, "report") {
		_ = os.WriteFile(filepath.Join(dir, ".nbsandbox-events.jsonl"), []byte(`{"kind":"item","item":{"n":1}}`+"\n"+
			`{"kind":"progress","fraction":0.5,"message":"half"}`+"\n"+`{"kind":"item","item":{"n":2},"total":2}`+"\n"), 0o600)
	}
	if strings.HasPrefix(spec.Notebook, "slow") {
		<-ctx.Done()
		_ = os.WriteFile(filepath.Join(dir, ".nbsandbox-status.json"), []byte(`{"schema_version":2,"state":"terminated","cgroup":{}}`), 0o600)
		return isolation.RunResult{Stopped: true, ExitCode: 143}, nil
	}
	_ = os.WriteFile(filepath.Join(dir, "output.ipynb"), []byte(nbJSON), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "output.html"), []byte("<html></html>"), 0o600)
	if !strings.HasPrefix(spec.Notebook, "report") { // a streamed run returns items, not a value
		_ = os.WriteFile(filepath.Join(dir, "output.json"), []byte(`{"answer": 42}`), 0o600)
	}
	_ = os.WriteFile(filepath.Join(dir, "artifacts", "summary.csv"), []byte("a,b\n1,2\n"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, ".nbsandbox-status.json"), []byte(`{"schema_version":2,"state":"succeeded","rendered":true,"cgroup":{}}`), 0o600)
	return isolation.RunResult{}, nil
}

type clients struct {
	spaces nbsandboxv1connect.SpaceServiceClient
	fns    nbsandboxv1connect.FunctionServiceClient
	runs   nbsandboxv1connect.RunServiceClient
	admin  nbsandboxv1connect.AdminServiceClient
}

// newServer serves the gRPC API over HTTP/2 and returns clients speaking
// the gRPC protocol as user (no credentials when "").
func newServer(t *testing.T) func(user string) clients {
	t.Helper()
	var users strings.Builder
	users.WriteString("users:\n")
	for _, u := range []string{"alice", "bob", "root"} {
		h, _ := bcrypt.GenerateFromPassword([]byte(u+"-password"), bcrypt.MinCost)
		fmt.Fprintf(&users, "  - username: %s\n    password_hash: %q\n    admin: %t\n", u, h, u == "root")
	}
	us, err := auth.ParseUsers([]byte(users.String()))
	if err != nil {
		t.Fatal(err)
	}
	db := storetest.Open(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc, err := spaces.New(spaces.Config{DataDir: t.TempDir(), MaxFile: 1 << 20, MaxSpace: 10 << 20}, db, blob.NewMemory(), log)
	if err != nil {
		t.Fatal(err)
	}
	mgr := runs.New(runs.Config{Image: "nbsandbox-runner:test", Concurrency: 2, Timeout: 10 * time.Second, LogCap: 1 << 20,
		Retention: 24 * time.Hour, DependencyCheck: "warn",
		Limits: isolation.Limits{MemoryBytes: 2 << 30, NanoCPUs: 1e9, PIDs: 256, TmpfsBytes: 256 << 20, StopGrace: 50 * time.Millisecond}},
		runs.Deps{DB: db, Spaces: svc, Backend: fakeBackend{}, Log: log})
	if err := mgr.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	mgr.SetDockerUp(true)
	t.Cleanup(func() { mgr.Shutdown(0) })
	h := Handler(Deps{Auth: &auth.Authenticator{Users: us}, Spaces: svc, Runs: mgr, Logger: log, Shutdown: make(chan struct{}),
		Runtime: func() (api.RuntimeInfo, error) {
			var ri api.RuntimeInfo
			ri.Image.Ref, ri.PythonVersion, ri.Packages, ri.Runtime = "nbsandbox-runner:test", "3.13.18", map[string]string{"pandas": "3.1.1"}, "runc"
			return ri, nil
		}})
	srv := httptest.NewUnstartedServer(h)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return func(user string) clients {
		opts := []connect.ClientOption{connect.WithGRPC()}
		if user != "" {
			opts = append(opts, connect.WithInterceptors(basic(user)))
		}
		return clients{
			spaces: nbsandboxv1connect.NewSpaceServiceClient(srv.Client(), srv.URL, opts...),
			fns:    nbsandboxv1connect.NewFunctionServiceClient(srv.Client(), srv.URL, opts...),
			runs:   nbsandboxv1connect.NewRunServiceClient(srv.Client(), srv.URL, opts...),
			admin:  nbsandboxv1connect.NewAdminServiceClient(srv.Client(), srv.URL, opts...),
		}
	}
}

// basic adds the user's basic-auth credentials to every call.
type basic string

func (b basic) set(h http.Header) {
	req, _ := http.NewRequest("GET", "/", nil)
	req.SetBasicAuth(string(b), string(b)+"-password")
	h.Set("Authorization", req.Header.Get("Authorization"))
}

func (b basic) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, r connect.AnyRequest) (connect.AnyResponse, error) {
		b.set(r.Header())
		return next(ctx, r)
	}
}

func (b basic) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, s connect.Spec) connect.StreamingClientConn {
		c := next(ctx, s)
		b.set(c.RequestHeader())
		return c
	}
}

func (b basic) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

func code(err error) connect.Code { return connect.CodeOf(err) }

// Both transports cover the same operations: every RPC is an operation with
// a scope, and every operation has an RPC (the REST side checks the same in
// api's TestRoutesCoverOperations).
func TestRPCsCoverOperations(t *testing.T) {
	got := Procedures()
	for _, op := range got {
		if op.Scope() == "" {
			t.Errorf("RPC %s is not an operation", op)
		}
	}
	for _, op := range auth.AllOps() {
		if !slices.Contains(got, op) {
			t.Errorf("operation %s has no RPC", op)
		}
	}
}

func download(t *testing.T, st *connect.ServerStreamForClient[pb.FileChunk]) []byte {
	t.Helper()
	var b bytes.Buffer
	for st.Receive() {
		b.Write(st.Msg().GetData())
	}
	if err := st.Err(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// Every RPC, as alice; bob sees nothing of hers; no credentials, nothing.
func TestEveryRPC(t *testing.T) {
	ctx := context.Background()
	as := newServer(t)
	alice, bob, anon := as("alice"), as("bob"), as("")

	if _, err := anon.spaces.ListSpaces(ctx, connect.NewRequest(&pb.ListSpacesRequest{})); code(err) != connect.CodeUnauthenticated {
		t.Fatalf("anonymous: %v", err)
	}
	sp, err := alice.spaces.CreateSpace(ctx, connect.NewRequest(&pb.CreateSpaceRequest{Name: "grpc"}))
	if err != nil || sp.Msg.Name != "grpc" || sp.Msg.Usage == nil {
		t.Fatalf("create: %v %v", sp, err)
	}
	sid := sp.Msg.Id
	if _, err := bob.spaces.GetSpace(ctx, connect.NewRequest(&pb.GetSpaceRequest{SpaceId: sid})); code(err) != connect.CodeNotFound {
		t.Fatalf("bob sees alice's space: %v", err)
	}
	if _, err := alice.spaces.GetSpace(ctx, connect.NewRequest(&pb.GetSpaceRequest{SpaceId: "nope"})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("bad id: %v", err)
	}
	if got, err := alice.spaces.UpdateSpace(ctx, connect.NewRequest(&pb.UpdateSpaceRequest{SpaceId: sid, Name: "renamed"})); err != nil || got.Msg.Name != "renamed" {
		t.Fatalf("rename: %v %v", got, err)
	}
	if l, err := alice.spaces.ListSpaces(ctx, connect.NewRequest(&pb.ListSpacesRequest{Limit: 10})); err != nil || len(l.Msg.Items) != 1 {
		t.Fatalf("list: %v %v", l, err)
	}

	// Files: an upload stream of two files (one in chunks).
	up := alice.spaces.UploadFiles(ctx)
	for _, m := range []*pb.UploadFilesRequest{
		{Part: &pb.UploadFilesRequest_Target{Target: &pb.UploadTarget{SpaceId: sid}}},
		{Part: &pb.UploadFilesRequest_FileName{FileName: "nb.ipynb"}},
		{Part: &pb.UploadFilesRequest_Data{Data: []byte(nbJSON[:10])}},
		{Part: &pb.UploadFilesRequest_Data{Data: []byte(nbJSON[10:])}},
		{Part: &pb.UploadFilesRequest_FileName{FileName: "lines.ipynb"}},
		{Part: &pb.UploadFilesRequest_Data{Data: []byte(nbJSON)}},
		{Part: &pb.UploadFilesRequest_FileName{FileName: "slow.ipynb"}},
		{Part: &pb.UploadFilesRequest_Data{Data: []byte(nbJSON)}},
	} {
		if err := up.Send(m); err != nil {
			t.Fatal(err)
		}
	}
	upRes, err := up.CloseAndReceive()
	if err != nil || len(upRes.Msg.Files) != 3 {
		t.Fatalf("upload: %v %v", upRes, err)
	}
	put := alice.spaces.PutFile(ctx)
	_ = put.Send(&pb.PutFileRequest{Part: &pb.PutFileRequest_Target{Target: &pb.PutTarget{SpaceId: sid, Path: "data/x.txt"}}})
	_ = put.Send(&pb.PutFileRequest{Part: &pb.PutFileRequest_Data{Data: []byte("hello")}})
	putRes, err := put.CloseAndReceive()
	if err != nil || !putRes.Msg.Created || putRes.Msg.File.Size != 5 {
		t.Fatalf("put: %v %v", putRes, err)
	}
	// Replacing needs the version being replaced.
	put = alice.spaces.PutFile(ctx)
	_ = put.Send(&pb.PutFileRequest{Part: &pb.PutFileRequest_Target{Target: &pb.PutTarget{SpaceId: sid, Path: "data/x.txt"}}})
	if _, err := put.CloseAndReceive(); code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("replace without if_match: %v", err)
	}
	if got := download(t, mustStream(alice.spaces.GetFile(ctx, connect.NewRequest(&pb.GetFileRequest{SpaceId: sid, Path: "data/x.txt"})))); string(got) != "hello" {
		t.Fatalf("download %q", got)
	}
	if f, err := alice.spaces.CreateFolder(ctx, connect.NewRequest(&pb.CreateFolderRequest{SpaceId: sid, Path: "empty"})); err != nil || f.Msg.Type != "folder" {
		t.Fatalf("folder: %v %v", f, err)
	}
	if f, err := alice.spaces.MoveFile(ctx, connect.NewRequest(&pb.MoveFileRequest{SpaceId: sid, Path: "data/x.txt", NewPath: "data/y.txt"})); err != nil || f.Msg.Path != "data/y.txt" {
		t.Fatalf("move: %v %v", f, err)
	}
	if _, err := alice.spaces.DeleteFile(ctx, connect.NewRequest(&pb.DeleteFileRequest{SpaceId: sid, Path: "empty"})); err != nil {
		t.Fatal(err)
	}
	if l, err := alice.spaces.ListFiles(ctx, connect.NewRequest(&pb.ListFilesRequest{SpaceId: sid})); err != nil || len(l.Msg.Items) != 5 {
		t.Fatalf("files: %v %v", l, err)
	}

	// Runs.
	if rt, err := alice.runs.GetRuntime(ctx, connect.NewRequest(&pb.GetRuntimeRequest{})); err != nil || rt.Msg.Packages["pandas"] != "3.1.1" {
		t.Fatalf("runtime: %v %v", rt, err)
	}
	params, _ := structpb.NewValue(map[string]any{"region": "jakarta"})
	run, err := alice.runs.CreateRun(ctx, connect.NewRequest(&pb.CreateRunRequest{SpaceId: sid, Notebook: "lines.ipynb", Parameters: params}))
	if err != nil || run.Msg.Status == "" || run.Msg.Parameters.GetStructValue().Fields["region"].GetStringValue() != "jakarta" {
		t.Fatalf("create run: %v %v", run, err)
	}
	rid := run.Msg.Id
	logs := mustStream(alice.runs.FollowRunLog(ctx, connect.NewRequest(&pb.FollowRunLogRequest{SpaceId: sid, RunId: rid})))
	var lines []string
	var end *pb.LogEnd
	for logs.Receive() {
		if l := logs.Msg().GetLine(); l != nil {
			lines = append(lines, l.Text)
		}
		end = logs.Msg().GetEnd()
	}
	if logs.Err() != nil || len(lines) < 11 || end == nil || end.Status != "succeeded" {
		t.Fatalf("follow log: %d lines, end %v, %v", len(lines), end, logs.Err())
	}
	res, err := alice.runs.GetRunResult(ctx, connect.NewRequest(&pb.GetRunResultRequest{SpaceId: sid, RunId: rid, WaitSeconds: 5}))
	if err != nil || res.Msg.GetResult().GetStatus() != "succeeded" || res.Msg.GetResult().Output.GetStructValue().Fields["answer"].GetNumberValue() != 42 {
		t.Fatalf("result: %v %v", res, err)
	}
	got, err := alice.runs.GetRun(ctx, connect.NewRequest(&pb.GetRunRequest{SpaceId: sid, RunId: rid}))
	if err != nil || got.Msg.Result == nil || got.Msg.Result.Status != "succeeded" || got.Msg.FinishedAt == nil {
		t.Fatalf("get run: %v %v", got, err)
	}
	if l, err := alice.runs.ListRuns(ctx, connect.NewRequest(&pb.ListRunsRequest{SpaceId: sid, Status: "succeeded"})); err != nil || len(l.Msg.Items) != 1 {
		t.Fatalf("list runs: %v %v", l, err)
	}
	if _, err := alice.runs.ListRuns(ctx, connect.NewRequest(&pb.ListRunsRequest{SpaceId: sid, Status: "bogus"})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("bad status: %v", err)
	}
	for name, st := range map[string]func() (*connect.ServerStreamForClient[pb.FileChunk], error){
		"notebook": func() (*connect.ServerStreamForClient[pb.FileChunk], error) {
			return alice.runs.GetRunNotebook(ctx, connect.NewRequest(&pb.GetRunFileRequest{SpaceId: sid, RunId: rid}))
		},
		"html": func() (*connect.ServerStreamForClient[pb.FileChunk], error) {
			return alice.runs.GetRunHTML(ctx, connect.NewRequest(&pb.GetRunFileRequest{SpaceId: sid, RunId: rid}))
		},
		"log": func() (*connect.ServerStreamForClient[pb.FileChunk], error) {
			return alice.runs.GetRunLog(ctx, connect.NewRequest(&pb.GetRunFileRequest{SpaceId: sid, RunId: rid}))
		},
		"artifact": func() (*connect.ServerStreamForClient[pb.FileChunk], error) {
			return alice.runs.GetArtifact(ctx, connect.NewRequest(&pb.GetArtifactRequest{SpaceId: sid, RunId: rid, Path: "summary.csv"}))
		},
	} {
		if b := download(t, mustStream(st())); len(b) == 0 {
			t.Errorf("%s: empty", name)
		}
	}
	if a, err := alice.runs.ListArtifacts(ctx, connect.NewRequest(&pb.ListArtifactsRequest{SpaceId: sid, RunId: rid})); err != nil || len(a.Msg.Items) != 1 {
		t.Fatalf("artifacts: %v %v", a, err)
	}
	if f, err := alice.runs.PromoteArtifact(ctx, connect.NewRequest(&pb.PromoteArtifactRequest{SpaceId: sid, RunId: rid, Artifact: "summary.csv", To: "out/summary.csv"})); err != nil || f.Msg.Path != "out/summary.csv" {
		t.Fatalf("promote: %v %v", f, err)
	}
	evs := mustStream(alice.runs.FollowRunEvents(ctx, connect.NewRequest(&pb.FollowRunEventsRequest{SpaceId: sid, RunId: rid})))
	var statuses []string
	for evs.Receive() {
		statuses = append(statuses, evs.Msg().Status)
	}
	if evs.Err() != nil || !slices.Equal(statuses, []string{"queued", "running", "succeeded"}) {
		t.Fatalf("run events %v %v", statuses, evs.Err())
	}

	// Cancel a slow run while following the space's events.
	sctx, stop := context.WithCancel(ctx)
	defer stop()
	spaceEvs := mustStream(alice.runs.FollowSpaceEvents(sctx, connect.NewRequest(&pb.FollowSpaceEventsRequest{SpaceId: sid})))
	slow, err := alice.runs.CreateRun(ctx, connect.NewRequest(&pb.CreateRunRequest{SpaceId: sid, Notebook: "slow.ipynb"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := alice.runs.DeleteRun(ctx, connect.NewRequest(&pb.DeleteRunRequest{SpaceId: sid, RunId: slow.Msg.Id})); code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("delete an active run: %v", err)
	}
	if r, err := alice.runs.GetRunResult(ctx, connect.NewRequest(&pb.GetRunResultRequest{SpaceId: sid, RunId: slow.Msg.Id})); err != nil || r.Msg.GetRun() == nil {
		t.Fatalf("result of an active run: %v %v", r, err)
	}
	if c, err := alice.runs.CancelRun(ctx, connect.NewRequest(&pb.CancelRunRequest{SpaceId: sid, RunId: slow.Msg.Id, Reason: "enough"})); err != nil || c.Msg.Id != slow.Msg.Id {
		t.Fatalf("cancel: %v %v", c, err)
	}
	for spaceEvs.Receive() {
		if e := spaceEvs.Msg(); e.RunId == slow.Msg.Id && e.Status == "cancelled" {
			if e.Seq == 0 || e.Result.GetError().GetCancelReason() != "enough" {
				t.Fatalf("cancelled event %v", e)
			}
			break
		}
	}
	stop()
	if _, err := alice.runs.DeleteRun(ctx, connect.NewRequest(&pb.DeleteRunRequest{SpaceId: sid, RunId: slow.Msg.Id})); err != nil {
		t.Fatal(err)
	}

	// Functions.
	v, err := alice.fns.PublishFunction(ctx, connect.NewRequest(&pb.PublishFunctionRequest{SpaceId: sid, Name: "fn", Notebook: "nb.ipynb", Files: []string{"out"}}))
	if err != nil || v.Msg.Version != 1 || len(v.Msg.Files) != 1 {
		t.Fatalf("publish: %v %v", v, err)
	}
	if f, err := alice.fns.GetFunction(ctx, connect.NewRequest(&pb.GetFunctionRequest{SpaceId: sid, Name: "fn"})); err != nil || f.Msg.LatestVersion != 1 {
		t.Fatalf("function: %v %v", f, err)
	}
	if l, err := alice.fns.ListFunctions(ctx, connect.NewRequest(&pb.ListFunctionsRequest{SpaceId: sid})); err != nil || len(l.Msg.Items) != 1 {
		t.Fatalf("functions: %v %v", l, err)
	}
	if fv, err := alice.fns.GetFunctionVersion(ctx, connect.NewRequest(&pb.GetFunctionVersionRequest{SpaceId: sid, Name: "fn", Version: 1})); err != nil || fv.Msg.Notebook != "nb.ipynb" {
		t.Fatalf("version: %v %v", fv, err)
	}
	if c, err := alice.fns.GetFunctionContract(ctx, connect.NewRequest(&pb.GetFunctionContractRequest{SpaceId: sid, Name: "fn"})); err != nil || c.Msg.Version != 1 {
		t.Fatalf("contract: %v %v", c, err)
	}
	inv, err := alice.fns.InvokeFunction(ctx, connect.NewRequest(&pb.InvokeFunctionRequest{SpaceId: sid, Name: "fn", WaitSeconds: 10, IdempotencyKey: "k1"}))
	if err != nil || inv.Msg.GetResult().GetStatus() != "succeeded" || inv.Msg.Replay {
		t.Fatalf("invoke: %v %v", inv, err)
	}
	again, err := alice.fns.InvokeFunction(ctx, connect.NewRequest(&pb.InvokeFunctionRequest{SpaceId: sid, Name: "fn", IdempotencyKey: "k1"}))
	if err != nil || !again.Msg.Replay || again.Msg.GetRun().GetId() != inv.Msg.GetResult().GetRunId() {
		t.Fatalf("replay: %v %v", again, err)
	}
	if _, err := alice.fns.InvokeFunction(ctx, connect.NewRequest(&pb.InvokeFunctionRequest{SpaceId: sid, Name: "fn", WaitSeconds: 61})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("too long a wait: %v", err)
	}
	if _, err := alice.fns.DeleteFunction(ctx, connect.NewRequest(&pb.DeleteFunctionRequest{SpaceId: sid, Name: "fn"})); err != nil {
		t.Fatal(err)
	}
	if _, err := alice.spaces.DeleteSpace(ctx, connect.NewRequest(&pb.DeleteSpaceRequest{SpaceId: sid})); err != nil {
		t.Fatal(err)
	}
	if _, err := alice.spaces.GetSpace(ctx, connect.NewRequest(&pb.GetSpaceRequest{SpaceId: sid})); code(err) != connect.CodeNotFound {
		t.Fatalf("after delete: %v", err)
	}
}

func mustStream[M any](st *connect.ServerStreamForClient[M], err error) *connect.ServerStreamForClient[M] {
	if err != nil {
		panic(err)
	}
	return st
}

// A streamed output reaches gRPC callers item by item, then as the result;
// progress is on the events stream when asked for.
func TestResultStreamOverGRPC(t *testing.T) {
	ctx := context.Background()
	alice := newServer(t)("alice")
	sp, err := alice.spaces.CreateSpace(ctx, connect.NewRequest(&pb.CreateSpaceRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	sid := sp.Msg.Id
	up := alice.spaces.UploadFiles(ctx)
	_ = up.Send(&pb.UploadFilesRequest{Part: &pb.UploadFilesRequest_Target{Target: &pb.UploadTarget{SpaceId: sid}}})
	_ = up.Send(&pb.UploadFilesRequest{Part: &pb.UploadFilesRequest_FileName{FileName: "report.ipynb"}})
	_ = up.Send(&pb.UploadFilesRequest{Part: &pb.UploadFilesRequest_Data{Data: []byte(streamingNB)}})
	if _, err := up.CloseAndReceive(); err != nil {
		t.Fatal(err)
	}
	run, err := alice.runs.CreateRun(ctx, connect.NewRequest(&pb.CreateRunRequest{SpaceId: sid, Notebook: "report.ipynb"}))
	if err != nil {
		t.Fatal(err)
	}
	collect := func(st *connect.ServerStreamForClient[pb.ResultEvent]) (items []*pb.ResultItem, progress int, res *pb.Result) {
		t.Helper()
		for st.Receive() {
			switch e := st.Msg().Event.(type) {
			case *pb.ResultEvent_Item:
				items = append(items, e.Item)
			case *pb.ResultEvent_Progress:
				progress++
			case *pb.ResultEvent_Result:
				res = e.Result
			}
		}
		if err := st.Err(); err != nil {
			t.Fatal(err)
		}
		return items, progress, res
	}
	items, progress, res := collect(mustStream(alice.runs.FollowRunResult(ctx, connect.NewRequest(&pb.FollowRunResultRequest{
		SpaceId: sid, RunId: run.Msg.Id, IncludeProgress: true}))))
	if len(items) != 2 || items[0].Index != 1 || items[1].GetTotal() != 2 || items[0].Item.GetStructValue().Fields["n"].GetNumberValue() != 1 ||
		progress != 1 || res.Status != "succeeded" || res.GetItems() != 2 || len(res.Output.GetListValue().Values) != 2 {
		t.Fatalf("items %v progress %d result %v", items, progress, res)
	}
	// Resumed after item 1.
	items, _, _ = collect(mustStream(alice.runs.FollowRunResult(ctx, connect.NewRequest(&pb.FollowRunResultRequest{
		SpaceId: sid, RunId: run.Msg.Id, After: 1}))))
	if len(items) != 1 || items[0].Index != 2 {
		t.Fatalf("resumed %v", items)
	}
	// Events: status only, unless asked.
	kinds := func(include ...string) []string {
		evs := mustStream(alice.runs.FollowRunEvents(ctx, connect.NewRequest(&pb.FollowRunEventsRequest{SpaceId: sid, RunId: run.Msg.Id, Include: include})))
		var out []string
		for evs.Receive() {
			out = append(out, evs.Msg().Kind)
		}
		return out
	}
	if got := kinds(); !slices.Equal(got, []string{"status", "status", "status"}) {
		t.Fatalf("events %v", got)
	}
	if got := kinds("progress"); !slices.Equal(got, []string{"status", "status", "progress", "status"}) {
		t.Fatalf("events with progress %v", got)
	}

	// Published with a contract in the request, invoked as a stream.
	in, _ := structpb.NewValue(map[string]any{"type": "object"})
	out, _ := structpb.NewValue(map[string]any{"mode": "stream", "item": map[string]any{"type": "object"}})
	v, err := alice.fns.PublishFunction(ctx, connect.NewRequest(&pb.PublishFunctionRequest{SpaceId: sid, Name: "fn", Notebook: "report.ipynb",
		Contract: &pb.ContractSpec{Input: in, Output: out}}))
	if err != nil || v.Msg.Contract.Mode != "stream" || v.Msg.Contract.ModeInferred || v.Msg.Contract.Item == nil {
		t.Fatalf("publish %v %v", v, err)
	}
	items, _, res = collect(mustStream(alice.fns.InvokeFunctionStream(ctx, connect.NewRequest(&pb.InvokeFunctionRequest{SpaceId: sid, Name: "fn"}))))
	if len(items) != 2 || res.GetItems() != 2 || res.Function != "fn" {
		t.Fatalf("invoke stream %v %v", items, res)
	}
}

const streamingNB = `{"nbformat": 4, "nbformat_minor": 5, "metadata": {}, "cells": [
 {"cell_type": "code", "source": "import nbsandbox\nnbsandbox.emit({'n': 1})", "metadata": {}, "outputs": [], "execution_count": null}]}`
