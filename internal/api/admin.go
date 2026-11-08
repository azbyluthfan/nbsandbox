// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"

	"github.com/azbyluthfan/nbsandbox/internal/spaces"
)

// adminSpaceJSON is a space with its owner, for administrators.
type adminSpaceJSON struct {
	spaceJSON
	Owner string `json:"owner"`
}

func toAdminSpaceJSON(sp spaces.Space, withUsage bool) adminSpaceJSON {
	return adminSpaceJSON{spaceJSON: toSpaceJSON(sp, withUsage), Owner: sp.Owner}
}

// adminListSpaces is GET /v1/admin/spaces: every owner's spaces, or
// ?owner=<caller ID>'s, newest first.
func (a *API) adminListSpaces(w http.ResponseWriter, r *http.Request) {
	cursor, limit, err := pageParams(r)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	list, next, err := a.d.Spaces.AdminList(r.Context(), r.URL.Query().Get("owner"), cursor, limit)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	items := make([]adminSpaceJSON, 0, len(list))
	for _, sp := range list {
		items = append(items, toAdminSpaceJSON(sp, false))
	}
	WriteJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": next})
}

// adminUpdateSpace is PATCH /v1/admin/spaces/{id}: {"owner": "<caller ID>"}
// gives the space to another owner.
func (a *API) adminUpdateSpace(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Owner string `json:"owner"`
	}
	if err := decodeJSON(r, &body, false); err != nil {
		writeDomainError(w, r, err)
		return
	}
	sp, err := a.d.Spaces.AdminSetOwner(r.Context(), r.PathValue("id"), body.Owner)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	a.d.Logger.Info("space owner changed by an administrator", "space_id", sp.ID, "owner", sp.Owner, "by", user(r))
	WriteJSON(w, http.StatusOK, toAdminSpaceJSON(sp, true))
}

// adminDeleteSpace is DELETE /v1/admin/spaces/{id}: any owner's space.
func (a *API) adminDeleteSpace(w http.ResponseWriter, r *http.Request) {
	if err := a.d.Spaces.AdminDelete(r.Context(), r.PathValue("id")); err != nil {
		writeDomainError(w, r, err)
		return
	}
	a.d.Logger.Info("space deleted by an administrator", "space_id", r.PathValue("id"), "by", user(r))
	w.WriteHeader(http.StatusNoContent)
}
