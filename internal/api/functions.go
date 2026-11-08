// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/azbyluthfan/nbsandbox/internal/contract"
	"github.com/azbyluthfan/nbsandbox/internal/runs"
	"github.com/azbyluthfan/nbsandbox/internal/spaces"
)

func versionParam(r *http.Request) (int, error) {
	s := r.PathValue("version")
	if s == "" {
		return 0, nil
	}
	v, err := strconv.Atoi(s)
	if err != nil || v < 1 {
		return 0, &spaces.InvalidError{Msg: "version must be a positive whole number"}
	}
	return v, nil
}

func (a *API) listFunctions(w http.ResponseWriter, r *http.Request) {
	list, err := a.d.Runs.Functions(r.Context(), user(r), r.PathValue("id"))
	if err != nil {
		writeRunError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"items": list})
}

func (a *API) getFunction(w http.ResponseWriter, r *http.Request) {
	f, err := a.d.Runs.Function(r.Context(), user(r), r.PathValue("id"), r.PathValue("name"))
	if err != nil {
		writeRunError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, f)
}

func (a *API) deleteFunction(w http.ResponseWriter, r *http.Request) {
	if err := a.d.Runs.DeleteFunction(r.Context(), user(r), r.PathValue("id"), r.PathValue("name")); err != nil {
		writeRunError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// publishFunction publishes the next version:
// {"notebook": "forecast.ipynb", "files": ["models/model.pkl", "data/"]}.
func (a *API) publishFunction(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Notebook string         `json:"notebook"`
		Files    []string       `json:"files"`
		Contract *contract.Spec `json:"contract"` // instead of the notebook's
	}
	if err := decodeJSON(r, &body, false); err != nil {
		writeRunError(w, r, err)
		return
	}
	if body.Notebook == "" {
		writeRunError(w, r, &spaces.InvalidError{Msg: "notebook is required"})
		return
	}
	v, err := a.d.Runs.Publish(r.Context(), user(r), r.PathValue("id"), r.PathValue("name"),
		runs.PublishRequest{Notebook: body.Notebook, Files: body.Files, Contract: body.Contract})
	if err != nil {
		writeRunError(w, r, err)
		return
	}
	w.Header().Set("Location", "/v1/spaces/"+r.PathValue("id")+"/functions/"+v.Function+"/versions/"+strconv.Itoa(v.Version))
	WriteJSON(w, http.StatusCreated, v)
}

func (a *API) getFunctionVersion(w http.ResponseWriter, r *http.Request) {
	ver, err := versionParam(r)
	if err != nil {
		writeRunError(w, r, err)
		return
	}
	v, err := a.d.Runs.FunctionVersion(r.Context(), user(r), r.PathValue("id"), r.PathValue("name"), ver)
	if err != nil {
		writeRunError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, v)
}

// getFunctionContract returns the latest version's input and output schemas.
func (a *API) getFunctionContract(w http.ResponseWriter, r *http.Request) {
	v, err := a.d.Runs.FunctionVersion(r.Context(), user(r), r.PathValue("id"), r.PathValue("name"), 0)
	if err != nil {
		writeRunError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, v.Contract)
}

// invokeFunction calls the latest version, or the one in the path:
// {"parameters": {...}}. 201 with the run; with ?wait= the answer waits for
// the result (see runs.WaitResult).
func (a *API) invokeFunction(w http.ResponseWriter, r *http.Request) {
	ver, err := versionParam(r)
	if err != nil {
		writeRunError(w, r, err)
		return
	}
	wait, err := waitParam(r)
	if err != nil {
		writeRunError(w, r, err)
		return
	}
	var body struct {
		Parameters json.RawMessage `json:"parameters"`
	}
	if err := decodeJSON(r, &body, true); err != nil {
		writeRunError(w, r, err)
		return
	}
	// The request is fully read: record it even if the caller hangs up now.
	ctx := context.WithoutCancel(r.Context())
	v, replay, err := a.d.Runs.Invoke(ctx, user(r), r.PathValue("id"), r.PathValue("name"), runs.InvokeRequest{
		Version: ver, Parameters: body.Parameters, IdempotencyKey: r.Header.Get("Idempotency-Key"),
	})
	if err != nil {
		writeRunError(w, r, err)
		return
	}
	w.Header().Set("Location", "/v1/spaces/"+v.SpaceID+"/runs/"+v.ID)
	if wantsStream(r) {
		// The call's result as it is made, as GET .../result streams it.
		a.streamResult(w, r, v.SpaceID, v.ID)
		return
	}
	if wait > 0 {
		res, now, done, err := a.d.Runs.WaitResult(r.Context(), user(r), v.SpaceID, v.ID, wait, a.d.Shutdown)
		switch {
		case err != nil:
			writeRunError(w, r, err)
		case done:
			WriteJSON(w, http.StatusOK, res)
		default:
			WriteJSON(w, http.StatusAccepted, now)
		}
		return
	}
	code := http.StatusCreated
	if replay {
		code = http.StatusOK
	}
	WriteJSON(w, code, v)
}
