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
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	pb "github.com/azbyluthfan/nbsandbox/gen/nbsandbox/v1"
	"github.com/azbyluthfan/nbsandbox/gen/nbsandbox/v1/nbsandboxv1connect"
	"github.com/azbyluthfan/nbsandbox/internal/api"
	"github.com/azbyluthfan/nbsandbox/internal/audit"
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
	// Audit records who did what (nil: nothing is recorded).
	Audit *audit.Recorder
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
		connect.WithInterceptors(&authInterceptor{a: d.Auth, rec: d.Audit}),
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
type authInterceptor struct {
	a   *auth.Authenticator
	rec *audit.Recorder
}

func (i *authInterceptor) check(ctx context.Context, procedure string, h http.Header, peer string) (context.Context, error) {
	op := opOf(procedure)
	scope := op.Scope()
	if scope == "" {
		return ctx, connect.NewError(connect.CodeUnimplemented, errors.New("unknown operation"))
	}
	ctx = auth.WithClientAddr(ctx, auth.ClientIP(ctx, peer, h, i.a.TrustedProxies))
	p, err := i.a.Authenticate(ctx, h.Get("Authorization"))
	if wait, limited := auth.IsTooManyAttempts(err); limited {
		cerr := connect.NewError(connect.CodeResourceExhausted, errors.New(auth.FailureMessage(err)))
		cerr.Meta().Set("Retry-After", strconv.Itoa(int(wait.Seconds())))
		return ctx, cerr
	}
	if err != nil {
		cerr := connect.NewError(connect.CodeUnauthenticated, errors.New(auth.FailureMessage(err)))
		for _, c := range i.a.Challenges(err) {
			cerr.Meta().Add("WWW-Authenticate", c)
		}
		return ctx, cerr
	}
	ctx = auth.WithPrincipal(ctx, p)
	if !p.Has(scope) {
		cerr := connect.NewError(connect.CodePermissionDenied, errors.New("the access token lacks the scope "+scope))
		cerr.Meta().Set("WWW-Authenticate", auth.InsufficientScope(scope))
		return ctx, cerr
	}
	return ctx, nil
}

// record writes the audit row of an audited operation that has finished
// (or was refused after the caller was known).
func (i *authInterceptor) record(ctx context.Context, procedure string, msg any, err error) {
	op := opOf(procedure)
	if _, known := auth.From(ctx); !known || !i.rec.Audited(op) {
		return
	}
	space, target := targetOf(msg)
	i.rec.Record(ctx, audit.Entry{Action: string(op), SpaceID: space, Target: target, Status: httpStatus(err)})
}

// targetOf reads what a request names, as the REST path does: its space,
// and a run (and artifact), a file or a function (and version).
func targetOf(msg any) (space, target string) {
	m, ok := msg.(proto.Message)
	if !ok || m == nil {
		return "", ""
	}
	r := m.ProtoReflect()
	// A streaming upload or write names its file in a "target" message.
	if fd := r.Descriptor().Fields().ByName("target"); fd != nil && fd.Kind() == protoreflect.MessageKind && r.Has(fd) {
		r = r.Get(fd).Message()
	}
	get := func(name string) string {
		fd := r.Descriptor().Fields().ByName(protoreflect.Name(name))
		if fd == nil || !r.Has(fd) {
			return ""
		}
		return fmt.Sprint(r.Get(fd).Interface())
	}
	run, path, name, version := get("run_id"), get("path"), get("name"), get("version")
	switch {
	case run != "" && path != "":
		target = run + "/" + path
	case run != "":
		target = run
	case path != "":
		target = path
	case name != "" && version != "":
		target = name + "@" + version
	default:
		target = name
	}
	return get("space_id"), target
}

// httpStatus maps a call's outcome to the HTTP status REST would give.
func httpStatus(err error) int {
	if err == nil {
		return http.StatusOK
	}
	switch connect.CodeOf(err) {
	case connect.CodeInvalidArgument, connect.CodeOutOfRange:
		return http.StatusBadRequest
	case connect.CodeUnauthenticated:
		return http.StatusUnauthorized
	case connect.CodePermissionDenied:
		return http.StatusForbidden
	case connect.CodeNotFound:
		return http.StatusNotFound
	case connect.CodeAlreadyExists, connect.CodeAborted, connect.CodeFailedPrecondition:
		return http.StatusConflict
	case connect.CodeResourceExhausted:
		return http.StatusTooManyRequests
	case connect.CodeCanceled:
		return 499 // the client went away
	case connect.CodeUnavailable:
		return http.StatusServiceUnavailable
	case connect.CodeDeadlineExceeded:
		return http.StatusGatewayTimeout
	case connect.CodeUnimplemented:
		return http.StatusNotImplemented
	default:
		return http.StatusInternalServerError
	}
}

func (i *authInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		ctx = audit.WithNote(audit.WithVia(ctx, "grpc"))
		ctx, err := i.check(ctx, req.Spec().Procedure, req.Header(), req.Peer().Addr)
		if err != nil {
			i.record(ctx, req.Spec().Procedure, req.Any(), err)
			return nil, err
		}
		resp, err := next(ctx, req)
		i.record(ctx, req.Spec().Procedure, req.Any(), err)
		return resp, err
	}
}

func (i *authInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (i *authInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		ctx = audit.WithNote(audit.WithVia(ctx, "grpc"))
		ctx, err := i.check(ctx, conn.Spec().Procedure, conn.RequestHeader(), conn.Peer().Addr)
		if err != nil {
			i.record(ctx, conn.Spec().Procedure, nil, err)
			return err
		}
		if !i.rec.Audited(opOf(conn.Spec().Procedure)) {
			return next(ctx, conn)
		}
		// The request's first message says what the call is about.
		c := &firstMessage{StreamingHandlerConn: conn}
		err = next(ctx, c)
		i.record(ctx, conn.Spec().Procedure, c.first, err)
		return err
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

// firstMessage keeps the first message a streaming call receives.
type firstMessage struct {
	connect.StreamingHandlerConn
	first any
}

func (c *firstMessage) Receive(m any) error {
	err := c.StreamingHandlerConn.Receive(m)
	if err == nil && c.first == nil {
		c.first = m
	}
	return err
}

// clip keeps an audit detail under 1 KiB.
func clip(s string) string {
	const maxDetail = 1024
	if len(s) <= maxDetail {
		return s
	}
	return s[:maxDetail-3] + "..."
}
