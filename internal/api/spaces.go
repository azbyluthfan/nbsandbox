// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/auth"
	"github.com/azbyluthfan/nbsandbox/internal/obs"
	"github.com/azbyluthfan/nbsandbox/internal/spaces"
)

// Paging limits for list endpoints.
const (
	DefaultPageSize = 50
	MaxPageSize     = 200
)

type spaceJSON struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	Usage     *usageJSON `json:"usage,omitempty"`
}

type usageJSON struct {
	BytesUsed  int64 `json:"bytes_used"`
	QuotaBytes int64 `json:"quota_bytes"`
	FileCount  int64 `json:"file_count"`
	RunCount   int   `json:"run_count"`
}

func toSpaceJSON(sp spaces.Space, withUsage bool) spaceJSON {
	out := spaceJSON{ID: sp.ID, Name: sp.Name, CreatedAt: sp.CreatedAt.UTC(), UpdatedAt: sp.UpdatedAt.UTC()}
	if withUsage {
		out.Usage = &usageJSON{BytesUsed: sp.Usage.BytesUsed, QuotaBytes: sp.Usage.QuotaBytes,
			FileCount: sp.Usage.FileCount, RunCount: sp.Usage.RunCount}
	}
	return out
}

func user(r *http.Request) string {
	u, _ := auth.User(r.Context())
	return u
}

// writeDomainError maps service errors to HTTP. Unknown errors are logged
// and become a 500 without details.
func writeDomainError(w http.ResponseWriter, r *http.Request, err error) {
	var (
		pe  *spaces.PathError
		ie  *spaces.InvalidError
		le  *spaces.LimitError
		ve  *spaces.VersionError
		mbe *http.MaxBytesError
	)
	switch {
	case errors.Is(err, spaces.ErrNotFound):
		WriteError(w, http.StatusNotFound, CodeNotFound, "not found")
	case errors.Is(err, spaces.ErrActiveRun), errors.Is(err, spaces.ErrExists), errors.Is(err, spaces.ErrNotEmpty):
		WriteError(w, http.StatusConflict, CodeConflict, err.Error())
	case errors.As(err, &ve):
		status, code := http.StatusPreconditionFailed, CodeVersionMismatch
		if ve.Required {
			status, code = http.StatusPreconditionRequired, CodeVersionRequired
		}
		if ve.Current != "" {
			w.Header().Set("ETag", etag(ve.Current))
		}
		WriteJSON(w, status, map[string]any{
			"error":           ErrorDetail{Code: code, Message: ve.Error()},
			"current_version": ve.Current,
		})
	case errors.As(err, &pe), errors.As(err, &ie):
		WriteError(w, http.StatusBadRequest, CodeBadRequest, err.Error())
	case errors.As(err, &le):
		WriteError(w, http.StatusRequestEntityTooLarge, CodePayloadTooLarge, err.Error())
	case errors.As(err, &mbe):
		WriteError(w, http.StatusRequestEntityTooLarge, CodePayloadTooLarge,
			fmt.Sprintf("request body larger than %d bytes", mbe.Limit))
	default:
		obs.Logger(r.Context()).Error("request failed", "error", err)
		WriteError(w, http.StatusInternalServerError, CodeInternal, "internal error")
	}
}

// decodeJSON reads a small JSON body strictly (unknown fields are errors).
// An empty body decodes to the zero value when allowEmpty is set.
func decodeJSON(r *http.Request, v any, allowEmpty bool) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) && allowEmpty {
			return nil
		}
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return err
		}
		return &spaces.InvalidError{Msg: "invalid JSON body: " + err.Error()}
	}
	if dec.More() {
		return &spaces.InvalidError{Msg: "invalid JSON body: trailing data"}
	}
	return nil
}

func (a *API) createSpace(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(r, &body, true); err != nil {
		writeDomainError(w, r, err)
		return
	}
	sp, err := a.d.Spaces.Create(r.Context(), user(r), body.Name)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	w.Header().Set("Location", "/v1/spaces/"+sp.ID)
	WriteJSON(w, http.StatusCreated, toSpaceJSON(sp, true))
}

func pageParams(r *http.Request) (string, int, error) {
	limit := DefaultPageSize
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > MaxPageSize {
			return "", 0, &spaces.InvalidError{Msg: fmt.Sprintf("limit must be between 1 and %d", MaxPageSize)}
		}
		limit = n
	}
	return r.URL.Query().Get("cursor"), limit, nil
}

func (a *API) listSpaces(w http.ResponseWriter, r *http.Request) {
	cursor, limit, err := pageParams(r)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	list, next, err := a.d.Spaces.List(r.Context(), user(r), cursor, limit)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	items := make([]spaceJSON, 0, len(list))
	for _, sp := range list {
		items = append(items, toSpaceJSON(sp, false))
	}
	WriteJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": next})
}

func (a *API) getSpace(w http.ResponseWriter, r *http.Request) {
	sp, err := a.d.Spaces.Get(r.Context(), user(r), r.PathValue("id"))
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, toSpaceJSON(sp, true))
}

func (a *API) patchSpace(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name *string `json:"name"`
	}
	if err := decodeJSON(r, &body, false); err != nil {
		writeDomainError(w, r, err)
		return
	}
	if body.Name == nil {
		writeDomainError(w, r, &spaces.InvalidError{Msg: "name is required"})
		return
	}
	sp, err := a.d.Spaces.Rename(r.Context(), user(r), r.PathValue("id"), *body.Name)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, toSpaceJSON(sp, true))
}

func (a *API) deleteSpace(w http.ResponseWriter, r *http.Request) {
	if err := a.d.Spaces.Delete(r.Context(), user(r), r.PathValue("id")); err != nil {
		writeDomainError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// uploadFiles stores one or more files sent as multipart/form-data, each in
// a part named "file". ?folder= puts them in a folder. The original filename is
// checked as sent: one with a folder or ".." in it is rejected (400), not
// silently shortened.
func (a *API) uploadFiles(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Has("dir") {
		// The old name: refuse it rather than upload to the top level.
		writeDomainError(w, r, &spaces.InvalidError{Msg: "the destination parameter is ?folder=, not ?dir="})
		return
	}
	mr, err := r.MultipartReader()
	if err != nil {
		writeDomainError(w, r, &spaces.InvalidError{Msg: "body must be multipart/form-data with parts named \"file\""})
		return
	}
	next := func() (*spaces.UploadPart, error) {
		part, err := mr.NextPart()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, io.EOF
			}
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				return nil, err
			}
			return nil, &spaces.InvalidError{Msg: "malformed multipart body: " + err.Error()}
		}
		name, ok := rawFileName(part)
		if part.FormName() != "file" || !ok {
			return nil, &spaces.InvalidError{Msg: fmt.Sprintf("unexpected part %q: send files as parts named \"file\" with a filename", part.FormName())}
		}
		return &spaces.UploadPart{Name: name, Body: part}, nil
	}
	files, err := a.d.Spaces.Upload(r.Context(), user(r), r.PathValue("id"), r.URL.Query().Get("folder"), next)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusCreated, map[string]any{"files": files})
}

// rawFileName returns the filename exactly as the client sent it.
// multipart.Part.FileName strips folders, which would hide traversal
// attempts instead of rejecting them.
func rawFileName(p *multipart.Part) (string, bool) {
	_, params, err := mime.ParseMediaType(p.Header.Get("Content-Disposition"))
	if err != nil {
		return "", false
	}
	name, ok := params["filename"]
	// A name with / or \ is passed through so the service rejects it with a
	// clear message.
	return name, ok && name != ""
}

func (a *API) listFiles(w http.ResponseWriter, r *http.Request) {
	files, err := a.d.Spaces.ListFiles(r.Context(), user(r), r.PathValue("id"))
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	if files == nil {
		files = []spaces.File{}
	}
	WriteJSON(w, http.StatusOK, map[string]any{"items": files})
}

// getFile downloads a file. Content is the user's, so it is always sent as an
// attachment with sniffing disabled: a stored .html file must never run as a
// page on the API's origin.
func (a *API) getFile(w http.ResponseWriter, r *http.Request) {
	f, info, err := a.d.Spaces.OpenFile(r.Context(), user(r), r.PathValue("id"), r.PathValue("path"))
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	defer func() { _ = f.Close() }()
	name := path.Base(r.PathValue("path"))
	// The version the editor works on: send it back in If-Match to save.
	w.Header().Set("ETag", etag(info.Version))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, no-cache")
	http.ServeContent(w, r, name, info.Modified, f)
}

// deleteFile deletes a file, or a folder: an empty one, or with
// ?recursive=true everything in it.
func (a *API) deleteFile(w http.ResponseWriter, r *http.Request) {
	recursive := false
	switch q := r.URL.Query().Get("recursive"); q {
	case "", "false":
	case "true":
		recursive = true
	default:
		writeDomainError(w, r, &spaces.InvalidError{Msg: "recursive must be true or false"})
		return
	}
	if err := a.d.Spaces.DeletePath(r.Context(), user(r), r.PathValue("id"), r.PathValue("path"), recursive); err != nil {
		writeDomainError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// etag quotes a file version for the ETag header; parseETag undoes it.
func etag(version string) string { return `"` + version + `"` }

func parseETag(h string) string {
	h = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(h), "W/"))
	return strings.Trim(h, `"`)
}

// putFile creates or replaces one file with the raw request body. Replacing
// needs If-Match with the version being replaced; If-None-Match: * creates
// only. 412 when the file changed meanwhile (the body says the current
// version), 428 when If-Match is missing for an existing file.
func (a *API) putFile(w http.ResponseWriter, r *http.Request) {
	req := spaces.WriteRequest{Path: r.PathValue("path"), Body: r.Body,
		IfMatch: parseETag(r.Header.Get("If-Match")), IfNoneMatch: strings.TrimSpace(r.Header.Get("If-None-Match")) == "*"}
	if req.IfMatch == "*" {
		writeDomainError(w, r, &spaces.InvalidError{Msg: "If-Match must name a version, not *"})
		return
	}
	existed := false
	if _, err := a.d.Spaces.Stat(r.Context(), user(r), r.PathValue("id"), req.Path); err == nil {
		existed = true
	}
	f, err := a.d.Spaces.WriteFile(r.Context(), user(r), r.PathValue("id"), req)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	w.Header().Set("ETag", etag(f.Version))
	code := http.StatusCreated
	if existed {
		code = http.StatusOK
	}
	WriteJSON(w, code, f)
}

// patchFile renames or moves a file or folder: {"path": "new/place"}.
// If-Match (optional) moves a file only if it is still that version.
func (a *API) patchFile(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
	}
	if err := decodeJSON(r, &body, false); err != nil {
		writeDomainError(w, r, err)
		return
	}
	if body.Path == "" {
		writeDomainError(w, r, &spaces.InvalidError{Msg: "path is required"})
		return
	}
	f, err := a.d.Spaces.Move(r.Context(), user(r), r.PathValue("id"), r.PathValue("path"), body.Path,
		parseETag(r.Header.Get("If-Match")))
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, f)
}

// createFolder makes an empty folder: {"path": "data/2026"}.
func (a *API) createFolder(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
	}
	if err := decodeJSON(r, &body, false); err != nil {
		writeDomainError(w, r, err)
		return
	}
	f, err := a.d.Spaces.CreateFolder(r.Context(), user(r), r.PathValue("id"), body.Path)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusCreated, f)
}
