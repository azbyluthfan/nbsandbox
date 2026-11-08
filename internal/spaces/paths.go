// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package spaces

import (
	"fmt"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Path limits.
const (
	MaxPathBytes    = 1024
	MaxSegmentBytes = 255
)

// The folders inside a space's directory on this host, for active runs only:
// a run's outputs in runs/<run>/ (mounted at /run for that run) until they
// are stored when it ends, and its working copy of the space (or of a
// function version) in work/<run>/ (mounted at /space). Everything else is in
// the database and object storage.
const (
	RunsDir = "runs"
	WorkDir = "work"
)

// internalPrefix marks files nbsandboxd and the runner write for themselves.
const internalPrefix = ".nbsandbox"

// ValidatePath checks a user-supplied relative file path before any
// filesystem call. It is the first of two layers: everything that
// passes is still opened through os.Root, which refuses to leave the space.
//
// Rules: valid UTF-8; relative; segments separated by "/"; no empty, "." or
// ".." segments; no backslashes, NUL or control characters; no trailing
// slash; no ".nbsandbox*" names; length limits. Paths are rejected, never "cleaned", so what is stored is exactly
// what was asked for.
func ValidatePath(p string) error {
	switch {
	case p == "":
		return pathErr(p, "is empty")
	case len(p) > MaxPathBytes:
		return pathErr(p, fmt.Sprintf("is longer than %d bytes", MaxPathBytes))
	case !utf8.ValidString(p):
		return pathErr(p, "is not valid UTF-8")
	case strings.HasPrefix(p, "/"):
		return pathErr(p, "must be relative")
	case strings.ContainsRune(p, '\\'):
		return pathErr(p, "must use / as separator")
	}
	for _, r := range p {
		if r == 0 || unicode.IsControl(r) {
			return pathErr(p, "contains a control character")
		}
	}
	segs := strings.Split(p, "/")
	for _, s := range segs {
		switch {
		case s == "":
			return pathErr(p, "has an empty segment")
		case s == "." || s == "..":
			return pathErr(p, "must not contain . or .. segments")
		case len(s) > MaxSegmentBytes:
			return pathErr(p, fmt.Sprintf("has a segment longer than %d bytes", MaxSegmentBytes))
		case strings.HasPrefix(s, internalPrefix):
			return pathErr(p, "uses a reserved .nbsandbox name")
		}
	}
	// Belt and braces: the checks above imply this.
	if path.Clean(p) != p || !isLocal(p) {
		return pathErr(p, "is not a clean relative path")
	}
	return nil
}

// ValidateDir is ValidatePath for an optional destination folder ("" = top).
func ValidateDir(d string) error {
	if d == "" {
		return nil
	}
	return ValidatePath(d)
}

func isLocal(p string) bool {
	return p != "" && !strings.HasPrefix(p, "/") && p != ".." && !strings.HasPrefix(p, "../")
}

// PathError is a rejected path; the API maps it to 400.
type PathError struct {
	Path   string
	Reason string
}

func (e *PathError) Error() string {
	shown := e.Path
	if len(shown) > 80 {
		shown = shown[:80] + "…"
	}
	return fmt.Sprintf("path %q %s", shown, e.Reason)
}

func pathErr(p, reason string) error { return &PathError{Path: p, Reason: reason} }
