// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package grpcapi

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/azbyluthfan/nbsandbox/gen/nbsandbox/v1"
	"github.com/azbyluthfan/nbsandbox/internal/audit"
	"github.com/azbyluthfan/nbsandbox/internal/runs"
	"github.com/azbyluthfan/nbsandbox/internal/spaces"
	"github.com/azbyluthfan/nbsandbox/internal/store"
)

func toAdminSpace(sp spaces.Space, withUsage bool) *pb.AdminSpace {
	s := toSpace(sp, withUsage)
	return &pb.AdminSpace{Id: s.Id, Name: s.Name, Owner: sp.Owner, CreatedAt: timestamppb.New(sp.CreatedAt),
		UpdatedAt: timestamppb.New(sp.UpdatedAt), Usage: s.Usage}
}

func (s *server) AdminListSpaces(ctx context.Context, req *connect.Request[pb.AdminListSpacesRequest]) (*connect.Response[pb.AdminListSpacesResponse], error) {
	cursor, limit, err := page(req.Msg.Cursor, req.Msg.Limit)
	if err != nil {
		return nil, err
	}
	list, next, err := s.d.Spaces.AdminList(ctx, req.Msg.Owner, cursor, limit)
	if err != nil {
		return nil, s.toErr(err)
	}
	out := &pb.AdminListSpacesResponse{NextCursor: next}
	for _, sp := range list {
		out.Items = append(out.Items, toAdminSpace(sp, false))
	}
	return connect.NewResponse(out), nil
}

func (s *server) AdminUpdateSpace(ctx context.Context, req *connect.Request[pb.AdminUpdateSpaceRequest]) (*connect.Response[pb.AdminSpace], error) {
	if err := validIDs(req.Msg.SpaceId); err != nil {
		return nil, err
	}
	sp, err := s.d.Spaces.AdminSetOwner(ctx, req.Msg.SpaceId, req.Msg.Owner)
	if err != nil {
		return nil, s.toErr(err)
	}
	s.d.Logger.Info("space owner changed by an administrator", "space_id", sp.ID, "owner", sp.Owner, "by", owner(ctx))
	return connect.NewResponse(toAdminSpace(sp, true)), nil
}

func (s *server) AdminDeleteSpace(ctx context.Context, req *connect.Request[pb.AdminDeleteSpaceRequest]) (*connect.Response[pb.AdminDeleteSpaceResponse], error) {
	if err := validIDs(req.Msg.SpaceId); err != nil {
		return nil, err
	}
	if err := s.d.Spaces.AdminDelete(ctx, req.Msg.SpaceId); err != nil {
		return nil, s.toErr(err)
	}
	s.d.Logger.Info("space deleted by an administrator", "space_id", req.Msg.SpaceId, "by", owner(ctx))
	return connect.NewResponse(&pb.AdminDeleteSpaceResponse{}), nil
}

func (s *server) adminRun(v runs.AdminView) (*pb.AdminRun, error) {
	r, err := s.run(v.View)
	if err != nil {
		return nil, err
	}
	return &pb.AdminRun{Run: r, Owner: v.Owner}, nil
}

func (s *server) AdminListRuns(ctx context.Context, req *connect.Request[pb.AdminListRunsRequest]) (*connect.Response[pb.AdminListRunsResponse], error) {
	cursor, limit, err := page(req.Msg.Cursor, req.Msg.Limit)
	if err != nil {
		return nil, err
	}
	list, next, err := s.d.Runs.AdminList(ctx, runs.AdminFilter{Status: req.Msg.Status, Owner: req.Msg.Owner, SpaceID: req.Msg.SpaceId}, cursor, limit)
	if err != nil {
		return nil, s.toErr(err)
	}
	out := &pb.AdminListRunsResponse{NextCursor: next}
	for _, v := range list {
		r, err := s.adminRun(v)
		if err != nil {
			return nil, s.toErr(err)
		}
		out.Items = append(out.Items, r)
	}
	return connect.NewResponse(out), nil
}

func (s *server) AdminCancelRun(ctx context.Context, req *connect.Request[pb.AdminCancelRunRequest]) (*connect.Response[pb.AdminRun], error) {
	if !spaces.ValidID(req.Msg.RunId) {
		return nil, invalid("run id must be a lowercase UUID")
	}
	v, err := s.d.Runs.AdminCancel(ctx, req.Msg.RunId, owner(ctx), req.Msg.Reason)
	if err != nil {
		return nil, s.toErr(err)
	}
	audit.SetSpace(ctx, v.SpaceID)
	audit.SetDetail(ctx, "owner "+v.Owner+"; reason "+req.Msg.Reason)
	s.d.Logger.Info("run cancelled by an administrator", "run_id", v.ID, "owner", v.Owner, "by", owner(ctx))
	out, err := s.adminRun(v)
	if err != nil {
		return nil, s.toErr(err)
	}
	return connect.NewResponse(out), nil
}

func toDrain(d store.Drain) *pb.DrainState {
	out := &pb.DrainState{Draining: d.Draining, Reason: d.Reason, QueuedRuns: int32(d.Queued), RunningRuns: int32(d.Running)} //nolint:gosec // counts of runs
	if d.Draining {
		out.By = d.By
	}
	if !d.Since.IsZero() {
		out.Since = timestamppb.New(d.Since)
	}
	return out
}

func (s *server) AdminGetDrain(ctx context.Context, _ *connect.Request[pb.AdminGetDrainRequest]) (*connect.Response[pb.DrainState], error) {
	d, err := s.d.Runs.DrainState(ctx)
	if err != nil {
		return nil, s.toErr(err)
	}
	return connect.NewResponse(toDrain(d)), nil
}

func (s *server) AdminSetDrain(ctx context.Context, req *connect.Request[pb.AdminSetDrainRequest]) (*connect.Response[pb.DrainState], error) {
	audit.SetDetail(ctx, fmt.Sprintf("draining %t; reason %s", req.Msg.Draining, req.Msg.Reason))
	d, err := s.d.Runs.SetDrain(ctx, req.Msg.Draining, req.Msg.Reason, owner(ctx))
	if err != nil {
		return nil, s.toErr(err)
	}
	return connect.NewResponse(toDrain(d)), nil
}

func (s *server) AdminListAudit(ctx context.Context, req *connect.Request[pb.AdminListAuditRequest]) (*connect.Response[pb.AdminListAuditResponse], error) {
	cursor, limit, err := page(req.Msg.Cursor, req.Msg.Limit)
	if err != nil {
		return nil, err
	}
	f := store.AuditFilter{Actor: req.Msg.Actor, SpaceID: req.Msg.SpaceId, Action: req.Msg.Action}
	if req.Msg.Since != nil {
		t := req.Msg.Since.AsTime()
		f.Since = &t
	}
	if req.Msg.Until != nil {
		t := req.Msg.Until.AsTime()
		f.Until = &t
	}
	rows, next, err := s.d.Audit.List(ctx, f, cursor, limit)
	if errors.Is(err, store.ErrBadCursor) {
		return nil, invalid("invalid cursor")
	}
	if err != nil {
		return nil, s.toErr(err)
	}
	out := &pb.AdminListAuditResponse{NextCursor: next}
	for _, e := range rows {
		out.Items = append(out.Items, &pb.AuditEvent{Id: strconv.FormatInt(e.ID, 10), At: timestamppb.New(e.At), Actor: e.Actor,
			Method: e.Method, Client: e.Client, Via: e.Via, Action: e.Action, SpaceId: e.SpaceID, Target: e.Target,
			Detail: e.Detail, Status: int32(e.Status), Address: e.Address}) //nolint:gosec // an HTTP status
	}
	return connect.NewResponse(out), nil
}
