// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package runs

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/contract"
	"github.com/azbyluthfan/nbsandbox/internal/spaces"
	"github.com/azbyluthfan/nbsandbox/internal/store"
)

// FunctionView is a published function.
type FunctionView struct {
	Name          string        `json:"name"`
	LatestVersion int           `json:"latest_version"`
	CreatedAt     time.Time     `json:"created_at"`
	UpdatedAt     time.Time     `json:"updated_at"`
	Versions      []VersionView `json:"versions,omitempty"`
}

// VersionView is one immutable published version.
type VersionView struct {
	Function       string        `json:"function"`
	Version        int           `json:"version"`
	Notebook       string        `json:"notebook"`
	Files          []string      `json:"files"`
	NotebookSHA256 string        `json:"notebook_sha256"`
	BundleBytes    int64         `json:"bundle_bytes"`
	CreatedBy      string        `json:"created_by"`
	CreatedAt      time.Time     `json:"created_at"`
	Contract       *ContractView `json:"contract,omitempty"`
}

// ContractView is a version's contract: JSON Schemas, null when absent.
type ContractView struct {
	Function string          `json:"function"`
	Version  int             `json:"version"`
	Input    json.RawMessage `json:"input"`
	// Mode is how the function returns its result: single (one value, the
	// output schema) or stream (a sequence of items, each matching item).
	Mode         string          `json:"mode"`
	ModeInferred bool            `json:"mode_inferred"` // from the notebook's calls, not declared
	Output       json.RawMessage `json:"output"`        // single: the value's schema, or null
	Item         json.RawMessage `json:"item"`          // stream: each item's schema, or null
}

func versionView(fv store.FunctionVersion, withContract bool) VersionView {
	v := VersionView{Function: fv.Function, Version: fv.Version, Notebook: fv.Notebook, NotebookSHA256: fv.NotebookSHA256,
		BundleBytes: fv.BundleBytes, CreatedBy: fv.CreatedBy, CreatedAt: fv.CreatedAt.UTC(), Files: []string{}}
	_ = json.Unmarshal([]byte(fv.FilesJSON), &v.Files)
	if withContract {
		v.Contract = contractView(fv)
	}
	return v
}

func contractView(fv store.FunctionVersion) *ContractView {
	c := &ContractView{Function: fv.Function, Version: fv.Version, Input: json.RawMessage("null"), Output: json.RawMessage("null"),
		Item: json.RawMessage("null"), Mode: fv.ContractMode, ModeInferred: !fv.ContractModeDeclared}
	if c.Mode == "" {
		c.Mode = contract.ModeSingle
	}
	if fv.ContractInput != "" {
		c.Input = json.RawMessage(fv.ContractInput)
	}
	if fv.ContractOutput != "" {
		if c.Mode == contract.ModeStream {
			c.Item = json.RawMessage(fv.ContractOutput)
		} else {
			c.Output = json.RawMessage(fv.ContractOutput)
		}
	}
	return c
}

func functionView(f store.Function) FunctionView {
	return FunctionView{Name: f.Name, LatestVersion: f.LatestVersion, CreatedAt: f.CreatedAt.UTC(), UpdatedAt: f.UpdatedAt.UTC()}
}

// PublishRequest publishes the space's notebook, with the files it needs, as
// the next version of a function.
type PublishRequest struct {
	Notebook string
	Files    []string
	// Contract, if given, is used instead of the one in the notebook's
	// metadata (the notebook file is not changed).
	Contract *contract.Spec
}

func badName(name string) error {
	if !spaces.ValidFunctionName(name) {
		return &spaces.InvalidError{Msg: "function names are 1-63 lowercase letters, digits, - and _, starting with a letter or digit"}
	}
	return nil
}

// Publish creates the next immutable version of a function (creating the
// function with version 1). Later edits to the space don't change it.
func (m *Manager) Publish(ctx context.Context, owner, spaceID, name string, req PublishRequest) (VersionView, error) {
	if err := badName(name); err != nil {
		return VersionView{}, err
	}
	if err := m.d.Spaces.Owned(ctx, owner, spaceID); err != nil {
		return VersionView{}, err
	}
	if len(req.Files) > spaces.MaxBundleFiles {
		return VersionView{}, &spaces.LimitError{Msg: "too many files"}
	}
	nb, err := m.d.Spaces.ReadNotebook(ctx, spaceID, req.Notebook)
	if err != nil {
		return VersionView{}, err
	}
	if err := CheckLanguage(nb); err != nil {
		return VersionView{}, err
	}
	c, err := resolveContract(nb, req.Contract)
	if err != nil {
		return VersionView{}, err
	}
	if m.cfg.RequireSchemas && (!c.HasInput() || !c.HasOutput()) {
		return VersionView{}, &contract.Error{Msg: "this server requires every function to declare input and output schemas " +
			"(functions.require_schemas); add a contract to the notebook's metadata or the publish request"}
	}
	m.publishMu.Lock()
	defer m.publishMu.Unlock()
	version, err := m.d.DB.NextFunctionVersion(ctx, spaceID, name)
	if err != nil {
		return VersionView{}, err
	}
	files := req.Files
	if files == nil {
		files = []string{}
	}
	refs, size, err := m.d.Spaces.BundleFiles(ctx, spaceID, req.Notebook, files)
	if err != nil {
		return VersionView{}, err
	}
	// The version's notebook is what invocations run: its contents' hash.
	nbSHA := ""
	for _, r := range refs {
		if r.Path == req.Notebook {
			nbSHA = r.SHA256
		}
	}
	fj, _ := json.Marshal(files)
	fv := store.FunctionVersion{SpaceID: spaceID, Function: name, Version: version, Notebook: req.Notebook,
		FilesJSON: string(fj), NotebookSHA256: nbSHA, BundleBytes: size, CreatedBy: owner,
		CreatedAt: m.d.Now(), Files: refs}
	if c.HasInput() {
		fv.ContractInput = string(c.InputJSON)
	}
	if c.HasOutput() {
		fv.ContractOutput = string(c.OutputJSON)
	}
	fv.ContractMode, fv.ContractModeDeclared = c.GetMode(), c.ModeDeclared
	if err := m.d.DB.AddFunctionVersion(ctx, fv); err != nil {
		return VersionView{}, err
	}
	m.d.Log.Info("function published", "space_id", spaceID, "function", name, "version", version, "bytes", size)
	return versionView(fv, true), nil
}

// InvokeRequest calls a published function.
type InvokeRequest struct {
	Version        int // 0 = the latest
	Parameters     json.RawMessage
	IdempotencyKey string
}

// Invoke runs a published version: an ordinary run, with the version's
// bundle mounted read-only as /space, so invocations of one space can run
// side by side.
func (m *Manager) Invoke(ctx context.Context, owner, spaceID, name string, req InvokeRequest) (View, bool, error) {
	if err := badName(name); err != nil {
		return View{}, false, err
	}
	if err := m.d.Spaces.Owned(ctx, owner, spaceID); err != nil {
		return View{}, false, err
	}
	fv, err := m.d.DB.GetFunctionVersion(ctx, spaceID, name, req.Version)
	if errors.Is(err, store.ErrNotFound) {
		return View{}, false, spaces.ErrNotFound
	}
	if err != nil {
		return View{}, false, err
	}
	nb, err := m.d.Spaces.ReadBundleNotebook(ctx, spaceID, name, fv.Version, fv.Notebook)
	if err != nil {
		return View{}, false, err
	}
	params, err := NormalizeParameters(req.Parameters, m.cfg.MaxParameters)
	if err != nil {
		return View{}, false, err
	}
	c, err := contract.Compile(json.RawMessage(fv.ContractInput), json.RawMessage(fv.ContractOutput))
	if err != nil {
		return View{}, false, err
	}
	c = c.WithMode(fv.ContractMode, fv.ContractModeDeclared)
	if err := c.ValidateInput(json.RawMessage(params)); err != nil {
		return View{}, false, err
	}
	return m.submit(ctx, owner, spaceID, SubmitRequest{Notebook: fv.Notebook, IdempotencyKey: req.IdempotencyKey},
		submission{notebook: nb, params: params, outputSchema: c, function: name, version: fv.Version})
}

// Functions lists a space's functions.
func (m *Manager) Functions(ctx context.Context, owner, spaceID string) ([]FunctionView, error) {
	if err := m.d.Spaces.Owned(ctx, owner, spaceID); err != nil {
		return nil, err
	}
	fs, err := m.d.DB.Functions(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	out := make([]FunctionView, 0, len(fs))
	for _, f := range fs {
		out = append(out, functionView(f))
	}
	return out, nil
}

// Function returns one function with its versions (newest first).
func (m *Manager) Function(ctx context.Context, owner, spaceID, name string) (FunctionView, error) {
	if err := badName(name); err != nil {
		return FunctionView{}, err
	}
	if err := m.d.Spaces.Owned(ctx, owner, spaceID); err != nil {
		return FunctionView{}, err
	}
	f, err := m.d.DB.GetFunction(ctx, spaceID, name)
	if errors.Is(err, store.ErrNotFound) {
		return FunctionView{}, spaces.ErrNotFound
	}
	if err != nil {
		return FunctionView{}, err
	}
	fvs, err := m.d.DB.FunctionVersions(ctx, spaceID, name)
	if err != nil {
		return FunctionView{}, err
	}
	v := functionView(f)
	v.Versions = make([]VersionView, 0, len(fvs))
	for _, fv := range fvs {
		v.Versions = append(v.Versions, versionView(fv, false))
	}
	return v, nil
}

// FunctionVersion returns one version with its contract (0 = the latest).
func (m *Manager) FunctionVersion(ctx context.Context, owner, spaceID, name string, version int) (VersionView, error) {
	if err := badName(name); err != nil {
		return VersionView{}, err
	}
	if err := m.d.Spaces.Owned(ctx, owner, spaceID); err != nil {
		return VersionView{}, err
	}
	fv, err := m.d.DB.GetFunctionVersion(ctx, spaceID, name, version)
	if errors.Is(err, store.ErrNotFound) {
		return VersionView{}, spaces.ErrNotFound
	}
	if err != nil {
		return VersionView{}, err
	}
	return versionView(fv, true), nil
}

// DeleteFunction removes a function and all its versions. Its past
// invocations (runs and results) stay; it refuses while one is active.
func (m *Manager) DeleteFunction(ctx context.Context, owner, spaceID, name string) error {
	if err := badName(name); err != nil {
		return err
	}
	if err := m.d.Spaces.Owned(ctx, owner, spaceID); err != nil {
		return err
	}
	m.publishMu.Lock()
	defer m.publishMu.Unlock()
	switch err := m.d.DB.DeleteFunction(ctx, spaceID, name); {
	case errors.Is(err, store.ErrNotFound):
		return spaces.ErrNotFound
	case errors.Is(err, store.ErrActiveRun):
		return ErrFunctionBusy
	case err != nil:
		return err
	}
	return nil // the versions' files go with their rows; their contents with the cleanup
}

// ErrFunctionBusy: a function with queued or running invocations can't be
// deleted (409).
var ErrFunctionBusy = errors.New("the function has queued or running invocations")
