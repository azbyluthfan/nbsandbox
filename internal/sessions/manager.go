// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

// Package sessions runs interactive sessions: a notebook's live kernel in a
// container with a run's isolation, reached over the container's stdio as
// Jupyter messages (docs/sessions.md).
//
// The API side records a session (store.OpenSession); a worker takes it
// (store.ClaimSession), checks out the space's files, starts the container
// and keeps it: it relays messages between the session's bridge and the
// connected clients, ends it when idle or past its maximum, writes a
// read-write session's file changes back, and re-attaches to its sessions
// after a restart.
package sessions

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/isolation"
	"github.com/azbyluthfan/nbsandbox/internal/spaces"
	"github.com/azbyluthfan/nbsandbox/internal/store"
)

// Config is the session settings in effect.
type Config struct {
	Enabled        bool
	Image, Runtime string
	Limits         isolation.Limits
	Idle, Max      time.Duration
	WriteBack      time.Duration
	Retention      time.Duration
	PerUser        int
	PerWorker      int
	MaxMessage     int64
	WorkerID       string
	// NoExecute: this process doesn't hold sessions (an API node).
	NoExecute bool
	// Shared: other workers share the database (API and worker
	// processes); a session another live worker holds is left alone, and is
	// reached through it (Secret).
	Shared bool
	// Secret authenticates API nodes to workers (InternalHandler).
	Secret string
	// Tick is how often sessions are checked (idle, maximum, write-back,
	// ended elsewhere); 10s if zero.
	Tick time.Duration
	// StartTimeout bounds a session's start, until its kernel is ready;
	// 3 minutes if zero.
	StartTimeout time.Duration
}

// DB is the store the manager uses (*store.DB).
type DB interface {
	OpenSession(ctx context.Context, s store.Session, perUser int) (store.Session, bool, error)
	ClaimSession(ctx context.Context, worker string, perWorker int) (store.Session, bool, error)
	MarkSessionReady(ctx context.Context, id string) (bool, error)
	EndSession(ctx context.Context, id, reason, by string, at time.Time) (bool, error)
	GetSession(ctx context.Context, id string) (store.Session, error)
	AdoptSession(ctx context.Context, id, worker string) (store.Session, error)
	OpenSessions(ctx context.Context, f store.SessionFilter) ([]store.Session, error)
	DeleteEndedSessionsBefore(ctx context.Context, t time.Time) (int64, error)
	Notify(ctx context.Context, channel, payload string) error
	SessionConnections(ctx context.Context, sessionID string) (map[string]string, error)
}

// Spaces is the space service the manager uses (*spaces.Service).
type Spaces interface {
	Get(ctx context.Context, owner, id string) (spaces.Space, error)
	Stat(ctx context.Context, owner, id, p string) (spaces.File, error)
	Checkout(ctx context.Context, spaceID, id string) (string, error)
	Sync(ctx context.Context, spaceID, id string) ([]store.Conflict, []string, error)
	Discard(spaceID, id string)
	WorkPath(spaceID, id string) string
	// Usage is the space's usage, working copies included (a read-write
	// session over its space's quota ends).
	Usage(ctx context.Context, id string) (spaces.Usage, error)
	InvalidateUsage(id string)
}

// Deps are the manager's collaborators.
type Deps struct {
	DB     DB
	Spaces Spaces
	// Backend runs sessions; nil where sessions can't run (an API node, or
	// a backend without sessions).
	Backend isolation.SessionBackend
	// Packages returns the space's package set folder on this worker ("" if
	// none), waiting while it is built. A note, if any, is shown in the
	// session (why it has no packages).
	Packages func(ctx context.Context, spaceID string) (dir, note string, err error)
	// Draining reports whether workers are drained: no session starts.
	Draining func() bool
	// Workers lists the live workers (with Shared): who else holds
	// sessions, and where API nodes reach them.
	Workers func(ctx context.Context) ([]store.Worker, error)
	// Data gives sessions their space's connections, as runs get theirs
	// (internal/connections; nil: connections are off).
	Data DataAccess
	// HTTP reaches other workers (nil: a default client).
	HTTP *http.Client
	Log  *slog.Logger
	Now  func() time.Time
}

// Errors.
var (
	ErrDisabled    = errors.New("interactive sessions are turned off on this nbsandbox")
	ErrUnavailable = errors.New("interactive sessions can't run here (no worker with a backend that runs them)")
	ErrDraining    = errors.New("the workers are drained: no session starts until they are resumed")
	ErrNotHere     = errors.New("the session runs on another worker")
	ErrEnded       = errors.New("the session has ended")
	ErrLimit       = store.ErrSessionLimit
)

// InvalidError is a request the caller can fix.
type InvalidError struct{ Msg string }

func (e *InvalidError) Error() string { return e.Msg }

// Manager starts, holds and ends sessions.
type Manager struct {
	cfg Config
	d   Deps

	mu   sync.Mutex
	live map[string]*live // the sessions this process holds
	wake chan struct{}
	stop chan struct{}
	done sync.WaitGroup
	once sync.Once
}

// New returns a manager. Start begins holding sessions on a worker.
func New(cfg Config, d Deps) *Manager {
	if cfg.Tick <= 0 {
		cfg.Tick = 10 * time.Second
	}
	if cfg.StartTimeout <= 0 {
		cfg.StartTimeout = 3 * time.Minute
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.Draining == nil {
		d.Draining = func() bool { return false }
	}
	return &Manager{cfg: cfg, d: d, live: map[string]*live{}, wake: make(chan struct{}, 1), stop: make(chan struct{})}
}

// Enabled reports whether sessions are on.
func (m *Manager) Enabled() bool { return m != nil && m.cfg.Enabled }

// Holds reports whether this process holds sessions (a worker with a
// backend that runs them).
func (m *Manager) Holds() bool {
	return m.Enabled() && !m.cfg.NoExecute && m.d.Backend != nil
}

// Wake tells the worker loop a session may be waiting for it.
func (m *Manager) Wake() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// Open returns the notebook's open session, or starts one. mode is
// store.SessionReadWrite (the default) or store.SessionReadOnly; a
// read-write session is given only while nothing else can write the space,
// otherwise the session is read-only (Session.Mode says which).
// DataAccess resolves a session's names and makes its fetcher.
type DataAccess interface {
	Resolve(ctx context.Context, spaceID, environment string) (string, map[string]string, error)
	Fetcher(ctx context.Context, id, owner string, names map[string]string, expires time.Time) (*isolation.Fetcher, error)
}

// Open opens a session on the notebook, reading the space's names in
// environment ("": the space's default), or returns the notebook's open
// session (which keeps the environment it was opened in).
func (m *Manager) Open(ctx context.Context, owner, spaceID, notebook, mode, environment string) (store.Session, bool, error) {
	if !m.Enabled() {
		return store.Session{}, false, ErrDisabled
	}
	switch mode {
	case "":
		mode = store.SessionReadWrite
	case store.SessionReadWrite, store.SessionReadOnly:
	default:
		return store.Session{}, false, &InvalidError{Msg: `mode must be "read-write" or "read-only"`}
	}
	if !strings.HasSuffix(notebook, ".ipynb") {
		return store.Session{}, false, &InvalidError{Msg: "notebook must be a .ipynb file in the space"}
	}
	f, err := m.d.Spaces.Stat(ctx, owner, spaceID, notebook)
	if err != nil {
		return store.Session{}, false, err
	}
	if f.Type != spaces.TypeFile {
		return store.Session{}, false, &InvalidError{Msg: "notebook must be a .ipynb file in the space"}
	}
	if m.d.Draining() {
		return store.Session{}, false, ErrDraining
	}
	var env string
	var names map[string]string
	switch {
	case m.d.Data != nil:
		if env, names, err = m.d.Data.Resolve(ctx, spaceID, environment); err != nil {
			return store.Session{}, false, err
		}
	case environment != "":
		return store.Session{}, false, &InvalidError{Msg: "environment: connections are off on this nbsandbox"}
	}
	s, created, err := m.d.DB.OpenSession(ctx, store.Session{
		ID: spaces.NewID(), SpaceID: spaceID, Owner: owner, Notebook: path.Clean(notebook), Mode: mode, CreatedAt: m.d.Now(),
		Environment: env, Connections: names,
	}, m.cfg.PerUser)
	if err != nil {
		return store.Session{}, false, err
	}
	if created {
		m.Wake()
	}
	return s, created, nil
}

// Get returns one of the owner's sessions in the space, open or ended.
func (m *Manager) Get(ctx context.Context, owner, spaceID, id string) (store.Session, error) {
	s, err := m.d.DB.GetSession(ctx, id)
	if err != nil {
		return store.Session{}, err
	}
	if s.Owner != owner || s.SpaceID != spaceID {
		return store.Session{}, store.ErrNotFound
	}
	return s, nil
}

// List returns the open sessions of the owner's space.
func (m *Manager) List(ctx context.Context, owner, spaceID string) ([]store.Session, error) {
	if _, err := m.d.Spaces.Get(ctx, owner, spaceID); err != nil {
		return nil, err
	}
	return m.d.DB.OpenSessions(ctx, store.SessionFilter{SpaceID: spaceID, Owner: owner})
}

// All returns every open session (administrators).
func (m *Manager) All(ctx context.Context) ([]store.Session, error) {
	return m.d.DB.OpenSessions(ctx, store.SessionFilter{})
}

// End ends one of the owner's sessions. by is who ended it (recorded).
func (m *Manager) End(ctx context.Context, owner, spaceID, id, by string) (store.Session, error) {
	s, err := m.Get(ctx, owner, spaceID, id)
	if err != nil {
		return store.Session{}, err
	}
	return m.end(ctx, s, "ended by "+by, by)
}

// EndAny ends any session (administrators).
func (m *Manager) EndAny(ctx context.Context, id, by, reason string) (store.Session, error) {
	s, err := m.d.DB.GetSession(ctx, id)
	if err != nil {
		return store.Session{}, err
	}
	if reason == "" {
		reason = "ended by an administrator"
	}
	return m.end(ctx, s, reason, by)
}

// EndSpace ends every open session of a space (before it is deleted).
func (m *Manager) EndSpace(ctx context.Context, spaceID, by string) error {
	open, err := m.d.DB.OpenSessions(ctx, store.SessionFilter{SpaceID: spaceID})
	if err != nil {
		return err
	}
	for _, s := range open {
		if _, err := m.end(ctx, s, "the space was deleted", by); err != nil {
			return err
		}
	}
	return nil
}

// end ends a session: its container stopped and its changes written back,
// then its record ended. Here if this process holds it; otherwise by the
// worker holding it, asked to (so the answer comes once the changes are
// in). If that worker can't be reached, the record is ended, and the
// worker stops the session when it next looks.
func (m *Manager) end(ctx context.Context, s store.Session, reason, by string) (store.Session, error) {
	if !s.Open() {
		return s, nil
	}
	if l := m.get(s.ID); l != nil {
		l.end(ctx, reason, by)
		return m.d.DB.GetSession(ctx, s.ID)
	}
	if s.Worker != "" {
		_, err := m.remoteAction(ctx, s, "end", url.Values{"reason": {reason}, "by": {by}})
		if err == nil {
			return m.d.DB.GetSession(ctx, s.ID)
		}
		m.d.Log.Warn("ask the worker holding a session to end it; ending its record", "session_id", s.ID, "worker", s.Worker, "error", err)
	}
	if _, err := m.d.DB.EndSession(ctx, s.ID, reason, by, m.d.Now()); err != nil {
		return store.Session{}, err
	} else if err := m.d.DB.Notify(ctx, store.ChanSessions, s.ID); err != nil {
		m.d.Log.Warn("tell the workers a session ended", "session_id", s.ID, "error", err)
	}
	return m.d.DB.GetSession(ctx, s.ID)
}

// Attach connects a client to one of the owner's open sessions: here if
// this process holds it, otherwise through the worker that does. A session
// still starting is waited for.
func (m *Manager) Attach(ctx context.Context, owner, spaceID, id string) (Conn, store.Session, error) {
	s, err := m.Get(ctx, owner, spaceID, id)
	if err != nil {
		return nil, store.Session{}, err
	}
	l, s, err := m.find(ctx, s)
	if err != nil {
		return nil, s, err
	}
	if l != nil {
		return l.subscribe(), s, nil
	}
	c, err := m.remoteAttach(ctx, s)
	return c, s, err
}

// Interrupt stops the cell an owner's session is running.
func (m *Manager) Interrupt(ctx context.Context, owner, spaceID, id string) error {
	return m.act(ctx, owner, spaceID, id, "interrupt", func(l *live) error { return l.control("interrupt_request", nil) })
}

// Restart starts the session's kernel again: its variables are gone.
func (m *Manager) Restart(ctx context.Context, owner, spaceID, id string) error {
	return m.act(ctx, owner, spaceID, id, "restart", func(l *live) error {
		return l.control("shutdown_request", map[string]any{"restart": true})
	})
}

// SyncFiles writes a read-write session's file changes to the space now.
func (m *Manager) SyncFiles(ctx context.Context, owner, spaceID, id string) ([]store.Conflict, error) {
	s, err := m.Get(ctx, owner, spaceID, id)
	if err != nil {
		return nil, err
	}
	if s.Mode != store.SessionReadWrite {
		return nil, &InvalidError{Msg: "a read-only session has no changes to write back"}
	}
	l, s, err := m.find(ctx, s)
	if err != nil {
		return nil, err
	}
	if l != nil {
		return l.sync(ctx)
	}
	body, err := m.remoteAction(ctx, s, "sync", nil)
	if err != nil {
		return nil, err
	}
	return conflictsFrom(body)
}

// act does something to a session, here or through its worker.
func (m *Manager) act(ctx context.Context, owner, spaceID, id, action string, here func(*live) error) error {
	s, err := m.Get(ctx, owner, spaceID, id)
	if err != nil {
		return err
	}
	l, s, err := m.find(ctx, s)
	if err != nil {
		return err
	}
	if l != nil {
		return here(l)
	}
	_, err = m.remoteAction(ctx, s, action, nil)
	return err
}

// find locates an open session: the live one if this process holds it,
// else its record once a worker holds it and it is ready (nil live: reach
// it through that worker). A session still starting is waited for.
func (m *Manager) find(ctx context.Context, s store.Session) (*live, store.Session, error) {
	if !s.Open() {
		return nil, s, ErrEnded
	}
	if l := m.get(s.ID); l != nil {
		return l, s, nil
	}
	if s.Worker == m.cfg.WorkerID && m.Holds() || s.Worker == "" && !m.cfg.Shared && m.Holds() {
		l, err := m.waitLive(ctx, s.ID)
		return l, s, err
	}
	if !m.cfg.Shared {
		return nil, s, ErrNotHere
	}
	s, err := m.waitReady(ctx, s.ID)
	if err != nil {
		return nil, s, err
	}
	if s.Worker == m.cfg.WorkerID && m.Holds() {
		l, err := m.waitLive(ctx, s.ID)
		return l, s, err
	}
	return nil, s, nil
}

// waitReady waits for a session to be ready on some worker.
func (m *Manager) waitReady(ctx context.Context, id string) (store.Session, error) {
	ctx, cancel := context.WithTimeout(ctx, m.cfg.StartTimeout)
	defer cancel()
	t := time.NewTicker(200 * time.Millisecond)
	defer t.Stop()
	for {
		s, err := m.d.DB.GetSession(ctx, id)
		switch {
		case err != nil:
			return s, err
		case !s.Open():
			return s, fmt.Errorf("%w: %s", ErrEnded, s.EndReason)
		case s.Status == store.SessionReady && s.Worker != "":
			return s, nil
		}
		select {
		case <-ctx.Done():
			return s, fmt.Errorf("the session hasn't started: %w", ctx.Err())
		case <-t.C:
		}
	}
}

func (m *Manager) get(id string) *live {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.live[id]
}

// waitLive waits for a session this process is starting.
func (m *Manager) waitLive(ctx context.Context, id string) (*live, error) {
	ctx, cancel := context.WithTimeout(ctx, m.cfg.StartTimeout)
	defer cancel()
	m.Wake()
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		if l := m.get(id); l != nil {
			return l, nil
		}
		select {
		case <-ctx.Done():
			s, err := m.d.DB.GetSession(context.WithoutCancel(ctx), id)
			if err == nil && !s.Open() {
				return nil, fmt.Errorf("%w: %s", ErrEnded, s.EndReason)
			}
			return nil, fmt.Errorf("the session hasn't started: %w", ctx.Err())
		case <-t.C:
		}
		if s, err := m.d.DB.GetSession(ctx, id); err == nil && !s.Open() {
			return nil, fmt.Errorf("%w: %s", ErrEnded, s.EndReason)
		}
	}
}
