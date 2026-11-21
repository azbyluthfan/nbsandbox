// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/audit"
	"github.com/azbyluthfan/nbsandbox/internal/runs"
	"github.com/azbyluthfan/nbsandbox/internal/spaces"
	"github.com/azbyluthfan/nbsandbox/internal/store"
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
	audit.SetDetail(r.Context(), "owner "+body.Owner)
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
	if err := a.endSpaceSessions(r, user(r)); err != nil {
		writeDomainError(w, r, err)
		return
	}
	if err := a.d.Spaces.AdminDelete(r.Context(), r.PathValue("id")); err != nil {
		writeDomainError(w, r, err)
		return
	}
	a.d.Logger.Info("space deleted by an administrator", "space_id", r.PathValue("id"), "by", user(r))
	w.WriteHeader(http.StatusNoContent)
}

// adminListRuns is GET /v1/admin/runs?status=&owner=&space=: every owner's
// runs, newest first, with the owner. Metadata only: no results.
func (a *API) adminListRuns(w http.ResponseWriter, r *http.Request) {
	cursor, limit, err := pageParams(r)
	if err != nil {
		writeRunError(w, r, err)
		return
	}
	q := r.URL.Query()
	list, next, err := a.d.Runs.AdminList(r.Context(), runs.AdminFilter{Status: q.Get("status"), Owner: q.Get("owner"), SpaceID: q.Get("space")}, cursor, limit)
	if err != nil {
		writeRunError(w, r, err)
		return
	}
	if list == nil {
		list = []runs.AdminView{}
	}
	WriteJSON(w, http.StatusOK, map[string]any{"items": list, "next_cursor": next})
}

// adminCancelRun is POST /v1/admin/runs/{run_id}/cancel {"reason": ""}:
// stop any owner's run.
func (a *API) adminCancelRun(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Reason string `json:"reason"`
	}
	if err := decodeJSON(r, &body, true); err != nil {
		writeRunError(w, r, err)
		return
	}
	v, err := a.d.Runs.AdminCancel(r.Context(), r.PathValue("run_id"), user(r), body.Reason)
	if err != nil {
		writeRunError(w, r, err)
		return
	}
	audit.SetSpace(r.Context(), v.SpaceID)
	audit.SetDetail(r.Context(), clip("owner "+v.Owner+"; reason "+body.Reason))
	a.d.Logger.Info("run cancelled by an administrator", "run_id", v.ID, "owner", v.Owner, "by", user(r))
	WriteJSON(w, http.StatusAccepted, v)
}

// drainJSON is the drain state.
type drainJSON struct {
	Draining bool       `json:"draining"`
	Reason   string     `json:"reason,omitempty"`
	By       string     `json:"by,omitempty"`
	Since    *time.Time `json:"since,omitempty"`
	Queued   int        `json:"queued_runs"`
	Running  int        `json:"running_runs"`
}

func toDrainJSON(d store.Drain) drainJSON {
	out := drainJSON{Draining: d.Draining, Reason: d.Reason, By: d.By, Queued: d.Queued, Running: d.Running}
	if !d.Since.IsZero() {
		t := d.Since.UTC()
		out.Since = &t
	}
	if !d.Draining {
		out.By = ""
	}
	return out
}

// adminGetDrain is GET /v1/admin/drain.
func (a *API) adminGetDrain(w http.ResponseWriter, r *http.Request) {
	d, err := a.d.Runs.DrainState(r.Context())
	if err != nil {
		writeRunError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, toDrainJSON(d))
}

// adminSetDrain is PUT /v1/admin/drain {"draining": true, "reason": ""}:
// no queued run starts until it is set back to false; running runs finish.
func (a *API) adminSetDrain(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Draining *bool  `json:"draining"`
		Reason   string `json:"reason"`
	}
	if err := decodeJSON(r, &body, false); err != nil {
		writeRunError(w, r, err)
		return
	}
	if body.Draining == nil {
		writeRunError(w, r, &spaces.InvalidError{Msg: "draining (true or false) is required"})
		return
	}
	audit.SetDetail(r.Context(), clip(fmt.Sprintf("draining %t; reason %s", *body.Draining, body.Reason)))
	d, err := a.d.Runs.SetDrain(r.Context(), *body.Draining, body.Reason, user(r))
	if err != nil {
		writeRunError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, toDrainJSON(d))
}

// auditJSON is an audit log row.
type auditJSON struct {
	ID      string    `json:"id"`
	At      time.Time `json:"at"`
	Actor   string    `json:"actor"`
	Method  string    `json:"method,omitempty"`
	Client  string    `json:"client,omitempty"`
	Via     string    `json:"via"`
	Action  string    `json:"action"`
	SpaceID string    `json:"space_id,omitempty"`
	Target  string    `json:"target,omitempty"`
	Detail  string    `json:"detail,omitempty"`
	Status  int       `json:"status"`
	Address string    `json:"address,omitempty"`
}

// adminListAudit is GET /v1/admin/audit?actor=&space=&action=&since=&until=:
// the audit log, newest first.
func (a *API) adminListAudit(w http.ResponseWriter, r *http.Request) {
	cursor, limit, err := pageParams(r)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	q := r.URL.Query()
	f := store.AuditFilter{Actor: q.Get("actor"), SpaceID: q.Get("space"), Action: q.Get("action")}
	for name, dst := range map[string]**time.Time{"since": &f.Since, "until": &f.Until} {
		if v := q.Get(name); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				writeDomainError(w, r, &spaces.InvalidError{Msg: name + " must be an RFC 3339 time"})
				return
			}
			*dst = &t
		}
	}
	rows, next, err := a.d.Audit.List(r.Context(), f, cursor, limit)
	if errors.Is(err, store.ErrBadCursor) {
		err = &spaces.InvalidError{Msg: "invalid cursor"}
	}
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	items := make([]auditJSON, 0, len(rows))
	for _, e := range rows {
		items = append(items, auditJSON{ID: strconv.FormatInt(e.ID, 10), At: e.At.UTC(), Actor: e.Actor, Method: e.Method, Client: e.Client,
			Via: e.Via, Action: e.Action, SpaceID: e.SpaceID, Target: e.Target, Detail: e.Detail, Status: e.Status, Address: e.Address})
	}
	WriteJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": next})
}
