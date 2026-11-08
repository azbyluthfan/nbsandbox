// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"

	"github.com/azbyluthfan/nbsandbox/internal/auth"
)

// UIPrefix is where the web UI reaches the API, acting as {user}.
const UIPrefix = "/ui/api/as/{user}"

// NewUIHandler serves the optional local web UI:
// the static app, the list of usernames, and every authenticated /v1 route
// under /ui/api/as/{user}/v1/..., run as {user} without a password. It uses
// the same route table and checks as the main API, so acting as alice still
// gets 404 on bob's spaces; only how the user is chosen differs. It must only
// be served on its own, local-only listener.
func NewUIHandler(d Deps, static http.Handler) http.Handler {
	a := &API{d: d}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ui/api/users", func(w http.ResponseWriter, _ *http.Request) {
		WriteJSON(w, http.StatusOK, map[string]any{"users": d.Users.Names()})
	})
	for _, rt := range a.routes() {
		if !rt.Auth { // health and metrics stay on the main port only
			continue
		}
		mux.Handle(rt.Method+" "+UIPrefix+rt.Pattern, actAs(d.Users, recordUser(wrapRoute(rt, d))))
	}
	mux.HandleFunc("/ui/api/", notFound)
	mux.Handle("/", static)

	var h http.Handler = mux
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
