// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

// Package grpcapi serves the nbsandbox API over gRPC, gRPC-Web and Connect
// (api/proto/nbsandbox/v1). Like the REST handlers, these only translate:
// every RPC calls the same spaces or runs method as its REST twin, needs the
// same scope (auth.Op), and maps the same errors.
package grpcapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	pb "github.com/azbyluthfan/nbsandbox/gen/nbsandbox/v1"
	"github.com/azbyluthfan/nbsandbox/gen/nbsandbox/v1/nbsandboxv1connect"
	"github.com/azbyluthfan/nbsandbox/internal/api"
	"github.com/azbyluthfan/nbsandbox/internal/auth"
	"github.com/azbyluthfan/nbsandbox/internal/completions"
	"github.com/azbyluthfan/nbsandbox/internal/runs"
	"github.com/azbyluthfan/nbsandbox/internal/spaces"
	"github.com/azbyluthfan/nbsandbox/internal/store"
)

// Deps are the handlers' collaborators: the same services the REST API uses.
type Deps struct {
	Auth        *auth.Authenticator
	Spaces      *spaces.Service
	Runs        *runs.Manager
	Runtime     func() (api.RuntimeInfo, error)
	Completions func(ctx context.Context, ref completions.Ref) (io.ReadCloser, error)
	Logger      *slog.Logger
	// Shutdown is closed when the server starts shutting down: open streams
	// end (callers resume where they were).
	Shutdown <-chan struct{}
}

// maxMessage bounds one message; uploads stream any number of them.
const maxMessage = 4 << 20

// chunkSize is the data per message of a download.
const chunkSize = 64 << 10

// Prefix is the path prefix of every RPC, for routing next to REST.
const Prefix = "/nbsandbox.v1."

// Handler serves the three services.
func Handler(d Deps) http.Handler {
	s := &server{d: d}
	opts := []connect.HandlerOption{
		connect.WithInterceptors(&authInterceptor{a: d.Auth}),
		connect.WithReadMaxBytes(maxMessage),
	}
	mux := http.NewServeMux()
	for _, h := range []func() (string, http.Handler){
		func() (string, http.Handler) { return nbsandboxv1connect.NewSpaceServiceHandler(s, opts...) },
		func() (string, http.Handler) { return nbsandboxv1connect.NewFunctionServiceHandler(s, opts...) },
		func() (string, http.Handler) { return nbsandboxv1connect.NewRunServiceHandler(s, opts...) },
		func() (string, http.Handler) { return nbsandboxv1connect.NewAdminServiceHandler(s, opts...) },
	} {
		path, handler := h()
		mux.Handle(path, handler)
	}
	return mux
}

type server struct {
	d Deps
	nbsandboxv1connect.UnimplementedSpaceServiceHandler
	nbsandboxv1connect.UnimplementedFunctionServiceHandler
	nbsandboxv1connect.UnimplementedRunServiceHandler
	nbsandboxv1connect.UnimplementedAdminServiceHandler
}

// Procedures lists every RPC's operation, from the service descriptors, for
// the test that both transports cover the same operations.
func Procedures() []auth.Op {
	var out []auth.Op
	svcs := pb.File_nbsandbox_v1_nbsandbox_proto.Services()
	for i := 0; i < svcs.Len(); i++ {
		ms := svcs.Get(i).Methods()
		for j := 0; j < ms.Len(); j++ {
			out = append(out, auth.Op(ms.Get(j).Name()))
		}
	}
	return out
}

// opOf is the operation of a procedure ("/nbsandbox.v1.RunService/GetRun").
func opOf(procedure string) auth.Op {
	return auth.Op(procedure[strings.LastIndex(procedure, "/")+1:])
}

// authInterceptor authenticates every call with its Authorization metadata
// and checks the operation's scope, as the REST middleware does.
type authInterceptor struct{ a *auth.Authenticator }

func (i *authInterceptor) check(ctx context.Context, procedure string, h http.Header) (context.Context, error) {
	op := opOf(procedure)
	scope := op.Scope()
	if scope == "" {
		return ctx, connect.NewError(connect.CodeUnimplemented, errors.New("unknown operation"))
	}
	p, err := i.a.Authenticate(ctx, h.Get("Authorization"))
	if err != nil {
		cerr := connect.NewError(connect.CodeUnauthenticated, errors.New(auth.FailureMessage(err)))
		for _, c := range i.a.Challenges(err) {
			cerr.Meta().Add("WWW-Authenticate", c)
		}
		return ctx, cerr
	}
	if !p.Has(scope) {
		cerr := connect.NewError(connect.CodePermissionDenied, errors.New("the access token lacks the scope "+scope))
		cerr.Meta().Set("WWW-Authenticate", auth.InsufficientScope(scope))
		return ctx, cerr
	}
	return auth.WithPrincipal(ctx, p), nil
}

func (i *authInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		ctx, err := i.check(ctx, req.Spec().Procedure, req.Header())
		if err != nil {
			return nil, err
		}
		return next(ctx, req)
	}
}

func (i *authInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (i *authInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		ctx, err := i.check(ctx, conn.Spec().Procedure, conn.RequestHeader())
		if err != nil {
			return err
		}
		return next(ctx, conn)
	}
}

func owner(ctx context.Context) string {
	u, _ := auth.User(ctx)
	return u
}

// conv fills m from v's JSON (its REST representation). A field the message
// lacks is an error, never silently dropped.
func conv[M proto.Message](v any, m M) (M, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return m, err
	}
	if err := protojson.Unmarshal(b, m); err != nil {
		return m, err
	}
	return m, nil
}

// rawJSON turns a protobuf JSON value into the JSON the services take.
func rawJSON(m proto.Message) (json.RawMessage, error) {
	if m == nil || !m.ProtoReflect().IsValid() {
		return nil, nil
	}
	b, err := protojson.Marshal(m)
	return json.RawMessage(b), err
}

func invalid(msg string) error { return connect.NewError(connect.CodeInvalidArgument, errors.New(msg)) }

// validIDs checks space and run IDs before anything else, as REST does.
func validIDs(spaceID string, runID ...string) error {
	if !spaces.ValidID(spaceID) {
		return invalid("space id must be a lowercase UUID")
	}
	for _, r := range runID {
		if !spaces.ValidID(r) {
			return invalid("run id must be a lowercase UUID")
		}
	}
	return nil
}

// toErr maps service errors to gRPC codes, like the REST error writers map
// them to HTTP statuses. Unknown errors are logged and hidden.
func (s *server) toErr(err error) error {
	if err == nil {
		return nil
	}
	var (
		ce   *connect.Error
		pe   *spaces.PathError
		ie   *spaces.InvalidError
		le   *spaces.LimitError
		ve   *spaces.VersionError
		cte  *runs.ContractError
		sa   *runs.ErrStillActive
		de   *runs.DependencyError
		code connect.Code
	)
	switch {
	case errors.As(err, &ce):
		return ce
	case errors.Is(err, context.Canceled):
		code = connect.CodeCanceled
	case errors.Is(err, spaces.ErrNotFound), errors.Is(err, store.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, errors.New("not found"))
	case errors.Is(err, spaces.ErrExists):
		code = connect.CodeAlreadyExists
	case errors.Is(err, spaces.ErrActiveRun), errors.Is(err, spaces.ErrNotEmpty), errors.As(err, &ve), errors.As(err, &sa),
		errors.Is(err, runs.ErrSpaceBusy), errors.Is(err, runs.ErrNotFinished), errors.Is(err, runs.ErrAlreadyFinished),
		errors.Is(err, runs.ErrFunctionBusy), errors.Is(err, runs.ErrIdempotencyMismatch), errors.As(err, &de):
		code = connect.CodeFailedPrecondition
	case errors.As(err, &pe), errors.As(err, &ie), errors.As(err, &cte):
		code = connect.CodeInvalidArgument
	case errors.As(err, &le):
		code = connect.CodeResourceExhausted
	case errors.Is(err, runs.ErrDockerUnavailable):
		code = connect.CodeUnavailable
	default:
		s.d.Logger.Error("grpc request failed", "error", err)
		return connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
	return connect.NewError(code, err)
}
