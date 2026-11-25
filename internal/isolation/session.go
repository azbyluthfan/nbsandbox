// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package isolation

import (
	"context"
	"io"
)

// SessionSpec is an interactive session's container: a notebook's live
// kernel with a run's isolation (no network, the same limits, runtime and
// package set), whose main process is the session bridge
// (runner-image/session_bridge.py). Limits come from service configuration.
type SessionSpec struct {
	SessionID, SpaceID string
	SpaceDir           string // host folder mounted at /space: the session's working copy, or the space's files
	SpaceReadOnly      bool   // mount /space read-only (a read-only session)
	Image, Runtime     string
	Limits             Limits
	PackagesDir        string // the space's package set, if any (see RunSpec.PackagesDir)
	MaxMessage         int64  // the largest message the bridge sends; larger outputs become a notice
	// Fetcher, if set, reads the session's connections, as a run's does
	// (RunSpec.Fetcher), for the session's whole life.
	Fetcher *Fetcher
}

// SessionConn is an attached session's stdio: Jupyter messages, one JSON
// object per line, read from the bridge's stdout and written to its stdin.
// Close detaches; the session keeps running.
type SessionConn interface {
	io.Reader
	io.Writer
	Close() error
}

// SessionInfo is a session container that exists.
type SessionInfo struct {
	SessionID, SpaceID string
	Running            bool
}

// SessionBackend is a backend that runs interactive sessions.
type SessionBackend interface {
	// StartSession creates and starts the session's container, attached
	// from its first output on.
	StartSession(ctx context.Context, spec SessionSpec) (SessionConn, error)
	// AttachSession attaches to a running session again (after nbsandboxd
	// restarted, or lost the connection). Output written while detached is
	// lost.
	AttachSession(ctx context.Context, sessionID string) (SessionConn, error)
	// StopSession stops the session's container (its grace, then killed)
	// and removes it. A session that doesn't exist is not an error.
	StopSession(ctx context.Context, sessionID string) error
	// Sessions lists the session containers that exist.
	Sessions(ctx context.Context) ([]SessionInfo, error)
}

// SessionFilePuller is a session backend whose sessions' files aren't on
// this worker's disk (Kubernetes): before a read-write session's changes
// are written back, its /space is pulled over the local working copy.
type SessionFilePuller interface {
	PullSessionFiles(ctx context.Context, sessionID, localDir string) error
}

// SessionBridge is where the session bridge is in the runner image.
const SessionBridge = "/opt/nbsandbox/session_bridge.py"
