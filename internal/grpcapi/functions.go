// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package grpcapi

import (
	"context"

	"connectrpc.com/connect"

	pb "github.com/azbyluthfan/nbsandbox/gen/nbsandbox/v1"
	"github.com/azbyluthfan/nbsandbox/internal/contract"
	"github.com/azbyluthfan/nbsandbox/internal/runs"
)

func (s *server) ListFunctions(ctx context.Context, req *connect.Request[pb.ListFunctionsRequest]) (*connect.Response[pb.ListFunctionsResponse], error) {
	if err := validIDs(req.Msg.SpaceId); err != nil {
		return nil, err
	}
	list, err := s.d.Runs.Functions(ctx, owner(ctx), req.Msg.SpaceId)
	if err != nil {
		return nil, s.toErr(err)
	}
	out, err := conv(map[string]any{"items": list}, &pb.ListFunctionsResponse{})
	if err != nil {
		return nil, s.toErr(err)
	}
	return connect.NewResponse(out), nil
}

func (s *server) GetFunction(ctx context.Context, req *connect.Request[pb.GetFunctionRequest]) (*connect.Response[pb.Function], error) {
	if err := validIDs(req.Msg.SpaceId); err != nil {
		return nil, err
	}
	f, err := s.d.Runs.Function(ctx, owner(ctx), req.Msg.SpaceId, req.Msg.Name)
	if err != nil {
		return nil, s.toErr(err)
	}
	out, err := conv(f, &pb.Function{})
	if err != nil {
		return nil, s.toErr(err)
	}
	return connect.NewResponse(out), nil
}

func (s *server) DeleteFunction(ctx context.Context, req *connect.Request[pb.DeleteFunctionRequest]) (*connect.Response[pb.DeleteFunctionResponse], error) {
	if err := validIDs(req.Msg.SpaceId); err != nil {
		return nil, err
	}
	if err := s.d.Runs.DeleteFunction(ctx, owner(ctx), req.Msg.SpaceId, req.Msg.Name); err != nil {
		return nil, s.toErr(err)
	}
	return connect.NewResponse(&pb.DeleteFunctionResponse{}), nil
}

func (s *server) version(v runs.VersionView, err error) (*connect.Response[pb.FunctionVersion], error) {
	if err != nil {
		return nil, s.toErr(err)
	}
	out, err := conv(v, &pb.FunctionVersion{})
	if err != nil {
		return nil, s.toErr(err)
	}
	return connect.NewResponse(out), nil
}

func (s *server) PublishFunction(ctx context.Context, req *connect.Request[pb.PublishFunctionRequest]) (*connect.Response[pb.FunctionVersion], error) {
	if err := validIDs(req.Msg.SpaceId); err != nil {
		return nil, err
	}
	if req.Msg.Notebook == "" {
		return nil, invalid("notebook is required")
	}
	pr := runs.PublishRequest{Notebook: req.Msg.Notebook, Files: req.Msg.Files}
	if c := req.Msg.Contract; c != nil {
		in, err := rawJSON(c.Input)
		if err != nil {
			return nil, invalid("contract input: " + err.Error())
		}
		out, err := rawJSON(c.Output)
		if err != nil {
			return nil, invalid("contract output: " + err.Error())
		}
		pr.Contract = &contract.Spec{Input: in, Output: out}
	}
	return s.version(s.d.Runs.Publish(ctx, owner(ctx), req.Msg.SpaceId, req.Msg.Name, pr))
}

func (s *server) GetFunctionVersion(ctx context.Context, req *connect.Request[pb.GetFunctionVersionRequest]) (*connect.Response[pb.FunctionVersion], error) {
	if err := validIDs(req.Msg.SpaceId); err != nil {
		return nil, err
	}
	if req.Msg.Version < 0 {
		return nil, invalid("version must be positive (0 = the latest)")
	}
	return s.version(s.d.Runs.FunctionVersion(ctx, owner(ctx), req.Msg.SpaceId, req.Msg.Name, int(req.Msg.Version)))
}

func (s *server) GetFunctionContract(ctx context.Context, req *connect.Request[pb.GetFunctionContractRequest]) (*connect.Response[pb.Contract], error) {
	if err := validIDs(req.Msg.SpaceId); err != nil {
		return nil, err
	}
	v, err := s.d.Runs.FunctionVersion(ctx, owner(ctx), req.Msg.SpaceId, req.Msg.Name, 0)
	if err != nil {
		return nil, s.toErr(err)
	}
	out, err := conv(v.Contract, &pb.Contract{})
	if err != nil {
		return nil, s.toErr(err)
	}
	return connect.NewResponse(out), nil
}

func (s *server) InvokeFunction(ctx context.Context, req *connect.Request[pb.InvokeFunctionRequest]) (*connect.Response[pb.RunOrResult], error) {
	m := req.Msg
	if err := validIDs(m.SpaceId); err != nil {
		return nil, err
	}
	if m.Version < 0 {
		return nil, invalid("version must be positive (0 = the latest)")
	}
	wait, err := waitFor(m.WaitSeconds)
	if err != nil {
		return nil, err
	}
	params, err := rawJSON(m.Parameters)
	if err != nil {
		return nil, invalid("parameters: " + err.Error())
	}
	v, replay, err := s.d.Runs.Invoke(context.WithoutCancel(ctx), owner(ctx), m.SpaceId, m.Name, runs.InvokeRequest{
		Version: int(m.Version), Parameters: params, IdempotencyKey: m.IdempotencyKey,
	})
	if err != nil {
		return nil, s.toErr(err)
	}
	var out *pb.RunOrResult
	if wait > 0 {
		if out, err = s.answer(ctx, v.SpaceID, v.ID, wait); err != nil {
			return nil, err
		}
	} else {
		r, err := s.run(v)
		if err != nil {
			return nil, s.toErr(err)
		}
		out = &pb.RunOrResult{Answer: &pb.RunOrResult_Run{Run: r}}
	}
	out.Replay = replay
	return connect.NewResponse(out), nil
}

func (s *server) InvokeFunctionStream(ctx context.Context, req *connect.Request[pb.InvokeFunctionRequest], st *connect.ServerStream[pb.ResultEvent]) error {
	m := req.Msg
	if err := validIDs(m.SpaceId); err != nil {
		return err
	}
	if m.Version < 0 {
		return invalid("version must be positive (0 = the latest)")
	}
	params, err := rawJSON(m.Parameters)
	if err != nil {
		return invalid("parameters: " + err.Error())
	}
	v, _, err := s.d.Runs.Invoke(context.WithoutCancel(ctx), owner(ctx), m.SpaceId, m.Name, runs.InvokeRequest{
		Version: int(m.Version), Parameters: params, IdempotencyKey: m.IdempotencyKey,
	})
	if err != nil {
		return s.toErr(err)
	}
	return s.followResult(ctx, v.SpaceID, v.ID, 0, false, st)
}
