// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package grpcapi

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/azbyluthfan/nbsandbox/gen/nbsandbox/v1"
	"github.com/azbyluthfan/nbsandbox/internal/audit"
	"github.com/azbyluthfan/nbsandbox/internal/connections"
	"github.com/azbyluthfan/nbsandbox/internal/spaces"
)

var errConnectionsOff = errors.New("connections are turned off on this deployment (connections.enabled)")

// connectionErr maps connection errors as the REST API does.
func (s *server) connectionErr(err error) error {
	var ie *connections.InvalidError
	switch {
	case errors.As(err, &ie):
		return connect.NewError(connect.CodeInvalidArgument, err)
	case errors.Is(err, connections.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, errors.New("not found"))
	case errors.Is(err, connections.ErrNameTaken):
		return connect.NewError(connect.CodeAlreadyExists, err)
	case errors.Is(err, connections.ErrInUse):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	}
	return s.toErr(err)
}

func (s *server) connectionsOn() error {
	if s.d.Connections == nil {
		return connect.NewError(connect.CodeNotFound, errConnectionsOff)
	}
	return nil
}

func (s *server) connectionID(id string) error {
	if err := s.connectionsOn(); err != nil {
		return err
	}
	if !spaces.ValidID(id) {
		return invalid("connection id must be a lowercase UUID")
	}
	return nil
}

func secretInput(in *pb.ConnectionSecretInput) connections.SecretInput {
	switch k := in.GetKind().(type) {
	case *pb.ConnectionSecretInput_Value:
		return connections.SecretInput{Value: &k.Value}
	case *pb.ConnectionSecretInput_Ref:
		return connections.SecretInput{Ref: &k.Ref}
	}
	return connections.SecretInput{None: true}
}

func toConnection(v connections.View) *pb.Connection {
	settings, _ := structpb.NewStruct(v.Settings)
	out := &pb.Connection{Id: v.ID, Name: v.Name, Type: v.Type, Settings: settings,
		Secret:    &pb.ConnectionSecret{Mode: v.Secret.Mode, Provider: v.Secret.Provider, Ref: v.Secret.Ref},
		CreatedAt: timestamppb.New(v.CreatedAt), UpdatedAt: timestamppb.New(v.UpdatedAt)}
	for _, u := range v.UsedBy {
		out.UsedBy = append(out.UsedBy, &pb.ConnectionUse{SpaceId: u.SpaceID, SpaceName: u.SpaceName, Environment: u.Environment, Name: u.Name})
	}
	return out
}

func toSpaceConnections(v connections.SpaceView) *pb.SpaceConnections {
	out := &pb.SpaceConnections{DefaultEnvironment: v.Default, Environments: map[string]*pb.BoundEnvironment{}}
	for env, names := range v.Environments {
		be := &pb.BoundEnvironment{Names: map[string]*pb.BoundConnection{}}
		for name, b := range names {
			be.Names[name] = &pb.BoundConnection{Id: b.ID, Name: b.Name, Type: b.Type}
		}
		out.Environments[env] = be
	}
	return out
}

// CreateConnection implements ConnectionService.
func (s *server) CreateConnection(ctx context.Context, req *connect.Request[pb.CreateConnectionRequest]) (*connect.Response[pb.Connection], error) {
	if err := s.connectionsOn(); err != nil {
		return nil, err
	}
	m := req.Msg
	audit.SetDetail(ctx, m.Name+" ("+m.Type+")")
	v, err := s.d.Connections.Create(ctx, owner(ctx), connections.Input{Name: m.Name, Type: m.Type,
		Settings: m.GetSettings().AsMap(), Secret: secretInput(m.Secret)})
	if err != nil {
		return nil, s.connectionErr(err)
	}
	audit.SetTarget(ctx, v.ID)
	return connect.NewResponse(toConnection(v)), nil
}

// ListConnections implements ConnectionService.
func (s *server) ListConnections(ctx context.Context, _ *connect.Request[pb.ListConnectionsRequest]) (*connect.Response[pb.ListConnectionsResponse], error) {
	if err := s.connectionsOn(); err != nil {
		return nil, err
	}
	list, err := s.d.Connections.List(ctx, owner(ctx))
	if err != nil {
		return nil, s.connectionErr(err)
	}
	out := &pb.ListConnectionsResponse{}
	for _, v := range list {
		out.Items = append(out.Items, toConnection(v))
	}
	return connect.NewResponse(out), nil
}

// GetConnection implements ConnectionService.
func (s *server) GetConnection(ctx context.Context, req *connect.Request[pb.GetConnectionRequest]) (*connect.Response[pb.Connection], error) {
	if err := s.connectionID(req.Msg.ConnectionId); err != nil {
		return nil, err
	}
	v, err := s.d.Connections.Get(ctx, owner(ctx), req.Msg.ConnectionId)
	if err != nil {
		return nil, s.connectionErr(err)
	}
	return connect.NewResponse(toConnection(v)), nil
}

// UpdateConnection implements ConnectionService.
func (s *server) UpdateConnection(ctx context.Context, req *connect.Request[pb.UpdateConnectionRequest]) (*connect.Response[pb.Connection], error) {
	m := req.Msg
	if err := s.connectionID(m.ConnectionId); err != nil {
		return nil, err
	}
	audit.SetTarget(ctx, m.ConnectionId)
	p := connections.Patch{Name: m.Name}
	if m.Settings != nil {
		p.Settings = m.Settings.AsMap()
	}
	if m.Secret != nil {
		in := secretInput(m.Secret)
		p.Secret = &in
		audit.SetDetail(ctx, "secret replaced")
	}
	v, err := s.d.Connections.Update(ctx, owner(ctx), m.ConnectionId, p)
	if err != nil {
		return nil, s.connectionErr(err)
	}
	return connect.NewResponse(toConnection(v)), nil
}

// DeleteConnection implements ConnectionService.
func (s *server) DeleteConnection(ctx context.Context, req *connect.Request[pb.GetConnectionRequest]) (*connect.Response[pb.DeleteConnectionResponse], error) {
	if err := s.connectionID(req.Msg.ConnectionId); err != nil {
		return nil, err
	}
	audit.SetTarget(ctx, req.Msg.ConnectionId)
	if err := s.d.Connections.Delete(ctx, owner(ctx), req.Msg.ConnectionId); err != nil {
		return nil, s.connectionErr(err)
	}
	return connect.NewResponse(&pb.DeleteConnectionResponse{}), nil
}

// GetSpaceConnections implements ConnectionService.
func (s *server) GetSpaceConnections(ctx context.Context, req *connect.Request[pb.GetSpaceConnectionsRequest]) (*connect.Response[pb.SpaceConnections], error) {
	if err := s.connectionsOn(); err != nil {
		return nil, err
	}
	if err := validIDs(req.Msg.SpaceId); err != nil {
		return nil, err
	}
	v, err := s.d.Connections.Space(ctx, owner(ctx), req.Msg.SpaceId)
	if err != nil {
		return nil, s.connectionErr(err)
	}
	return connect.NewResponse(toSpaceConnections(v)), nil
}

// SetSpaceConnections implements ConnectionService.
func (s *server) SetSpaceConnections(ctx context.Context, req *connect.Request[pb.SetSpaceConnectionsRequest]) (*connect.Response[pb.SpaceConnections], error) {
	if err := s.connectionsOn(); err != nil {
		return nil, err
	}
	m := req.Msg
	if err := validIDs(m.SpaceId); err != nil {
		return nil, err
	}
	in := connections.SpaceInput{Default: m.DefaultEnvironment, Environments: map[string]map[string]string{}}
	for env, b := range m.Environments {
		in.Environments[env] = map[string]string{}
		for name, conn := range b.GetNames() {
			in.Environments[env][name] = conn
		}
	}
	v, err := s.d.Connections.SetSpace(ctx, owner(ctx), m.SpaceId, in)
	if err != nil {
		return nil, s.connectionErr(err)
	}
	return connect.NewResponse(toSpaceConnections(v)), nil
}
