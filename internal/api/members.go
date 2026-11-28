// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/audit"
	"github.com/azbyluthfan/nbsandbox/internal/spaces"
)

// Shared spaces (docs/plan-shared-spaces.md): who has which role.

type memberJSON struct {
	Principal string     `json:"principal"`
	Role      string     `json:"role"`
	Creator   bool       `json:"creator,omitempty"`
	GrantedBy string     `json:"granted_by,omitempty"`
	GrantedAt *time.Time `json:"granted_at,omitempty"`
}

func toMemberJSON(m spaces.Member) memberJSON {
	out := memberJSON{Principal: m.Principal, Role: string(m.Role), Creator: m.Creator, GrantedBy: m.GrantedBy}
	if !m.GrantedAt.IsZero() {
		t := m.GrantedAt.UTC()
		out.GrantedAt = &t
	}
	return out
}

// listMembers is GET /v1/spaces/{id}/members.
func (a *API) listMembers(w http.ResponseWriter, r *http.Request) {
	ms, err := a.d.Spaces.Members(r.Context(), user(r), r.PathValue("id"))
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	items := make([]memberJSON, 0, len(ms))
	for _, m := range ms {
		items = append(items, toMemberJSON(m))
	}
	WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

// putMember is PUT /v1/spaces/{id}/members/{principal}.
func (a *API) putMember(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Role string `json:"role"`
	}
	if err := decodeJSON(r, &body, false); err != nil {
		writeDomainError(w, r, err)
		return
	}
	principal := r.PathValue("principal")
	audit.SetDetail(r.Context(), principal+" "+body.Role)
	m, err := a.d.Spaces.PutMember(r.Context(), user(r), r.PathValue("id"), principal, spaces.Role(body.Role))
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, toMemberJSON(m))
}

// deleteMember is DELETE /v1/spaces/{id}/members/{principal}.
func (a *API) deleteMember(w http.ResponseWriter, r *http.Request) {
	principal := r.PathValue("principal")
	audit.SetDetail(r.Context(), principal)
	if err := a.d.Spaces.RemoveMember(r.Context(), user(r), r.PathValue("id"), principal); err != nil {
		writeDomainError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// transferSpace is POST /v1/spaces/{id}/transfer.
func (a *API) transferSpace(w http.ResponseWriter, r *http.Request) {
	var body struct {
		To string `json:"to"`
	}
	if err := decodeJSON(r, &body, false); err != nil {
		writeDomainError(w, r, err)
		return
	}
	audit.SetDetail(r.Context(), "to "+body.To)
	sp, err := a.d.Spaces.Transfer(r.Context(), user(r), r.PathValue("id"), body.To)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, toSpaceJSON(sp, true))
}
