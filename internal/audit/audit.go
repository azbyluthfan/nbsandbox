// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

// Package audit records who did what, when, and how it ended: every change,
// every download and every administrative operation, refused ones included.
// The transports call the Recorder from one place each (the REST route
// wrapper, which also serves the web UI, and the gRPC interceptor), so no
// handler can forget; handlers only add what the request doesn't say, such
// as the ID of a run they created (SetTarget).
package audit

import (
	"context"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/azbyluthfan/nbsandbox/internal/auth"
	"github.com/azbyluthfan/nbsandbox/internal/store"
)

// Store keeps the audit log (store.DB).
type Store interface {
	InsertAudit(ctx context.Context, e store.AuditEvent) error
	ListAudit(ctx context.Context, f store.AuditFilter, cursor string, limit int) ([]store.AuditEvent, string, error)
	DeleteAuditBefore(ctx context.Context, t time.Time) (int64, error)
}

// Config chooses what is recorded and for how long.
type Config struct {
	Enabled   bool
	Retention time.Duration
	Downloads bool // also record downloads of files and run outputs
}

// Recorder writes audit rows. A nil Recorder records nothing.
type Recorder struct {
	cfg      Config
	db       Store
	log      *slog.Logger
	failures prometheus.Counter
	now      func() time.Time
}

// New returns a recorder. failures may be nil.
func New(cfg Config, db Store, log *slog.Logger, failures prometheus.Counter) *Recorder {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Recorder{cfg: cfg, db: db, log: log, failures: failures, now: time.Now}
}

// The operations recorded: every change, every administrative operation,
// and (with Downloads) every download. Listings and streams are not.
var (
	changes = map[auth.Op]bool{
		auth.OpCreateSpace: true, auth.OpUpdateSpace: true, auth.OpDeleteSpace: true,
		auth.OpUploadFiles: true, auth.OpPutFile: true, auth.OpMoveFile: true, auth.OpDeleteFile: true, auth.OpCreateFolder: true,
		auth.OpPublishFunction: true, auth.OpDeleteFunction: true, auth.OpInvokeFunction: true, auth.OpInvokeFunctionStream: true,
		auth.OpCreateRun: true, auth.OpCancelRun: true, auth.OpDeleteRun: true, auth.OpPromoteArtifact: true,
		auth.OpBuildSpacePackages: true,
		// A session's start and end, interrupts and restarts; never the code
		// its cells run.
		auth.OpOpenSession: true, auth.OpEndSession: true, auth.OpInterruptSession: true, auth.OpRestartSession: true,
		auth.OpSyncSession: true,
		// Connections: who created, changed, deleted or bound them; never
		// their secrets.
		auth.OpCreateConnection: true, auth.OpUpdateConnection: true, auth.OpDeleteConnection: true,
		auth.OpSetSpaceConnections: true,
	}
	downloads = map[auth.Op]bool{
		auth.OpGetFile: true, auth.OpGetRunNotebook: true, auth.OpGetRunHTML: true, auth.OpGetArtifact: true, auth.OpGetRunLog: true,
	}
)

// Admin operations are all recorded, reads included.
func isAdmin(op auth.Op) bool { return op.Scope() == auth.ScopeAdmin }

// Audited reports whether op is recorded.
func (r *Recorder) Audited(op auth.Op) bool {
	if r == nil {
		return false
	}
	return r.cfg.Enabled && (changes[op] || isAdmin(op) || (r.cfg.Downloads && downloads[op]))
}

// List returns audit rows, newest first (store.DB.ListAudit). It works
// while recording is off, for the rows kept from before.
func (r *Recorder) List(ctx context.Context, f store.AuditFilter, cursor string, limit int) ([]store.AuditEvent, string, error) {
	if r == nil {
		return nil, "", nil
	}
	return r.db.ListAudit(ctx, f, cursor, limit)
}

// The web UI's own actions (not API operations).
const (
	ActionSignIn  = "SignIn"
	ActionSignOut = "SignOut"
)

// Entry is what a transport knows about a finished operation.
type Entry struct {
	// Actor and Method name the caller when ctx carries none (a sign-out,
	// after the session is gone).
	Actor, Method string
	Action        string
	SpaceID       string
	Target        string
	Status        int // HTTP status (gRPC codes mapped by the caller)
}

// Record writes one row for ctx's caller. It never fails the request: a
// row that can't be written is logged as an error and counted.
func (r *Recorder) Record(ctx context.Context, e Entry) {
	if r == nil || !r.cfg.Enabled {
		return
	}
	p, _ := auth.From(ctx)
	if e.Actor != "" {
		p.ID, p.Method = e.Actor, e.Method
	}
	n := noteOf(ctx)
	if e.Target == "" {
		e.Target = n.target
	}
	row := store.AuditEvent{At: r.now(), Actor: p.ID, Method: p.Method, Client: p.Client, Via: ViaOf(ctx),
		Action: e.Action, SpaceID: e.SpaceID, Target: e.Target, Detail: n.detail, Status: e.Status, Address: auth.ClientAddr(ctx)}
	if row.SpaceID == "" {
		row.SpaceID = n.space
	}
	r.log.Info("audit", "actor", row.Actor, "action", row.Action, "space_id", row.SpaceID, "target", row.Target,
		"detail", row.Detail, "status", row.Status, "via", row.Via, "client", row.Client, "address", row.Address)
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := r.db.InsertAudit(wctx, row); err != nil {
		r.log.Error("audit row not written", "err", err, "actor", row.Actor, "action", row.Action)
		if r.failures != nil {
			r.failures.Inc()
		}
	}
}

// Run removes rows older than the retention every interval until ctx ends.
func (r *Recorder) Run(ctx context.Context, every time.Duration) {
	if r == nil {
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if n, err := r.db.DeleteAuditBefore(ctx, r.now().Add(-r.cfg.Retention)); err != nil {
			if ctx.Err() == nil {
				r.log.Warn("removing old audit rows failed", "err", err)
			}
		} else if n > 0 {
			r.log.Debug("old audit rows removed", "count", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ---- what the request carries ----

type viaKey struct{}

// WithVia records the transport: "rest" (the default), "grpc" or "ui".
func WithVia(ctx context.Context, via string) context.Context {
	return context.WithValue(ctx, viaKey{}, via)
}

// ViaOf is the transport recorded by WithVia.
func ViaOf(ctx context.Context) string {
	if v, ok := ctx.Value(viaKey{}).(string); ok {
		return v
	}
	return "rest"
}

// note is what a handler adds to its audit row.
type note struct{ target, detail, space string }

type noteKey struct{}

// WithNote gives the request a note its handler can fill (SetTarget,
// SetDetail, SetSpace); the transport does this before calling it.
func WithNote(ctx context.Context) context.Context {
	return context.WithValue(ctx, noteKey{}, &note{})
}

func noteOf(ctx context.Context) note {
	if n, ok := ctx.Value(noteKey{}).(*note); ok && n != nil {
		return *n
	}
	return note{}
}

func edit(ctx context.Context, f func(*note)) {
	if n, ok := ctx.Value(noteKey{}).(*note); ok && n != nil {
		f(n)
	}
}

// SetTarget names what the operation acted on, when the request doesn't
// (the run a request created).
func SetTarget(ctx context.Context, target string) { edit(ctx, func(n *note) { n.target = target }) }

// SetDetail adds a detail (the new owner of a space).
func SetDetail(ctx context.Context, detail string) { edit(ctx, func(n *note) { n.detail = detail }) }

// SetSpace names the space, when the request doesn't (a space it created).
func SetSpace(ctx context.Context, spaceID string) { edit(ctx, func(n *note) { n.space = spaceID }) }
