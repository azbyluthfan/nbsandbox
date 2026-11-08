// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/spaces"
)

// MaxWait bounds ?wait=: long enough to cover most calls, short enough for
// proxies and clients that time out idle requests.
const MaxWait = 60 * time.Second

// waitParam reads ?wait= as a duration ("30s") or a number of seconds.
func waitParam(r *http.Request) (time.Duration, error) {
	s := r.URL.Query().Get("wait")
	if s == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		n, nerr := strconv.Atoi(s)
		if nerr != nil {
			return 0, &spaces.InvalidError{Msg: `wait must be a duration such as "30s" or a number of seconds`}
		}
		d = time.Duration(n) * time.Second
	}
	if d < 0 || d > MaxWait {
		return 0, &spaces.InvalidError{Msg: "wait must be between 0 and " + MaxWait.String()}
	}
	return d, nil
}

// wantsStream reports whether the caller asked for the result as a stream:
// Accept: text/event-stream, or ?stream=1.
func wantsStream(r *http.Request) bool {
	switch r.URL.Query().Get("stream") {
	case "1", "true":
		return true
	}
	return strings.Contains(r.Header.Get("Accept"), "text/event-stream")
}

// getRunResultWait is GET .../result. As JSON: the result once the run has
// finished (with an optional ?wait=; 202 with the run if it is still going
// when the wait ends). As a stream: see streamResult.
func (a *API) getRunResultWait(w http.ResponseWriter, r *http.Request) {
	if wantsStream(r) {
		a.streamResult(w, r, r.PathValue("id"), r.PathValue("run_id"))
		return
	}
	wait, err := waitParam(r)
	if err != nil {
		writeRunError(w, r, err)
		return
	}
	if wait == 0 {
		a.getRunResult(w, r)
		return
	}
	res, now, done, err := a.d.Runs.WaitResult(r.Context(), user(r), r.PathValue("id"), r.PathValue("run_id"), wait, a.d.Shutdown)
	switch {
	case err != nil:
		writeRunError(w, r, err)
	case done:
		WriteJSON(w, http.StatusOK, res)
	default:
		WriteJSON(w, http.StatusAccepted, now)
	}
}

// streamResult sends a run's result as Server-Sent Events: an "item" event
// per item of a streamed output as soon as it is recorded (id = the item's
// number; resume with Last-Event-ID), "progress" events with
// ?include=progress, and last a "result" event with the result; then the
// stream ends. A finished run's items are replayed at once. The items are
// exactly the list in the JSON result's output.
func (a *API) streamResult(w http.ResponseWriter, r *http.Request, spaceID, runID string) {
	after, err := resumePoint(r)
	if err != nil {
		writeRunError(w, r, err)
		return
	}
	include := includeParam(r)
	for k := range include {
		if k != "progress" {
			writeRunError(w, r, &spaces.InvalidError{Msg: "the result stream's include takes progress"})
			return
		}
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	items, err := a.d.Runs.FollowResult(ctx, user(r), spaceID, runID, int(min(after, math.MaxInt32)), include["progress"])
	if err != nil {
		writeRunError(w, r, err)
		return
	}
	out, heartbeat, ok := a.openSSE(w, "nbsandbox result stream")
	defer heartbeat.Stop()
	if !ok {
		return
	}
	for {
		select {
		case ev, open := <-items:
			if !open {
				return
			}
			var err error
			switch ev.Kind {
			case "item":
				err = out.send(fmt.Sprintf("id: %d\nevent: item\ndata: %s\n\n", ev.Index, ev.Data))
			case "progress":
				err = out.send(fmt.Sprintf("event: progress\ndata: %s\n\n", ev.Data))
			case "result":
				_ = out.event("result", ev.Result)
				return
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
