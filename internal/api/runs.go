// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/completions"
	"github.com/azbyluthfan/nbsandbox/internal/contract"
	"github.com/azbyluthfan/nbsandbox/internal/runs"
	"github.com/azbyluthfan/nbsandbox/internal/spaces"
)

// maxRunBody bounds a run submission body.
func (a *API) maxRunBody() int64 {
	if a.d.MaxRunBody > 0 {
		return a.d.MaxRunBody
	}
	return runs.DefaultMaxParamsBytes + 64<<10
}

// requireValidRunID rejects a malformed {run_id} with 400.
func requireValidRunID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !spaces.ValidID(r.PathValue("run_id")) {
			WriteError(w, http.StatusBadRequest, CodeBadRequest, "run id must be a lowercase UUID")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// writeRunError maps run-manager errors, then falls back to the space errors.
func writeRunError(w http.ResponseWriter, r *http.Request, err error) {
	var (
		de *runs.DependencyError
		ce *contract.Error
		sa *runs.ErrStillActive
	)
	switch {
	case errors.As(err, &ce):
		code := CodeInvalidContract
		if len(ce.Problems) > 0 || strings.HasPrefix(ce.Msg, "parameters") {
			code = CodeInvalidParameters
		}
		WriteJSON(w, http.StatusBadRequest, ErrorBody{Error: ErrorDetail{Code: code, Message: ce.Msg, Problems: ce.Problems}})
	case errors.As(err, &sa):
		WriteJSON(w, http.StatusConflict, map[string]any{
			"error":  ErrorDetail{Code: CodeConflict, Message: sa.Error()},
			"status": sa.Status,
		})
	case errors.Is(err, runs.ErrDockerUnavailable):
		w.Header().Set("Retry-After", "10")
		WriteError(w, http.StatusServiceUnavailable, CodeDockerUnavailable, err.Error())
	case errors.Is(err, runs.ErrSpaceBusy), errors.Is(err, runs.ErrNotFinished), errors.Is(err, runs.ErrAlreadyFinished),
		errors.Is(err, runs.ErrFunctionBusy):
		WriteError(w, http.StatusConflict, CodeConflict, err.Error())
	case errors.Is(err, runs.ErrIdempotencyMismatch):
		WriteError(w, http.StatusUnprocessableEntity, CodeUnprocessable, err.Error())
	case errors.As(err, &de):
		WriteJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":               ErrorDetail{Code: CodeUnprocessable, Message: de.Error()},
			"dependency_warnings": de.Warnings,
		})
	default:
		writeDomainError(w, r, err)
	}
}

func (a *API) createRun(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Notebook   string          `json:"notebook"`
		Parameters json.RawMessage `json:"parameters"`
		Space      string          `json:"space"`
	}
	if err := decodeJSON(r, &body, false); err != nil {
		writeRunError(w, r, err)
		return
	}
	if body.Notebook == "" {
		writeRunError(w, r, &spaces.InvalidError{Msg: "notebook is required"})
		return
	}
	if body.Space != "" && body.Space != "read-write" && body.Space != "read-only" {
		writeRunError(w, r, &spaces.InvalidError{Msg: `space must be "read-write" or "read-only"`})
		return
	}
	// The request is fully read: record it even if the caller hangs up now
	//. Otherwise a disconnect could leave the caller
	// unsure whether the run exists; retrying with the same Idempotency-Key
	// then returns it.
	ctx := context.WithoutCancel(r.Context())
	v, replay, err := a.d.Runs.Submit(ctx, user(r), r.PathValue("id"), runs.SubmitRequest{
		Notebook: body.Notebook, Parameters: body.Parameters, IdempotencyKey: r.Header.Get("Idempotency-Key"),
		ReadOnlySpace: body.Space == "read-only",
	})
	if err != nil {
		writeRunError(w, r, err)
		return
	}
	w.Header().Set("Location", "/v1/spaces/"+v.SpaceID+"/runs/"+v.ID)
	code := http.StatusCreated
	if replay {
		code = http.StatusOK // same Idempotency-Key and body: the original run
	}
	WriteJSON(w, code, v)
}

func (a *API) listRuns(w http.ResponseWriter, r *http.Request) {
	cursor, limit, err := pageParams(r)
	if err != nil {
		writeRunError(w, r, err)
		return
	}
	q := r.URL.Query()
	f := runs.ListFilter{Status: q.Get("status")}
	for name, dst := range map[string]**time.Time{"created_after": &f.CreatedAfter, "created_before": &f.CreatedBefore} {
		if v := q.Get(name); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				writeRunError(w, r, &spaces.InvalidError{Msg: name + " must be an RFC 3339 time"})
				return
			}
			*dst = &t
		}
	}
	list, next, err := a.d.Runs.List(r.Context(), user(r), r.PathValue("id"), f, cursor, limit)
	if err != nil {
		writeRunError(w, r, err)
		return
	}
	if list == nil {
		list = []runs.View{}
	}
	WriteJSON(w, http.StatusOK, map[string]any{"items": list, "next_cursor": next})
}

func (a *API) getRun(w http.ResponseWriter, r *http.Request) {
	v, err := a.d.Runs.Get(r.Context(), user(r), r.PathValue("id"), r.PathValue("run_id"))
	if err != nil {
		writeRunError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, v)
}

func (a *API) cancelRun(w http.ResponseWriter, r *http.Request) {
	// Ownership first: another user's run is 404, whatever the body.
	if err := a.d.Runs.RunExists(r.Context(), user(r), r.PathValue("id"), r.PathValue("run_id")); err != nil {
		writeRunError(w, r, err)
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if err := decodeJSON(r, &body, true); err != nil {
		writeRunError(w, r, err)
		return
	}
	v, err := a.d.Runs.Cancel(r.Context(), user(r), r.PathValue("id"), r.PathValue("run_id"), user(r), body.Reason)
	if err != nil {
		writeRunError(w, r, err)
		return
	}
	// A running run stops asynchronously; poll it for the final status.
	WriteJSON(w, http.StatusAccepted, v)
}

// getRunResult is the caller's view of a finished run: the output, or the
// error with its reasons. 409 while the run is active.
func (a *API) getRunResult(w http.ResponseWriter, r *http.Request) {
	res, err := a.d.Runs.GetResult(r.Context(), user(r), r.PathValue("id"), r.PathValue("run_id"))
	if err != nil {
		writeRunError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, res)
}

func (a *API) deleteRun(w http.ResponseWriter, r *http.Request) {
	if err := a.d.Runs.Delete(r.Context(), user(r), r.PathValue("id"), r.PathValue("run_id")); err != nil {
		writeRunError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// serveRunFile streams one run output. Notebook content is user code and
// output, so nothing is ever served in a way that could run on the API's
// origin: downloads are attachments, and the HTML view is sandboxed.
func (a *API) serveRunFile(w http.ResponseWriter, r *http.Request, rel, contentType string, inline bool) {
	f, info, err := a.d.Runs.OpenOutput(r.Context(), user(r), r.PathValue("id"), r.PathValue("run_id"), rel)
	if err != nil {
		writeRunError(w, r, err)
		return
	}
	defer func() { _ = f.Close() }()
	name := path.Base(rel)
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, no-cache")
	if inline {
		// The rendered notebook is viewable, but scripts can't run and it
		// gets a unique origin; styles and data: images still work.
		w.Header().Set("Content-Security-Policy",
			"sandbox; default-src 'none'; style-src 'unsafe-inline'; img-src data:; font-src data:")
		w.Header().Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": name}))
	} else {
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	}
	http.ServeContent(w, r, name, info.ModTime(), f)
}

func (a *API) getRunNotebook(w http.ResponseWriter, r *http.Request) {
	a.serveRunFile(w, r, spaces.RunOutput, "application/x-ipynb+json", false)
}

func (a *API) getRunHTML(w http.ResponseWriter, r *http.Request) {
	a.serveRunFile(w, r, spaces.RunHTML, "text/html; charset=utf-8", true)
}

// getRunLog returns the log so far (the whole log once the run finished).
// Live streaming is GET .../log/stream.
func (a *API) getRunLog(w http.ResponseWriter, r *http.Request) {
	a.serveRunFile(w, r, spaces.RunLog, "text/plain; charset=utf-8", false)
}

func (a *API) listArtifacts(w http.ResponseWriter, r *http.Request) {
	files, err := a.d.Runs.Artifacts(r.Context(), user(r), r.PathValue("id"), r.PathValue("run_id"))
	if err != nil {
		writeRunError(w, r, err)
		return
	}
	if files == nil {
		files = []spaces.File{}
	}
	WriteJSON(w, http.StatusOK, map[string]any{"items": files})
}

// promoteArtifact copies a run's artifact into the space's files:
// {"artifact": "model.pkl", "to": "models/model.pkl"}.
func (a *API) promoteArtifact(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Artifact string `json:"artifact"`
		To       string `json:"to"`
	}
	if err := decodeJSON(r, &body, false); err != nil {
		writeRunError(w, r, err)
		return
	}
	f, err := a.d.Runs.PromoteArtifact(r.Context(), user(r), r.PathValue("id"), r.PathValue("run_id"), body.Artifact, body.To)
	if err != nil {
		writeRunError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusCreated, f)
}

func (a *API) getArtifact(w http.ResponseWriter, r *http.Request) {
	p := r.PathValue("path")
	if err := spaces.ValidateArtifactPath(p); err != nil {
		writeRunError(w, r, err)
		return
	}
	a.serveRunFile(w, r, spaces.RunArtifacts+"/"+p, "application/octet-stream", false)
}

// RuntimeInfo is GET /v1/runtime.
type RuntimeInfo struct {
	Image struct {
		Ref         string   `json:"ref"`
		ID          string   `json:"id"`
		RepoDigests []string `json:"repo_digests"`
	} `json:"image"`
	PythonVersion string            `json:"python_version"`
	Packages      map[string]string `json:"packages"`
	// Modules are the top-level names a notebook can import: the standard
	// library, the installed packages' modules, and nbsandbox (for editors that
	// flag imports the image lacks).
	Modules []string      `json:"modules"`
	Runtime string        `json:"runtime"`
	Limits  RuntimeLimits `json:"limits"`
	Docker  struct {
		Version         string `json:"version"`
		OperatingSystem string `json:"operating_system"`
	} `json:"docker"`
	// Completions names the image's completion index for editors (GET
	// /v1/runtime/completions); absent when the image has none.
	Completions *completions.Ref `json:"completions,omitempty"`
}

// RuntimeLimits are the limits every run gets.
type RuntimeLimits struct {
	MemoryBytes    int64   `json:"memory_bytes"`
	CPUs           float64 `json:"cpus"`
	PIDs           int64   `json:"pids"`
	TmpBytes       int64   `json:"tmp_bytes"`
	TimeoutSeconds float64 `json:"timeout_seconds"`
	LogBytes       int64   `json:"log_bytes"`
	MaxFileBytes   int64   `json:"max_file_bytes"`
	MaxSpaceBytes  int64   `json:"max_space_bytes"`
	Concurrency    int     `json:"concurrency"`
}

func (a *API) getRuntime(w http.ResponseWriter, r *http.Request) {
	info, err := a.d.Runtime()
	if err != nil {
		writeRunError(w, r, err)
		return
	}
	if info.Image.RepoDigests == nil { // the spec says array, never null
		info.Image.RepoDigests = []string{}
	}
	if info.Packages == nil {
		info.Packages = map[string]string{}
	}
	if info.Modules == nil {
		info.Modules = []string{}
	}
	WriteJSON(w, http.StatusOK, info)
}
