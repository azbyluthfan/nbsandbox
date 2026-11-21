// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package sessions

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/isolation"
	"github.com/azbyluthfan/nbsandbox/internal/store"
)

// Start begins holding sessions, on a worker: it re-attaches to the
// sessions this worker held before (ending those whose container is gone),
// removes session containers nobody holds, then takes sessions as they are
// opened. It returns at once; Shutdown stops it.
func (m *Manager) Start(ctx context.Context) error {
	if !m.Holds() {
		return nil
	}
	if err := m.reconcile(ctx); err != nil {
		m.d.Log.Warn("re-attach sessions", "error", err)
	}
	bg := context.WithoutCancel(ctx)
	m.done.Add(1)
	go func() {
		defer m.done.Done()
		m.loop(bg)
	}()
	return nil
}

// Shutdown lets go of the sessions without ending them: their containers
// keep running, and the next start re-attaches to them. A read-write
// session's changes are written back first.
func (m *Manager) Shutdown(ctx context.Context) {
	m.once.Do(func() { close(m.stop) })
	m.done.Wait()
	m.mu.Lock()
	held := make([]*live, 0, len(m.live))
	for _, l := range m.live {
		held = append(held, l)
	}
	m.live = map[string]*live{}
	m.mu.Unlock()
	for _, l := range held {
		if l.s.Mode == store.SessionReadWrite {
			_, _ = l.sync(ctx)
		}
		l.detach()
	}
}

func (m *Manager) loop(ctx context.Context) {
	tick := time.NewTicker(m.cfg.Tick)
	defer tick.Stop()
	lastWriteBack := m.d.Now()
	lastCleanup := time.Time{}
	for {
		m.claimAll(ctx)
		select {
		case <-m.stop:
			return
		case <-m.wake:
		case <-tick.C:
			m.housekeep(ctx)
			if now := m.d.Now(); now.Sub(lastWriteBack) >= m.cfg.WriteBack {
				lastWriteBack = now
				m.writeBackAll(ctx)
			}
			if now := m.d.Now(); now.Sub(lastCleanup) >= time.Hour {
				lastCleanup = now
				if _, err := m.d.DB.DeleteEndedSessionsBefore(ctx, now.Add(-m.cfg.Retention)); err != nil {
					m.d.Log.Warn("remove old session records", "error", err)
				}
			}
		}
	}
}

// claimAll starts every session waiting for a worker that this worker has
// room for.
func (m *Manager) claimAll(ctx context.Context) {
	for !m.d.Draining() {
		s, ok, err := m.d.DB.ClaimSession(ctx, m.cfg.WorkerID, m.cfg.PerWorker)
		if err != nil {
			m.d.Log.Warn("take a session", "error", err)
			return
		}
		if !ok {
			return
		}
		m.done.Add(1)
		go func() {
			defer m.done.Done()
			m.launch(ctx, s)
		}()
	}
}

// launch starts a session's container and holds it once its kernel is
// ready; a session that can't start is ended with the reason.
func (m *Manager) launch(ctx context.Context, s store.Session) {
	log := m.d.Log.With("session_id", s.ID, "space_id", s.SpaceID)
	fail := func(reason string) {
		log.Warn("session could not start", "reason", reason)
		m.d.Spaces.Discard(s.SpaceID, s.ID)
		if _, err := m.d.DB.EndSession(ctx, s.ID, reason, "", m.d.Now()); err != nil {
			log.Error("record a session's end", "error", err)
		}
	}
	sctx, cancel := context.WithTimeout(ctx, m.cfg.StartTimeout)
	defer cancel()
	go func() { // nbsandboxd stopping: a session still starting ends
		select {
		case <-m.stop:
			cancel()
		case <-sctx.Done():
		}
	}()
	dir, err := m.d.Spaces.Checkout(sctx, s.SpaceID, s.ID)
	if err != nil {
		fail(fmt.Sprintf("could not prepare the space's files: %v", err))
		return
	}
	var pkgDir, note string
	if m.d.Packages != nil {
		if pkgDir, note, err = m.d.Packages(sctx, s.SpaceID); err != nil {
			pkgDir, note = "", fmt.Sprintf("the session started without the space's packages: %v", err)
		}
	}
	conn, err := m.d.Backend.StartSession(sctx, isolation.SessionSpec{
		SessionID: s.ID, SpaceID: s.SpaceID, SpaceDir: dir, SpaceReadOnly: s.Mode == store.SessionReadOnly,
		Image: m.cfg.Image, Runtime: m.cfg.Runtime, Limits: m.cfg.Limits, PackagesDir: pkgDir, MaxMessage: m.cfg.MaxMessage,
	})
	if err != nil {
		fail(err.Error())
		return
	}
	l := newLive(m, s, conn)
	ready := make(chan error, 1)
	go m.hold(ctx, l, ready)
	select {
	case err = <-ready:
	case <-sctx.Done():
		err = errors.New("its kernel didn't become ready in time")
	}
	if err != nil {
		_ = conn.Close()
		_ = m.d.Backend.StopSession(ctx, s.ID)
		fail(err.Error())
		return
	}
	if ok, err := m.d.DB.MarkSessionReady(ctx, s.ID); err != nil || !ok {
		// Ended while it started (by its owner, or the space was deleted).
		l.end(ctx, "ended while it started", "")
		return
	}
	l.s.Status = store.SessionReady
	m.mu.Lock()
	m.live[s.ID] = l
	m.mu.Unlock()
	if note != "" {
		l.notice(note)
	}
	log.Info("session started", "mode", s.Mode, "notebook", s.Notebook)
}

// hold relays a session's messages for as long as its connection lasts.
// A connection that breaks while the container still runs is attached again;
// a container that stopped ends the session.
func (m *Manager) hold(ctx context.Context, l *live, ready chan<- error) {
	for {
		starting := ready != nil
		err := l.read(ready)
		ready = nil
		l.mu.Lock()
		ended := l.ended
		l.mu.Unlock()
		if ended || starting && m.get(l.s.ID) == nil {
			return // ended, or never became ready (launch ends it)
		}
		if conn, aerr := m.reattach(ctx, l.s.ID); aerr == nil {
			l.mu.Lock()
			l.conn, l.readerDone = conn, make(chan struct{})
			l.mu.Unlock()
			_ = l.control("kernel_info_request", nil) // learn its state again (an answer comes on shell)
			continue
		}
		l.end(ctx, fmt.Sprintf("the session's container stopped (%v)", err), "")
		return
	}
}

// reattach attaches to a session's container again if it still runs.
func (m *Manager) reattach(ctx context.Context, id string) (isolation.SessionConn, error) {
	list, err := m.d.Backend.Sessions(ctx)
	if err != nil {
		return nil, err
	}
	for _, c := range list {
		if c.SessionID == id && c.Running {
			return m.d.Backend.AttachSession(ctx, id)
		}
	}
	return nil, errors.New("not running")
}

// housekeep ends sessions that are idle, past their maximum, or ended
// elsewhere (by their owner through another API node, an administrator, the
// space's deletion).
func (m *Manager) housekeep(ctx context.Context) {
	now := m.d.Now()
	m.mu.Lock()
	held := make([]*live, 0, len(m.live))
	for _, l := range m.live {
		held = append(held, l)
	}
	m.mu.Unlock()
	for _, l := range held {
		switch q := l.quiet(); {
		case now.Sub(l.s.CreatedAt) >= m.cfg.Max:
			l.end(ctx, fmt.Sprintf("it reached the maximum length (%s)", m.cfg.Max), "")
		case !q.IsZero() && now.Sub(q) >= m.cfg.Idle:
			l.end(ctx, fmt.Sprintf("idle for %s (no cell running, nobody connected)", m.cfg.Idle), "")
		default:
			if s, err := m.d.DB.GetSession(ctx, l.s.ID); errors.Is(err, store.ErrNotFound) || (err == nil && !s.Open()) {
				reason, by := "the space was deleted", ""
				if err == nil {
					reason, by = s.EndReason, s.EndedBy
				}
				l.end(ctx, reason, by)
			}
		}
	}
}

func (m *Manager) writeBackAll(ctx context.Context) {
	m.mu.Lock()
	held := make([]*live, 0, len(m.live))
	for _, l := range m.live {
		if l.s.Mode == store.SessionReadWrite {
			held = append(held, l)
		}
	}
	m.mu.Unlock()
	for _, l := range held {
		_, _ = l.sync(ctx)
	}
}

// reconcile re-attaches to the sessions whose containers are on this
// worker's Docker: those it held, and those it adopts (it restarted under
// another worker ID, as a recreated container gets a new host name; or,
// with other workers, the one that held it is gone). Its open sessions whose
// container is gone end, their changes kept; containers of sessions that
// ended are removed.
func (m *Manager) reconcile(ctx context.Context) error {
	containers, err := m.d.Backend.Sessions(ctx)
	if err != nil {
		return err
	}
	others, err := m.liveWorkers(ctx)
	if err != nil {
		return err
	}
	held := map[string]bool{}
	for _, c := range containers {
		log := m.d.Log.With("session_id", c.SessionID)
		s, err := m.d.DB.GetSession(ctx, c.SessionID)
		switch {
		case errors.Is(err, store.ErrNotFound) || (err == nil && !s.Open()):
			if err := m.d.Backend.StopSession(ctx, c.SessionID); err != nil {
				log.Warn("remove the container of a session that ended", "error", err)
			}
			continue
		case err != nil:
			return err
		}
		if s.Worker != m.cfg.WorkerID {
			if others[s.Worker] {
				continue // another worker on this Docker holds it
			}
			if s, err = m.d.DB.AdoptSession(ctx, s.ID, m.cfg.WorkerID); err != nil {
				continue
			}
		}
		if s.Status != store.SessionReady || !c.Running {
			continue // ends below, as a session whose container is gone
		}
		conn, err := m.d.Backend.AttachSession(ctx, s.ID)
		if err != nil {
			log.Warn("re-attach a session", "error", err)
			continue
		}
		l := newLive(m, s, conn)
		l.kernel = KernelIdle
		m.mu.Lock()
		m.live[s.ID] = l
		m.mu.Unlock()
		held[s.ID] = true
		go m.hold(context.WithoutCancel(ctx), l, nil)
		_ = l.control("kernel_info_request", nil)
		log.Info("session re-attached")
	}
	// This worker's sessions; a single process's are every claimed one,
	// whatever worker ID they were recorded under.
	filter := store.SessionFilter{Worker: m.cfg.WorkerID}
	if !m.cfg.Shared {
		filter = store.SessionFilter{}
	}
	open, err := m.d.DB.OpenSessions(ctx, filter)
	if err != nil {
		return err
	}
	for _, s := range open {
		if held[s.ID] || s.Worker == "" {
			continue
		}
		// Its container is gone (or it never finished starting): it ends,
		// with what it wrote kept.
		log := m.d.Log.With("session_id", s.ID)
		_ = m.d.Backend.StopSession(ctx, s.ID)
		if s.Mode == store.SessionReadWrite {
			if _, _, err := m.d.Spaces.Sync(ctx, s.SpaceID, s.ID); err != nil {
				log.Warn("write an interrupted session's changes back", "error", err)
			}
		}
		m.d.Spaces.Discard(s.SpaceID, s.ID)
		if _, err := m.d.DB.EndSession(ctx, s.ID, "interrupted: nbsandboxd restarted and its container was gone", "", m.d.Now()); err != nil {
			log.Error("record a session's end", "error", err)
		}
	}
	return nil
}

// liveWorkers are the other workers that may hold sessions on this Docker
// (none for a single process).
func (m *Manager) liveWorkers(ctx context.Context) (map[string]bool, error) {
	out := map[string]bool{}
	if !m.cfg.Shared || m.d.Workers == nil {
		return out, nil
	}
	ws, err := m.d.Workers(ctx)
	if err != nil {
		return nil, err
	}
	for _, w := range ws {
		if w.ID != m.cfg.WorkerID {
			out[w.ID] = true
		}
	}
	return out, nil
}

func (m *Manager) forget(id string) {
	m.mu.Lock()
	delete(m.live, id)
	m.mu.Unlock()
	m.Wake() // room for another
}

// Held is how many sessions this process holds.
func (m *Manager) Held() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.live)
}
