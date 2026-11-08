// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package grpcapi

import (
	"context"
	"errors"
	"io"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/azbyluthfan/nbsandbox/gen/nbsandbox/v1"
	"github.com/azbyluthfan/nbsandbox/internal/api"
	"github.com/azbyluthfan/nbsandbox/internal/completions"
	"github.com/azbyluthfan/nbsandbox/internal/isolation"
	"github.com/azbyluthfan/nbsandbox/internal/runs"
	"github.com/azbyluthfan/nbsandbox/internal/spaces"
)

func (s *server) run(v runs.View) (*pb.Run, error) { return conv(v, &pb.Run{}) }

func waitFor(seconds int32) (time.Duration, error) {
	d := time.Duration(seconds) * time.Second
	if d < 0 || d > api.MaxWait {
		return 0, invalid("wait_seconds must be between 0 and " + api.MaxWait.String())
	}
	return d, nil
}

// answer is a finished run's result, or the run as it is now.
func (s *server) answer(ctx context.Context, spaceID, runID string, wait time.Duration) (*pb.RunOrResult, error) {
	var (
		res  runs.CallerResult
		now  runs.View
		done bool
		err  error
	)
	if wait > 0 {
		res, now, done, err = s.d.Runs.WaitResult(ctx, owner(ctx), spaceID, runID, wait, s.d.Shutdown)
	} else {
		var sa *runs.ErrStillActive
		res, err = s.d.Runs.GetResult(ctx, owner(ctx), spaceID, runID)
		done = err == nil
		if errors.As(err, &sa) {
			now, err = s.d.Runs.Get(ctx, owner(ctx), spaceID, runID)
		}
	}
	if err != nil {
		return nil, s.toErr(err)
	}
	if done {
		r, err := conv(res, &pb.Result{})
		if err != nil {
			return nil, s.toErr(err)
		}
		return &pb.RunOrResult{Answer: &pb.RunOrResult_Result{Result: r}}, nil
	}
	r, err := s.run(now)
	if err != nil {
		return nil, s.toErr(err)
	}
	return &pb.RunOrResult{Answer: &pb.RunOrResult_Run{Run: r}}, nil
}

func (s *server) GetRuntime(_ context.Context, _ *connect.Request[pb.GetRuntimeRequest]) (*connect.Response[pb.Runtime], error) {
	info, err := s.d.Runtime()
	if err != nil {
		return nil, s.toErr(err)
	}
	out, err := conv(info, &pb.Runtime{})
	if err != nil {
		return nil, s.toErr(err)
	}
	return connect.NewResponse(out), nil
}

func (s *server) CreateRun(ctx context.Context, req *connect.Request[pb.CreateRunRequest]) (*connect.Response[pb.Run], error) {
	m := req.Msg
	if err := validIDs(m.SpaceId); err != nil {
		return nil, err
	}
	if m.Notebook == "" {
		return nil, invalid("notebook is required")
	}
	params, err := rawJSON(m.Parameters)
	if err != nil {
		return nil, invalid("parameters: " + err.Error())
	}
	// The request is read: record it even if the caller goes away now.
	v, _, err := s.d.Runs.Submit(context.WithoutCancel(ctx), owner(ctx), m.SpaceId, runs.SubmitRequest{
		Notebook: m.Notebook, Parameters: params, IdempotencyKey: m.IdempotencyKey, ReadOnlySpace: m.ReadOnlySpace,
	})
	if err != nil {
		return nil, s.toErr(err)
	}
	out, err := s.run(v)
	if err != nil {
		return nil, s.toErr(err)
	}
	return connect.NewResponse(out), nil
}

func (s *server) ListRuns(ctx context.Context, req *connect.Request[pb.ListRunsRequest]) (*connect.Response[pb.ListRunsResponse], error) {
	m := req.Msg
	if err := validIDs(m.SpaceId); err != nil {
		return nil, err
	}
	cursor, limit, err := page(m.Cursor, m.Limit)
	if err != nil {
		return nil, err
	}
	f := runs.ListFilter{Status: m.Status}
	for _, t := range []struct {
		ts  *timestamppb.Timestamp
		dst **time.Time
	}{{m.CreatedAfter, &f.CreatedAfter}, {m.CreatedBefore, &f.CreatedBefore}} {
		if t.ts != nil {
			v := t.ts.AsTime()
			*t.dst = &v
		}
	}
	list, next, err := s.d.Runs.List(ctx, owner(ctx), m.SpaceId, f, cursor, limit)
	if err != nil {
		return nil, s.toErr(err)
	}
	out := &pb.ListRunsResponse{NextCursor: next}
	for _, v := range list {
		r, err := s.run(v)
		if err != nil {
			return nil, s.toErr(err)
		}
		out.Items = append(out.Items, r)
	}
	return connect.NewResponse(out), nil
}

func (s *server) GetRun(ctx context.Context, req *connect.Request[pb.GetRunRequest]) (*connect.Response[pb.Run], error) {
	if err := validIDs(req.Msg.SpaceId, req.Msg.RunId); err != nil {
		return nil, err
	}
	v, err := s.d.Runs.Get(ctx, owner(ctx), req.Msg.SpaceId, req.Msg.RunId)
	if err != nil {
		return nil, s.toErr(err)
	}
	out, err := s.run(v)
	if err != nil {
		return nil, s.toErr(err)
	}
	return connect.NewResponse(out), nil
}

func (s *server) CancelRun(ctx context.Context, req *connect.Request[pb.CancelRunRequest]) (*connect.Response[pb.Run], error) {
	if err := validIDs(req.Msg.SpaceId, req.Msg.RunId); err != nil {
		return nil, err
	}
	v, err := s.d.Runs.Cancel(ctx, owner(ctx), req.Msg.SpaceId, req.Msg.RunId, owner(ctx), req.Msg.Reason)
	if err != nil {
		return nil, s.toErr(err)
	}
	out, err := s.run(v)
	if err != nil {
		return nil, s.toErr(err)
	}
	return connect.NewResponse(out), nil
}

func (s *server) GetRunResult(ctx context.Context, req *connect.Request[pb.GetRunResultRequest]) (*connect.Response[pb.RunOrResult], error) {
	if err := validIDs(req.Msg.SpaceId, req.Msg.RunId); err != nil {
		return nil, err
	}
	wait, err := waitFor(req.Msg.WaitSeconds)
	if err != nil {
		return nil, err
	}
	out, err := s.answer(ctx, req.Msg.SpaceId, req.Msg.RunId, wait)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(out), nil
}

func (s *server) DeleteRun(ctx context.Context, req *connect.Request[pb.DeleteRunRequest]) (*connect.Response[pb.DeleteRunResponse], error) {
	if err := validIDs(req.Msg.SpaceId, req.Msg.RunId); err != nil {
		return nil, err
	}
	if err := s.d.Runs.Delete(ctx, owner(ctx), req.Msg.SpaceId, req.Msg.RunId); err != nil {
		return nil, s.toErr(err)
	}
	return connect.NewResponse(&pb.DeleteRunResponse{}), nil
}

func (s *server) sendRunFile(ctx context.Context, spaceID, runID, rel string, st *connect.ServerStream[pb.FileChunk]) error {
	if err := validIDs(spaceID, runID); err != nil {
		return err
	}
	f, info, err := s.d.Runs.OpenOutput(ctx, owner(ctx), spaceID, runID, rel)
	if err != nil {
		return s.toErr(err)
	}
	return sendFile(st, f, &pb.File{Path: rel, Type: spaces.TypeFile, Size: info.Size(), Modified: timestamppb.New(info.ModTime())})
}

func (s *server) GetRunNotebook(ctx context.Context, req *connect.Request[pb.GetRunFileRequest], st *connect.ServerStream[pb.FileChunk]) error {
	return s.sendRunFile(ctx, req.Msg.SpaceId, req.Msg.RunId, spaces.RunOutput, st)
}

func (s *server) GetRunHTML(ctx context.Context, req *connect.Request[pb.GetRunFileRequest], st *connect.ServerStream[pb.FileChunk]) error {
	return s.sendRunFile(ctx, req.Msg.SpaceId, req.Msg.RunId, spaces.RunHTML, st)
}

func (s *server) GetRunLog(ctx context.Context, req *connect.Request[pb.GetRunFileRequest], st *connect.ServerStream[pb.FileChunk]) error {
	return s.sendRunFile(ctx, req.Msg.SpaceId, req.Msg.RunId, spaces.RunLog, st)
}

func (s *server) ListArtifacts(ctx context.Context, req *connect.Request[pb.ListArtifactsRequest]) (*connect.Response[pb.ListArtifactsResponse], error) {
	if err := validIDs(req.Msg.SpaceId, req.Msg.RunId); err != nil {
		return nil, err
	}
	files, err := s.d.Runs.Artifacts(ctx, owner(ctx), req.Msg.SpaceId, req.Msg.RunId)
	if err != nil {
		return nil, s.toErr(err)
	}
	return connect.NewResponse(&pb.ListArtifactsResponse{Items: toFiles(files)}), nil
}

func (s *server) GetArtifact(ctx context.Context, req *connect.Request[pb.GetArtifactRequest], st *connect.ServerStream[pb.FileChunk]) error {
	if err := spaces.ValidateArtifactPath(req.Msg.Path); err != nil {
		return s.toErr(err)
	}
	return s.sendRunFile(ctx, req.Msg.SpaceId, req.Msg.RunId, spaces.RunArtifacts+"/"+req.Msg.Path, st)
}

func (s *server) PromoteArtifact(ctx context.Context, req *connect.Request[pb.PromoteArtifactRequest]) (*connect.Response[pb.File], error) {
	m := req.Msg
	if err := validIDs(m.SpaceId, m.RunId); err != nil {
		return nil, err
	}
	f, err := s.d.Runs.PromoteArtifact(ctx, owner(ctx), m.SpaceId, m.RunId, m.Artifact, m.To)
	if err != nil {
		return nil, s.toErr(err)
	}
	return connect.NewResponse(toFile(f)), nil
}

// errShutdown ends a stream when the server shuts down: resume elsewhere.
var errShutdown = connect.NewError(connect.CodeUnavailable, errors.New("nbsandboxd is shutting down; resume where you were"))

func (s *server) FollowRunLog(ctx context.Context, req *connect.Request[pb.FollowRunLogRequest], st *connect.ServerStream[pb.LogEvent]) error {
	if err := validIDs(req.Msg.SpaceId, req.Msg.RunId); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	items, err := s.d.Runs.FollowLog(ctx, owner(ctx), req.Msg.SpaceId, req.Msg.RunId, req.Msg.After)
	if err != nil {
		return s.toErr(err)
	}
	for {
		select {
		case it, open := <-items:
			if !open {
				return ctx.Err()
			}
			var ev *pb.LogEvent
			switch it.Kind {
			case runs.LogLine:
				ev = &pb.LogEvent{Event: &pb.LogEvent_Line{Line: &pb.LogLine{Seq: it.Line.Seq, Text: it.Line.Text}}}
			case runs.LogDone:
				return st.Send(&pb.LogEvent{Event: &pb.LogEvent_End{End: &pb.LogEnd{Status: it.Status, LastSeq: it.LastSeq}}})
			case runs.LogDropped:
				return st.Send(&pb.LogEvent{Event: &pb.LogEvent_End{End: &pb.LogEnd{LastSeq: it.LastSeq, Dropped: true}}})
			}
			if err := st.Send(ev); err != nil {
				return err
			}
		case <-s.d.Shutdown:
			return errShutdown
		}
	}
}

func (s *server) followEvents(ctx context.Context, spaceID string, q runs.EventsQuery, st *connect.ServerStream[pb.RunEvent]) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	items, err := s.d.Runs.FollowEvents(ctx, owner(ctx), spaceID, q)
	if err != nil {
		return s.toErr(err)
	}
	for {
		select {
		case it, open := <-items:
			if !open {
				return ctx.Err()
			}
			ev := &pb.RunEvent{}
			if it.Kind == runs.EventPruned {
				ev.Pruned = it.Message
			} else {
				if err := protojson.Unmarshal(it.Data, ev); err != nil {
					return s.toErr(err)
				}
				ev.Seq, ev.Kind = it.Seq, it.Type
				if ev.Kind == "" {
					ev.Kind = "status"
				}
			}
			if err := st.Send(ev); err != nil {
				return err
			}
		case <-s.d.Shutdown:
			return errShutdown
		}
	}
}

func (s *server) FollowRunEvents(ctx context.Context, req *connect.Request[pb.FollowRunEventsRequest], st *connect.ServerStream[pb.RunEvent]) error {
	if err := validIDs(req.Msg.SpaceId, req.Msg.RunId); err != nil {
		return err
	}
	return s.followEvents(ctx, req.Msg.SpaceId, runs.EventsQuery{RunID: req.Msg.RunId, After: req.Msg.After,
		Include: includeSet(req.Msg.Include)}, st)
}

func (s *server) FollowSpaceEvents(ctx context.Context, req *connect.Request[pb.FollowSpaceEventsRequest], st *connect.ServerStream[pb.RunEvent]) error {
	if err := validIDs(req.Msg.SpaceId); err != nil {
		return err
	}
	return s.followEvents(ctx, req.Msg.SpaceId, runs.EventsQuery{Function: req.Msg.Function, After: req.Msg.After,
		Include: includeSet(req.Msg.Include)}, st)
}

func includeSet(kinds []string) map[string]bool {
	out := map[string]bool{}
	for _, k := range kinds {
		out[k] = true
	}
	return out
}

func (s *server) FollowRunResult(ctx context.Context, req *connect.Request[pb.FollowRunResultRequest], st *connect.ServerStream[pb.ResultEvent]) error {
	if err := validIDs(req.Msg.SpaceId, req.Msg.RunId); err != nil {
		return err
	}
	return s.followResult(ctx, req.Msg.SpaceId, req.Msg.RunId, int(req.Msg.After), req.Msg.IncludeProgress, st)
}

// followResult sends runs.FollowResult as ResultEvents.
func (s *server) followResult(ctx context.Context, spaceID, runID string, after int, withProgress bool, st *connect.ServerStream[pb.ResultEvent]) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	items, err := s.d.Runs.FollowResult(ctx, owner(ctx), spaceID, runID, after, withProgress)
	if err != nil {
		return s.toErr(err)
	}
	for {
		select {
		case ev, open := <-items:
			if !open {
				return ctx.Err()
			}
			out := &pb.ResultEvent{}
			switch ev.Kind {
			case "item":
				it := &pb.ResultItem{}
				if err := protojson.Unmarshal(ev.Data, it); err != nil {
					return s.toErr(err)
				}
				out.Event = &pb.ResultEvent_Item{Item: it}
			case "progress":
				p := &pb.RunEvent{}
				if err := protojson.Unmarshal(ev.Data, p); err != nil {
					return s.toErr(err)
				}
				out.Event = &pb.ResultEvent_Progress{Progress: p}
			case "result":
				r, err := conv(ev.Result, &pb.Result{})
				if err != nil {
					return s.toErr(err)
				}
				return st.Send(&pb.ResultEvent{Event: &pb.ResultEvent_Result{Result: r}})
			}
			if err := st.Send(out); err != nil {
				return err
			}
		case <-s.d.Shutdown:
			return errShutdown
		}
	}
}

func (s *server) GetCompletions(ctx context.Context, _ *connect.Request[pb.GetCompletionsRequest]) (*connect.Response[pb.Completions], error) {
	info, err := s.d.Runtime()
	if err != nil {
		return nil, s.toErr(err)
	}
	if info.Completions == nil || s.d.Completions == nil {
		return nil, connect.NewError(connect.CodeNotFound, completions.ErrNone)
	}
	rc, err := s.d.Completions(ctx, *info.Completions)
	if errors.Is(err, completions.ErrNone) {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	if err != nil {
		return nil, s.toErr(err)
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(io.LimitReader(rc, isolation.MaxCompletions+1))
	if err != nil {
		return nil, s.toErr(err)
	}
	return connect.NewResponse(&pb.Completions{Sha256: info.Completions.SHA256, IndexGzip: b}), nil
}
