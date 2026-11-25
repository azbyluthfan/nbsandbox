// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/gorilla/websocket"

	"github.com/azbyluthfan/nbsandbox/internal/audit"
	"github.com/azbyluthfan/nbsandbox/internal/obs"
	"github.com/azbyluthfan/nbsandbox/internal/sessions"
	"github.com/azbyluthfan/nbsandbox/internal/spaces"
	"github.com/azbyluthfan/nbsandbox/internal/store"
)

// SessionView is a session as the API shows it.
type SessionView struct {
	ID        string     `json:"id"`
	SpaceID   string     `json:"space_id"`
	Notebook  string     `json:"notebook"`
	Mode      string     `json:"mode"`   // read-write or read-only
	Status    string     `json:"status"` // starting, ready or ended
	CreatedAt time.Time  `json:"created_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	EndReason string     `json:"end_reason,omitempty"`
	EndedBy   string     `json:"ended_by,omitempty"`
	// Environment is the space environment the session reads its names in.
	Environment string `json:"environment,omitempty"`
	// Administrators also see who owns it and where it runs.
	Owner  string `json:"owner,omitempty"`
	Worker string `json:"worker,omitempty"`
}

func sessionView(s store.Session, admin bool) SessionView {
	v := SessionView{ID: s.ID, SpaceID: s.SpaceID, Notebook: s.Notebook, Mode: s.Mode, Status: s.Status,
		CreatedAt: s.CreatedAt.UTC(), EndReason: s.EndReason, EndedBy: s.EndedBy, Environment: s.Environment}
	if !s.EndedAt.IsZero() {
		t := s.EndedAt.UTC()
		v.EndedAt = &t
	}
	if admin {
		v.Owner, v.Worker = s.Owner, s.Worker
	}
	return v
}

// requireValidSessionID rejects a malformed {session_id} with 400.
func requireValidSessionID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !spaces.ValidID(r.PathValue("session_id")) {
			WriteError(w, http.StatusBadRequest, CodeBadRequest, "session id must be a lowercase UUID")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// writeSessionError maps session errors, then falls back to the space
// errors.
func writeSessionError(w http.ResponseWriter, r *http.Request, err error) {
	var (
		ie *sessions.InvalidError
		re *sessions.RefusedError
	)
	switch {
	case errors.Is(err, store.ErrNotFound):
		WriteError(w, http.StatusNotFound, CodeNotFound, "not found")
	case errors.As(err, &ie), errors.As(err, &re):
		WriteError(w, http.StatusBadRequest, CodeBadRequest, err.Error())
	case errors.Is(err, sessions.ErrDisabled):
		WriteError(w, http.StatusNotFound, CodeNotFound, err.Error())
	case errors.Is(err, sessions.ErrLimit), errors.Is(err, sessions.ErrEnded):
		WriteError(w, http.StatusConflict, CodeConflict, err.Error())
	case errors.Is(err, sessions.ErrDraining), errors.Is(err, sessions.ErrUnavailable), errors.Is(err, sessions.ErrNotHere):
		w.Header().Set("Retry-After", "30")
		WriteError(w, http.StatusServiceUnavailable, CodeSessionsUnavailable, err.Error())
	default:
		writeDomainError(w, r, err)
	}
}

func (a *API) sessionsOn(w http.ResponseWriter) bool {
	if !a.d.Sessions.Enabled() {
		WriteError(w, http.StatusNotFound, CodeNotFound, sessions.ErrDisabled.Error())
		return false
	}
	return true
}

// openSession is POST /v1/spaces/{id}/sessions: the notebook's open
// session (200), or a new one (201, starting).
func (a *API) openSession(w http.ResponseWriter, r *http.Request) {
	if !a.sessionsOn(w) {
		return
	}
	var body struct {
		Notebook    string `json:"notebook"`
		Mode        string `json:"mode"`
		Environment string `json:"environment"`
	}
	if err := decodeJSON(r, &body, false); err != nil {
		writeSessionError(w, r, err)
		return
	}
	s, created, err := a.d.Sessions.Open(r.Context(), user(r), r.PathValue("id"), body.Notebook, body.Mode, body.Environment)
	if err != nil {
		writeSessionError(w, r, err)
		return
	}
	audit.SetTarget(r.Context(), s.ID)
	audit.SetDetail(r.Context(), s.Notebook+" ("+s.Mode+")")
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	w.Header().Set("Location", "/v1/spaces/"+s.SpaceID+"/sessions/"+s.ID)
	WriteJSON(w, status, sessionView(s, false))
}

// listSessions is GET /v1/spaces/{id}/sessions: the space's open sessions.
func (a *API) listSessions(w http.ResponseWriter, r *http.Request) {
	if !a.sessionsOn(w) {
		return
	}
	list, err := a.d.Sessions.List(r.Context(), user(r), r.PathValue("id"))
	if err != nil {
		writeSessionError(w, r, err)
		return
	}
	items := make([]SessionView, 0, len(list))
	for _, s := range list {
		items = append(items, sessionView(s, false))
	}
	WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

// getSession is GET /v1/spaces/{id}/sessions/{session_id}, open or ended.
func (a *API) getSession(w http.ResponseWriter, r *http.Request) {
	if !a.sessionsOn(w) {
		return
	}
	s, err := a.d.Sessions.Get(r.Context(), user(r), r.PathValue("id"), r.PathValue("session_id"))
	if err != nil {
		writeSessionError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, sessionView(s, false))
}

// endSession is DELETE /v1/spaces/{id}/sessions/{session_id}: its kernel
// stops, and a read-write session's file changes are written back.
func (a *API) endSession(w http.ResponseWriter, r *http.Request) {
	if !a.sessionsOn(w) {
		return
	}
	s, err := a.d.Sessions.End(r.Context(), user(r), r.PathValue("id"), r.PathValue("session_id"), user(r))
	if err != nil {
		writeSessionError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, sessionView(s, false))
}

// interruptSession is POST .../interrupt: the running cell stops.
func (a *API) interruptSession(w http.ResponseWriter, r *http.Request) {
	a.sessionAction(w, r, a.d.Sessions.Interrupt)
}

// restartSession is POST .../restart: a new kernel; variables are gone.
func (a *API) restartSession(w http.ResponseWriter, r *http.Request) {
	a.sessionAction(w, r, a.d.Sessions.Restart)
}

func (a *API) sessionAction(w http.ResponseWriter, r *http.Request, do func(ctx context.Context, owner, spaceID, id string) error) {
	if !a.sessionsOn(w) {
		return
	}
	if err := do(r.Context(), user(r), r.PathValue("id"), r.PathValue("session_id")); err != nil {
		writeSessionError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusAccepted, map[string]any{"ok": true})
}

// syncSession is POST .../sync: a read-write session's file changes are
// written to the space now (they also are every few minutes and at the end).
func (a *API) syncSession(w http.ResponseWriter, r *http.Request) {
	if !a.sessionsOn(w) {
		return
	}
	conflicts, err := a.d.Sessions.SyncFiles(r.Context(), user(r), r.PathValue("id"), r.PathValue("session_id"))
	if err != nil {
		writeSessionError(w, r, err)
		return
	}
	type conflict struct {
		Path    string `json:"path"`
		SavedAs string `json:"saved_as,omitempty"` // where the session's version was kept ("" if it deleted the file)
	}
	out := make([]conflict, 0, len(conflicts))
	for _, c := range conflicts {
		out = append(out, conflict{Path: c.Path, SavedAs: c.SavedAs})
	}
	WriteJSON(w, http.StatusOK, map[string]any{"conflicts": out})
}

// adminListSessions is GET /v1/admin/sessions: every open session.
func (a *API) adminListSessions(w http.ResponseWriter, r *http.Request) {
	if !a.sessionsOn(w) {
		return
	}
	list, err := a.d.Sessions.All(r.Context())
	if err != nil {
		writeSessionError(w, r, err)
		return
	}
	items := make([]SessionView, 0, len(list))
	for _, s := range list {
		items = append(items, sessionView(s, true))
	}
	WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

// adminEndSession is POST /v1/admin/sessions/{session_id}/end {"reason": ...}.
func (a *API) adminEndSession(w http.ResponseWriter, r *http.Request) {
	if !a.sessionsOn(w) {
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if err := decodeJSON(r, &body, true); err != nil {
		writeSessionError(w, r, err)
		return
	}
	reason := "ended by an administrator"
	if body.Reason != "" {
		reason += ": " + clip(body.Reason)
	}
	s, err := a.d.Sessions.EndAny(r.Context(), r.PathValue("session_id"), user(r), reason)
	if err != nil {
		writeSessionError(w, r, err)
		return
	}
	audit.SetSpace(r.Context(), s.SpaceID)
	audit.SetDetail(r.Context(), body.Reason)
	WriteJSON(w, http.StatusOK, sessionView(s, true))
}

var upgrader = websocket.Upgrader{
	ReadBufferSize: 32 << 10, WriteBufferSize: 32 << 10,
	CheckOrigin: sameOrigin,
}

// sameOrigin refuses a WebSocket opened by a page of another site (which a
// browser would send with the user's cookie or cached basic credentials):
// an Origin, if sent, must be this host's. Programs send none.
func sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	return err == nil && u.Host != "" && u.Host == r.Host
}

// sessionChannel is GET .../channel: a WebSocket carrying the session's
// Jupyter messages, one JSON object per text message, both ways. Clients
// may send execute, complete, inspect, is_complete and kernel_info requests,
// and interrupt requests on the control channel; anything else is answered
// with a refusal on the "bridge" channel. The first message is the
// session's state ({"channel": "bridge", "content": {"status": "attached",
// "kernel": ...}}); when the session ends, a last "ended" message, then the
// WebSocket closes.
func (a *API) sessionChannel(w http.ResponseWriter, r *http.Request) {
	if !a.sessionsOn(w) {
		return
	}
	if !websocket.IsWebSocketUpgrade(r) {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "this is a WebSocket (Upgrade: websocket)")
		return
	}
	if !sameOrigin(r) {
		WriteError(w, http.StatusForbidden, CodeForbidden, "a session's WebSocket must be opened from this site's own pages")
		return
	}
	client, _, err := a.d.Sessions.Attach(r.Context(), user(r), r.PathValue("id"), r.PathValue("session_id"))
	if err != nil {
		writeSessionError(w, r, err)
		return
	}
	defer client.Close()
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return // the upgrader has answered
	}
	defer func() { _ = conn.Close() }()
	sessions.ServeWebSocket(r.Context(), conn, client, a.d.Shutdown)
	obs.Logger(r.Context()).Debug("session channel closed", "session_id", r.PathValue("session_id"), "reason", client.Reason())
}
