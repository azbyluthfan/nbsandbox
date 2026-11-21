// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package grpcapi

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/azbyluthfan/nbsandbox/gen/nbsandbox/v1"
	"github.com/azbyluthfan/nbsandbox/internal/audit"
	"github.com/azbyluthfan/nbsandbox/internal/sessions"
	"github.com/azbyluthfan/nbsandbox/internal/spaces"
	"github.com/azbyluthfan/nbsandbox/internal/store"
)

func toSession(s store.Session, admin bool) *pb.Session {
	out := &pb.Session{Id: s.ID, SpaceId: s.SpaceID, Notebook: s.Notebook, Mode: s.Mode, Status: s.Status,
		CreatedAt: timestamppb.New(s.CreatedAt), EndReason: s.EndReason, EndedBy: s.EndedBy}
	if !s.EndedAt.IsZero() {
		out.EndedAt = timestamppb.New(s.EndedAt)
	}
	if admin {
		out.Owner, out.Worker = s.Owner, s.Worker
	}
	return out
}

// sessionErr maps session errors as the REST API does.
func (s *server) sessionErr(err error) error {
	var (
		ie *sessions.InvalidError
		re *sessions.RefusedError
	)
	switch {
	case errors.As(err, &ie), errors.As(err, &re):
		return connect.NewError(connect.CodeInvalidArgument, err)
	case errors.Is(err, sessions.ErrDisabled):
		return connect.NewError(connect.CodeNotFound, err)
	case errors.Is(err, sessions.ErrLimit), errors.Is(err, sessions.ErrEnded):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	case errors.Is(err, sessions.ErrDraining), errors.Is(err, sessions.ErrUnavailable), errors.Is(err, sessions.ErrNotHere):
		return connect.NewError(connect.CodeUnavailable, err)
	}
	return s.toErr(err)
}

func (s *server) sessionIDs(spaceID, sessionID string) error {
	if !s.d.Sessions.Enabled() {
		return connect.NewError(connect.CodeNotFound, sessions.ErrDisabled)
	}
	if err := validIDs(spaceID); err != nil {
		return err
	}
	if sessionID != "" && !spaces.ValidID(sessionID) {
		return invalid("session id must be a lowercase UUID")
	}
	return nil
}

// OpenSession implements SessionService.
func (s *server) OpenSession(ctx context.Context, req *connect.Request[pb.OpenSessionRequest]) (*connect.Response[pb.Session], error) {
	if err := s.sessionIDs(req.Msg.SpaceId, ""); err != nil {
		return nil, err
	}
	sess, _, err := s.d.Sessions.Open(ctx, owner(ctx), req.Msg.SpaceId, req.Msg.Notebook, req.Msg.Mode)
	if err != nil {
		return nil, s.sessionErr(err)
	}
	audit.SetTarget(ctx, sess.ID)
	audit.SetDetail(ctx, sess.Notebook+" ("+sess.Mode+")")
	return connect.NewResponse(toSession(sess, false)), nil
}

// ListSessions implements SessionService.
func (s *server) ListSessions(ctx context.Context, req *connect.Request[pb.ListSessionsRequest]) (*connect.Response[pb.ListSessionsResponse], error) {
	if err := s.sessionIDs(req.Msg.SpaceId, ""); err != nil {
		return nil, err
	}
	list, err := s.d.Sessions.List(ctx, owner(ctx), req.Msg.SpaceId)
	if err != nil {
		return nil, s.sessionErr(err)
	}
	out := &pb.ListSessionsResponse{}
	for _, sess := range list {
		out.Items = append(out.Items, toSession(sess, false))
	}
	return connect.NewResponse(out), nil
}

// GetSession implements SessionService.
func (s *server) GetSession(ctx context.Context, req *connect.Request[pb.GetSessionRequest]) (*connect.Response[pb.Session], error) {
	if err := s.sessionIDs(req.Msg.SpaceId, req.Msg.SessionId); err != nil {
		return nil, err
	}
	sess, err := s.d.Sessions.Get(ctx, owner(ctx), req.Msg.SpaceId, req.Msg.SessionId)
	if err != nil {
		return nil, s.sessionErr(err)
	}
	return connect.NewResponse(toSession(sess, false)), nil
}

// EndSession implements SessionService.
func (s *server) EndSession(ctx context.Context, req *connect.Request[pb.GetSessionRequest]) (*connect.Response[pb.Session], error) {
	if err := s.sessionIDs(req.Msg.SpaceId, req.Msg.SessionId); err != nil {
		return nil, err
	}
	sess, err := s.d.Sessions.End(ctx, owner(ctx), req.Msg.SpaceId, req.Msg.SessionId, owner(ctx))
	if err != nil {
		return nil, s.sessionErr(err)
	}
	return connect.NewResponse(toSession(sess, false)), nil
}

// InterruptSession implements SessionService.
func (s *server) InterruptSession(ctx context.Context, req *connect.Request[pb.GetSessionRequest]) (*connect.Response[pb.SessionActionResponse], error) {
	return s.sessionAction(ctx, req.Msg, s.d.Sessions.Interrupt)
}

// RestartSession implements SessionService.
func (s *server) RestartSession(ctx context.Context, req *connect.Request[pb.GetSessionRequest]) (*connect.Response[pb.SessionActionResponse], error) {
	return s.sessionAction(ctx, req.Msg, s.d.Sessions.Restart)
}

func (s *server) sessionAction(ctx context.Context, msg *pb.GetSessionRequest,
	do func(ctx context.Context, owner, spaceID, id string) error) (*connect.Response[pb.SessionActionResponse], error) {
	if err := s.sessionIDs(msg.SpaceId, msg.SessionId); err != nil {
		return nil, err
	}
	if err := do(ctx, owner(ctx), msg.SpaceId, msg.SessionId); err != nil {
		return nil, s.sessionErr(err)
	}
	return connect.NewResponse(&pb.SessionActionResponse{Ok: true}), nil
}

// SyncSession implements SessionService.
func (s *server) SyncSession(ctx context.Context, req *connect.Request[pb.GetSessionRequest]) (*connect.Response[pb.SyncSessionResponse], error) {
	if err := s.sessionIDs(req.Msg.SpaceId, req.Msg.SessionId); err != nil {
		return nil, err
	}
	conflicts, err := s.d.Sessions.SyncFiles(ctx, owner(ctx), req.Msg.SpaceId, req.Msg.SessionId)
	if err != nil {
		return nil, s.sessionErr(err)
	}
	out := &pb.SyncSessionResponse{}
	for _, c := range conflicts {
		out.Conflicts = append(out.Conflicts, &pb.SyncConflict{Path: c.Path, SavedAs: c.SavedAs})
	}
	return connect.NewResponse(out), nil
}

// SessionChannel implements SessionService: the WebSocket's messages, over
// a bidirectional stream.
func (s *server) SessionChannel(ctx context.Context, stream *connect.BidiStream[pb.SessionChannelRequest, pb.SessionMessage]) error {
	first, err := stream.Receive()
	if err != nil {
		return err
	}
	if err := s.sessionIDs(first.SpaceId, first.SessionId); err != nil {
		return err
	}
	client, _, err := s.d.Sessions.Attach(ctx, owner(ctx), first.SpaceId, first.SessionId)
	if err != nil {
		return s.sessionErr(err)
	}
	defer client.Close()
	refusals := make(chan string, 16)
	send := func(js string) {
		if js == "" {
			return
		}
		if err := client.Send([]byte(js)); err != nil {
			select {
			case refusals <- refusalJSON(err):
			default:
			}
		}
	}
	send(first.Json)
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		for {
			m, err := stream.Receive()
			if err != nil {
				return
			}
			send(m.Json)
		}
	}()
	for {
		select {
		case msg, ok := <-client.Messages():
			if !ok {
				return nil
			}
			if err := stream.Send(&pb.SessionMessage{Json: string(msg)}); err != nil {
				return err
			}
		case js := <-refusals:
			if err := stream.Send(&pb.SessionMessage{Json: js}); err != nil {
				return err
			}
		case <-readDone:
			return nil
		case <-s.d.Shutdown:
			return connect.NewError(connect.CodeUnavailable, errors.New("nbsandboxd is restarting; reconnect in a moment"))
		case <-ctx.Done():
			return nil
		}
	}
}

func refusalJSON(err error) string {
	return `{"channel":"bridge","content":{"refused":true,"error":` + quoteJSON(err.Error()) + `}}`
}

// AdminListSessions implements AdminService.
func (s *server) AdminListSessions(ctx context.Context, _ *connect.Request[pb.AdminListSessionsRequest]) (*connect.Response[pb.ListSessionsResponse], error) {
	if !s.d.Sessions.Enabled() {
		return nil, connect.NewError(connect.CodeNotFound, sessions.ErrDisabled)
	}
	list, err := s.d.Sessions.All(ctx)
	if err != nil {
		return nil, s.sessionErr(err)
	}
	out := &pb.ListSessionsResponse{}
	for _, sess := range list {
		out.Items = append(out.Items, toSession(sess, true))
	}
	return connect.NewResponse(out), nil
}

// AdminEndSession implements AdminService.
func (s *server) AdminEndSession(ctx context.Context, req *connect.Request[pb.AdminEndSessionRequest]) (*connect.Response[pb.Session], error) {
	if !s.d.Sessions.Enabled() {
		return nil, connect.NewError(connect.CodeNotFound, sessions.ErrDisabled)
	}
	if !spaces.ValidID(req.Msg.SessionId) {
		return nil, invalid("session id must be a lowercase UUID")
	}
	reason := "ended by an administrator"
	if req.Msg.Reason != "" {
		reason += ": " + req.Msg.Reason
	}
	sess, err := s.d.Sessions.EndAny(ctx, req.Msg.SessionId, owner(ctx), reason)
	if err != nil {
		return nil, s.sessionErr(err)
	}
	audit.SetSpace(ctx, sess.SpaceID)
	audit.SetDetail(ctx, req.Msg.Reason)
	return connect.NewResponse(toSession(sess, true)), nil
}
