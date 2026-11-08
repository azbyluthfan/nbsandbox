// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"slices"
	"strings"
)

// Scopes a caller may hold. A route names the one it needs; a token carries
// them in its scp, scope or roles claim. Basic-auth users hold all of them.
const (
	ScopeRead             = "nbsandbox.read"              // read spaces, files, runs, results, streams
	ScopeSpacesWrite      = "nbsandbox.spaces.write"      // create, rename, delete spaces
	ScopeFilesWrite       = "nbsandbox.files.write"       // upload, edit, move, delete files; promote artifacts
	ScopeRuns             = "nbsandbox.runs"              // start, cancel, delete runs
	ScopeFunctionsInvoke  = "nbsandbox.functions.invoke"  // call functions
	ScopeFunctionsPublish = "nbsandbox.functions.publish" // publish and delete functions
	// ScopeAdmin manages every owner's spaces. Basic-auth users get it only
	// when users.yaml says admin: true.
	ScopeAdmin = "nbsandbox.admin"
)

// AllScopes lists every scope, for documentation and tests.
var AllScopes = []string{ScopeRead, ScopeSpacesWrite, ScopeFilesWrite, ScopeRuns, ScopeFunctionsInvoke, ScopeFunctionsPublish, ScopeAdmin}

// Principal is an authenticated caller.
type Principal struct {
	// ID owns spaces. A basic-auth user's ID is the username; a token's is
	// "<prefix>:<claim>[:<claim>...]" (e.g. entra:<tid>:<oid>). Usernames
	// can't contain ":", so the two never collide.
	ID string
	// Method is "basic" or "oidc".
	Method string
	// Client is the application that sent the token (azp, appid or
	// client_id), when it says; empty for basic auth.
	Client string
	// Name is for display only (the token's name or preferred_username).
	Name string

	scopes []string // sorted; nil with all = true
	all    bool     // every scope but admin (basic auth)
	admin  bool     // with all: admin too
}

// Has reports whether the caller holds scope.
func (p Principal) Has(scope string) bool {
	if p.all {
		return scope != ScopeAdmin || p.admin
	}
	_, found := slices.BinarySearch(p.scopes, scope)
	return found
}

// Scopes returns the caller's scopes, sorted (every scope for basic auth).
func (p Principal) Scopes() []string {
	if p.all {
		out := slices.DeleteFunc(slices.Clone(AllScopes), func(s string) bool { return s == ScopeAdmin && !p.admin })
		slices.Sort(out)
		return out
	}
	return slices.Clone(p.scopes)
}

// BasicPrincipal is a users.yaml user: every scope but admin.
func BasicPrincipal(username string) Principal {
	return Principal{ID: username, Method: "basic", Name: username, all: true}
}

// BasicAdmin is a users.yaml user marked admin: every scope.
func BasicAdmin(username string) Principal {
	p := BasicPrincipal(username)
	p.admin = true
	return p
}

func newTokenPrincipal(id, client, name string, scopes []string) Principal {
	s := make([]string, 0, len(scopes))
	for _, x := range scopes {
		if x = strings.TrimSpace(x); x != "" {
			s = append(s, x)
		}
	}
	slices.Sort(s)
	return Principal{ID: id, Method: "oidc", Client: client, Name: name, scopes: slices.Compact(s)}
}

type ctxKey struct{}

// From returns the authenticated caller stored by the middleware.
func From(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}

// WithPrincipal returns a context carrying an authenticated caller.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// User returns the authenticated caller's ID (the owner of their spaces).
func User(ctx context.Context) (string, bool) {
	p, ok := From(ctx)
	return p.ID, ok
}

// WithUser returns a context carrying a basic-auth user (for tests, the
// local web UI and internal callers).
func WithUser(ctx context.Context, username string) context.Context {
	return WithPrincipal(ctx, BasicPrincipal(username))
}
