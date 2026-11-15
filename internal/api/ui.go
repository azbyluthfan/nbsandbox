// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"

	"github.com/azbyluthfan/nbsandbox/internal/audit"
	"github.com/azbyluthfan/nbsandbox/internal/auth"
	"github.com/azbyluthfan/nbsandbox/internal/uiauth"
)

// UIPrefix is where the web UI reaches the API when it has no sign-in
// (ui.auth none), acting as {user}.
const UIPrefix = "/ui/api/as/{user}"

// UISessionPrefix is where the web UI reaches the API as its signed-in user.
const UISessionPrefix = "/ui/api"

// NewUIHandler serves the optional web UI: the static app and every
// authenticated /v1 route, with the same route table and checks as the main
// API; only how the caller is found differs.
//
// With sessions nil (ui.auth none) the routes are under
// /ui/api/as/{user}/v1/..., run as {user} without a password, plus the list
// of usernames: a local development tool, for a loopback listener only.
// Otherwise they are under /ui/api/v1/..., run as the session's caller, next
// to the sign-in pages.
func NewUIHandler(d Deps, static http.Handler, sessions *uiauth.Service) http.Handler {
	a := &API{d: d}
	mux := http.NewServeMux()
	if sessions == nil {
		mux.HandleFunc("GET /ui/api/users", func(w http.ResponseWriter, _ *http.Request) {
			WriteJSON(w, http.StatusOK, map[string]any{"users": d.Users.Names()})
		})
		mux.HandleFunc("GET /ui/api/session", func(w http.ResponseWriter, _ *http.Request) {
			WriteJSON(w, http.StatusOK, map[string]any{"mode": uiauth.ModeNone})
		})
	} else {
		sessions.Routes(mux)
		mux.Handle("GET /ui/api/session", sessions.Require(authFailed, uiauth.SessionInfo(sessions.Mode())))
	}
	for _, rt := range a.routes() {
		if !rt.Auth { // health and metrics stay on the main port only
			continue
		}
		h := recordUser(wrapRoute(rt, d))
		if sessions == nil {
			mux.Handle(rt.Method+" "+UIPrefix+rt.Pattern, actAs(d.Users, h))
		} else {
			mux.Handle(rt.Method+" "+UISessionPrefix+rt.Pattern, sessions.Require(authFailed, h))
		}
	}
	mux.HandleFunc("/ui/", notFound)
	mux.Handle("/", static)

	var h http.Handler = mux
	h = uiContext(d, h)
	h = recoverPanics(h)
	h = observe(d, h)
	return h
}

// actAs sets the user named in the path, if it exists.
func actAs(users *auth.Users, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := r.PathValue("user")
		if !users.Has(u) {
			WriteError(w, http.StatusNotFound, CodeNotFound, "no such user")
			return
		}
		next.ServeHTTP(w, r.WithContext(auth.WithUser(r.Context(), u)))
	})
}

// uiContext marks requests as the web UI's for the audit log, with the
// client's address (through trusted proxies, as on the API's port).
func uiContext(d Deps, next http.Handler) http.Handler {
	var trusted *auth.TrustedProxies
	if d.Auth != nil {
		trusted = d.Auth.TrustedProxies
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := audit.WithVia(r.Context(), "ui")
		ctx = auth.WithClientAddr(ctx, auth.ClientIP(ctx, r.RemoteAddr, r.Header, trusted))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
