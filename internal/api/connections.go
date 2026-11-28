// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"net/http"

	"github.com/azbyluthfan/nbsandbox/internal/audit"
	"github.com/azbyluthfan/nbsandbox/internal/connections"
	"github.com/azbyluthfan/nbsandbox/internal/spaces"
)

// errConnectionsOff answers every connection route while they are turned off.
var errConnectionsOff = errors.New("connections are turned off on this deployment (connections.enabled)")

func requireValidConnectionID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !spaces.ValidID(r.PathValue("connection_id")) {
			WriteError(w, http.StatusBadRequest, CodeBadRequest, "connection id must be a lowercase UUID")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *API) connectionsOn(w http.ResponseWriter) bool {
	if a.d.Connections == nil {
		WriteError(w, http.StatusNotFound, CodeNotFound, errConnectionsOff.Error())
		return false
	}
	return true
}

func writeConnectionError(w http.ResponseWriter, r *http.Request, err error) {
	var ie *connections.InvalidError
	switch {
	case errors.As(err, &ie):
		WriteError(w, http.StatusBadRequest, CodeBadRequest, err.Error())
	case errors.Is(err, connections.ErrNotFound):
		WriteError(w, http.StatusNotFound, CodeNotFound, "not found")
	case errors.Is(err, connections.ErrNameTaken), errors.Is(err, connections.ErrInUse):
		WriteError(w, http.StatusConflict, CodeConflict, err.Error())
	default:
		writeDomainError(w, r, err)
	}
}

// secretBody is a credential in a request: its value (stored by nbsandbox),
// a reference to an existing secret, or none. It is never in a response.
type secretBody struct {
	Value *string `json:"value"`
	Ref   *string `json:"ref"`
	None  bool    `json:"none"`
}

func (b *secretBody) input() connections.SecretInput {
	if b == nil {
		return connections.SecretInput{None: true}
	}
	return connections.SecretInput{Value: b.Value, Ref: b.Ref, None: b.None}
}

// createConnection is POST /v1/connections.
func (a *API) createConnection(w http.ResponseWriter, r *http.Request) {
	if !a.connectionsOn(w) {
		return
	}
	var body struct {
		Name     string         `json:"name"`
		Type     string         `json:"type"`
		Settings map[string]any `json:"settings"`
		Secret   *secretBody    `json:"secret"`
	}
	if err := decodeJSON(r, &body, false); err != nil {
		writeConnectionError(w, r, err)
		return
	}
	in := connections.Input{Name: body.Name, Type: body.Type, Settings: body.Settings, Secret: body.Secret.input()}
	if body.Secret != nil && body.Secret.Value == nil && body.Secret.Ref == nil {
		in.Secret.None = true
	}
	audit.SetDetail(r.Context(), body.Name+" ("+body.Type+")")
	v, err := a.d.Connections.Create(r.Context(), user(r), in)
	if err != nil {
		writeConnectionError(w, r, err)
		return
	}
	audit.SetTarget(r.Context(), v.ID)
	w.Header().Set("Location", "/v1/connections/"+v.ID)
	WriteJSON(w, http.StatusCreated, v)
}

// listConnections is GET /v1/connections: the caller's connections.
func (a *API) listConnections(w http.ResponseWriter, r *http.Request) {
	if !a.connectionsOn(w) {
		return
	}
	list, err := a.d.Connections.List(r.Context(), user(r))
	if err != nil {
		writeConnectionError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"items": list})
}

// getConnection is GET /v1/connections/{connection_id}, with where it is used.
func (a *API) getConnection(w http.ResponseWriter, r *http.Request) {
	if !a.connectionsOn(w) {
		return
	}
	v, err := a.d.Connections.Get(r.Context(), user(r), r.PathValue("connection_id"))
	if err != nil {
		writeConnectionError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, v)
}

// updateConnection is PATCH /v1/connections/{connection_id}: rename,
// replace the settings, or replace the credential.
func (a *API) updateConnection(w http.ResponseWriter, r *http.Request) {
	if !a.connectionsOn(w) {
		return
	}
	var body struct {
		Name     *string        `json:"name"`
		Settings map[string]any `json:"settings"`
		Secret   *secretBody    `json:"secret"`
	}
	if err := decodeJSON(r, &body, false); err != nil {
		writeConnectionError(w, r, err)
		return
	}
	p := connections.Patch{Name: body.Name, Settings: body.Settings}
	if body.Secret != nil {
		in := body.Secret.input()
		p.Secret = &in
		audit.SetDetail(r.Context(), "secret replaced")
	}
	v, err := a.d.Connections.Update(r.Context(), user(r), r.PathValue("connection_id"), p)
	if err != nil {
		writeConnectionError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, v)
}

// deleteConnection is DELETE /v1/connections/{connection_id}; refused
// (409) while a space binds it.
func (a *API) deleteConnection(w http.ResponseWriter, r *http.Request) {
	if !a.connectionsOn(w) {
		return
	}
	if err := a.d.Connections.Delete(r.Context(), user(r), r.PathValue("connection_id")); err != nil {
		writeConnectionError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// spaceConnectionsBody is what names mean in a space.
type spaceConnectionsBody struct {
	Default      string                       `json:"default_environment"`
	Environments map[string]map[string]string `json:"environments"`
}

// getSpaceConnections is GET /v1/spaces/{id}/connections.
func (a *API) getSpaceConnections(w http.ResponseWriter, r *http.Request) {
	if !a.connectionsOn(w) {
		return
	}
	v, err := a.d.Connections.Space(r.Context(), user(r), r.PathValue("id"))
	if err != nil {
		writeConnectionError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, v)
}

// getSpaceNames is GET /v1/spaces/{id}/names: what the space's owners
// declare, and which names each member has bound.
func (a *API) getSpaceNames(w http.ResponseWriter, r *http.Request) {
	if !a.connectionsOn(w) {
		return
	}
	v, err := a.d.Connections.Names(r.Context(), user(r), r.PathValue("id"))
	if err != nil {
		writeConnectionError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, v)
}

// setSpaceNames is PUT /v1/spaces/{id}/names (owners).
func (a *API) setSpaceNames(w http.ResponseWriter, r *http.Request) {
	if !a.connectionsOn(w) {
		return
	}
	var body struct {
		Default      string            `json:"default_environment"`
		Environments []string          `json:"environments"`
		Names        map[string]string `json:"names"`
	}
	if err := decodeJSON(r, &body, false); err != nil {
		writeConnectionError(w, r, err)
		return
	}
	v, err := a.d.Connections.SetNames(r.Context(), user(r), r.PathValue("id"),
		connections.NamesInput{Default: body.Default, Environments: body.Environments, Names: body.Names})
	if err != nil {
		writeConnectionError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, v)
}

// setSpaceConnections is PUT /v1/spaces/{id}/connections: replaces the
// caller's bindings (and, from an owner with default_environment, the
// space's declarations).
func (a *API) setSpaceConnections(w http.ResponseWriter, r *http.Request) {
	if !a.connectionsOn(w) {
		return
	}
	var body spaceConnectionsBody
	if err := decodeJSON(r, &body, false); err != nil {
		writeConnectionError(w, r, err)
		return
	}
	v, err := a.d.Connections.SetSpace(r.Context(), user(r), r.PathValue("id"),
		connections.SpaceInput{Default: body.Default, Environments: body.Environments})
	if err != nil {
		writeConnectionError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, v)
}
