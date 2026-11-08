// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/azbyluthfan/nbsandbox/internal/runs"
)

// streamRunEvents is GET .../runs/{run_id}/events: the run's status changes
// (queued, running, then the final status with its result), and the stream
// ends after the final one.
func (a *API) streamRunEvents(w http.ResponseWriter, r *http.Request) {
	a.streamEvents(w, r, runs.EventsQuery{RunID: r.PathValue("run_id")})
}

// includeParam reads ?include=progress,cell.
func includeParam(r *http.Request) map[string]bool {
	out := map[string]bool{}
	for _, v := range r.URL.Query()["include"] {
		for _, k := range strings.Split(v, ",") {
			if k = strings.TrimSpace(k); k != "" {
				out[k] = true
			}
		}
	}
	return out
}

// streamSpaceEvents is GET /v1/spaces/{id}/events: the status changes of
// every run in the space (optionally ?function=name), until the client
// disconnects.
func (a *API) streamSpaceEvents(w http.ResponseWriter, r *http.Request) {
	a.streamEvents(w, r, runs.EventsQuery{Function: r.URL.Query().Get("function")})
}

// streamEvents sends runs.FollowEvents as numbered "status" events;
// reconnecting with Last-Event-ID (or ?after=) continues after the last one.
func (a *API) streamEvents(w http.ResponseWriter, r *http.Request, q runs.EventsQuery) {
	after, err := resumePoint(r)
	if err != nil {
		writeRunError(w, r, err)
		return
	}
	q.After = after
	q.Include = includeParam(r)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	items, err := a.d.Runs.FollowEvents(ctx, user(r), r.PathValue("id"), q)
	if err != nil {
		writeRunError(w, r, err)
		return
	}
	out, heartbeat, ok := a.openSSE(w, "nbsandbox status stream")
	defer heartbeat.Stop()
	if !ok {
		return
	}
	for {
		select {
		case it, open := <-items:
			if !open {
				return
			}
			var err error
			switch {
			case it.Kind == runs.EventPruned:
				err = out.event("pruned", map[string]string{"message": it.Message + " (GET .../runs/{id}/result)"})
			case it.Seq > 0:
				// The event's kind names it: status, partial, progress or cell.
				err = out.send(fmt.Sprintf("id: %d\nevent: %s\ndata: %s\n\n", it.Seq, eventName(it.Type), it.Data))
			default:
				err = out.send(fmt.Sprintf("event: status\ndata: %s\n\n", it.Data))
			}
			if err != nil {
				return
			}
		case <-heartbeat.C:
			if out.send(": heartbeat\n\n") != nil {
				return
			}
		case <-a.d.Shutdown:
			_ = out.event("shutdown", map[string]string{"message": shutdownMessage})
			return
		}
	}
}

func eventName(kind string) string {
	if kind == "" {
		return "status"
	}
	return kind
}
