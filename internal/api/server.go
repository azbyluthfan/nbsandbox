// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/auth"
	"github.com/azbyluthfan/nbsandbox/internal/completions"
	"github.com/azbyluthfan/nbsandbox/internal/obs"
	"github.com/azbyluthfan/nbsandbox/internal/runs"
	"github.com/azbyluthfan/nbsandbox/internal/spaces"
)

// DefaultMaxBody limits request bodies on routes that don't set their own.
const DefaultMaxBody = 1 << 20

// Deps are what the HTTP layer needs.
type Deps struct {
	// Auth authenticates /v1 requests; nil = basic auth against Users.
	Auth *auth.Authenticator
	// Users are the users.yaml users: basic auth, and the web UI's user list.
	Users   *auth.Users
	Health  *obs.Health
	Metrics *obs.Metrics
	Logger  *slog.Logger
	Spaces  *spaces.Service
	// MaxUpload bounds an upload request body (the per-space quota plus
	// multipart overhead); per-file and quota limits are enforced while
	// streaming.
	MaxUpload int64
	// MaxRunBody bounds a run or invoke request body (the parameters limit
	// plus room for the rest of the request).
	MaxRunBody int64
	Runs       *runs.Manager
	// Runtime describes the runner image and limits (GET /v1/runtime).
	Runtime func() (RuntimeInfo, error)
	// Completions opens a stored completion index (gzipped JSON); nil:
	// none is served.
	Completions func(ctx context.Context, ref completions.Ref) (io.ReadCloser, error)
	// Shutdown is closed when the server starts shutting down: open log
	// streams send a final event and end.
	Shutdown <-chan struct{}
	// StreamHeartbeat is the SSE keep-alive interval (default 15 s).
	StreamHeartbeat time.Duration
}

// NewHandler builds the full HTTP handler from the route table: operational
// endpoints without auth, and the /v1 API behind basic auth or bearer tokens.
func NewHandler(d Deps) http.Handler {
	a := &API{d: d}
	authn := d.Auth
	if authn == nil {
		authn = &auth.Authenticator{Users: d.Users}
	}
	mux := http.NewServeMux()
	for _, rt := range a.routes() {
		h := wrapRoute(rt, d)
		if rt.Auth {
			h = authn.Middleware(authFailed, recordUser(h))
		}
		mux.Handle(rt.Method+" "+rt.Pattern, h)
	}
	// Unknown /v1 paths still require auth, so they reveal nothing; then 404.
	mux.Handle("/v1/", authn.Middleware(authFailed, recordUser(http.HandlerFunc(notFound))))
	mux.HandleFunc("/", notFound)

	var h http.Handler = mux
	h = recoverPanics(h)
	h = observe(d, h)
	return h
}

// wrapRoute applies a route's own checks: scope, ID validation, ownership
// and body limit.
func wrapRoute(rt Route, d Deps) http.Handler {
	var h http.Handler = rt.Handler
	if rt.SpaceScoped {
		h = requireOwner(d, h)
	}
	if rt.RunScoped {
		h = requireValidRunID(h)
	}
	if rt.SpaceScoped || rt.AnySpace {
		h = requireValidSpaceID(h)
	}
	if rt.Auth {
		h = requireScope(rt.Scope(), h)
	}
	return limitBody(rt.MaxBody, h)
}

// requireScope refuses a caller whose token lacks the route's scope (403),
// before anything about the space is looked at: the answer depends only on
// the token, so it reveals nothing.
func requireScope(scope string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := auth.From(r.Context())
		if !p.Has(scope) {
			authFailed(w, r, auth.Forbidden(w, scope))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireOwner answers 404 for a space the caller doesn't own before the
// handler reads anything else (its body, query or headers), so another
// user's space looks the same whatever the request.
func requireOwner(d Deps, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if d.Spaces != nil {
			// Not tied to the request: a caller hanging up right after
			// sending a complete submission must not lose it.
			if err := d.Spaces.Owned(context.WithoutCancel(r.Context()), user(r), r.PathValue("id")); err != nil {
				writeDomainError(w, r, err)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func notFound(w http.ResponseWriter, _ *http.Request) {
	WriteError(w, http.StatusNotFound, CodeNotFound, "no such endpoint")
}

func authFailed(w http.ResponseWriter, _ *http.Request, f auth.Failure) {
	code := CodeUnauthorized
	if f.Status == http.StatusForbidden {
		code = CodeForbidden
	}
	WriteError(w, f.Status, code, f.Message)
}

func limitBody(limit int64, next http.Handler) http.Handler {
	if limit <= 0 {
		limit = DefaultMaxBody
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, limit)
		}
		next.ServeHTTP(w, r)
	})
}

func recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				handlePanic(w, r, v)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func handlePanic(w http.ResponseWriter, r *http.Request, v any) {
	if v == http.ErrAbortHandler { //nolint:errorlint // sentinel compared by identity, as net/http does
		panic(v)
	}
	obs.Logger(r.Context()).Error("panic in handler", "panic", v, "stack", string(debug.Stack()))
	WriteError(w, http.StatusInternalServerError, CodeInternal, "internal error")
}

// observe assigns a request ID, puts a request-scoped logger in the context,
// and records the access log and the request metric.
func observe(d Deps, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		id := r.Header.Get("X-Request-ID")
		if id == "" || len(id) > 64 {
			id = newRequestID()
		}
		w.Header().Set("X-Request-ID", id)
		logger := d.Logger.With("request_id", id)
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		info := &requestInfo{}
		ctx := context.WithValue(obs.WithLogger(r.Context(), logger), requestInfoKey{}, info)
		req := r.WithContext(ctx)

		next.ServeHTTP(rec, req)

		route := req.Pattern // set by the mux on the request it was given
		if route == "" {
			route = "unmatched"
		}
		d.Metrics.HTTPRequestsTotal.WithLabelValues(route, strconv.Itoa(rec.status)).Inc()
		user := info.user
		level := slog.LevelInfo
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" || r.URL.Path == "/metrics" {
			level = slog.LevelDebug // probes would drown the log
		}
		logger.Log(r.Context(), level, "http request",
			"method", r.Method, "path", r.URL.Path, "status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(), "user", user)
	})
}

// requestInfo carries facts learned deeper in the chain (the authenticated
// user) back up to the access log.
type requestInfo struct{ user string }

type requestInfoKey struct{}

func recordUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if info, ok := r.Context().Value(requestInfoKey{}).(*requestInfo); ok {
			info.user, _ = auth.User(r.Context())
		}
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wrote {
		s.status, s.wrote = code, true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	s.wrote = true
	return s.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach Flush and deadlines (SSE).
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func newRequestID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
