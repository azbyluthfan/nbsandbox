// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package grpcapi

import (
	"context"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/azbyluthfan/nbsandbox/gen/nbsandbox/v1"
	"github.com/azbyluthfan/nbsandbox/internal/spaces"
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
