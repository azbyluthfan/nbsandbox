// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Notification channels (PostgreSQL LISTEN/NOTIFY). Notifications only wake
// nodes up early: everything they announce is in the tables, and every
// waiter also re-reads on a timer, so a lost notification costs a delay, not
// correctness.
const (
	ChanEvents = "events" // payload: a space ID that has a new status event
	ChanQueued = "queued" // a run was queued: workers try to claim one
	ChanCancel = "cancel" // payload: a run ID whose cancel was requested
	ChanLogs   = "logs"   // payload: a run ID with new live log lines
)

func (db *DB) channel(name string) string { return db.prefix + "_" + name }

// Notify sends a notification now.
func (db *DB) Notify(ctx context.Context, channel, payload string) error {
	_, err := db.w.ExecContext(ctx, "SELECT pg_notify(?, ?)", db.channel(channel), payload)
	return err
}

// notifyTx sends a notification when tx commits (none if it rolls back).
func (db *DB) notifyTx(ctx context.Context, tx txn, channel, payload string) error {
	_, err := tx.ExecContext(ctx, "SELECT pg_notify(?, ?)", db.channel(channel), payload)
	return err
}

// Listen delivers notifications on the given channels to fn until ctx ends,
// on a connection of its own, reconnecting after a failure. After every
// (re)connection it calls fn("", ""): notifications may have been missed,
// so waiters should re-read.
func (db *DB) Listen(ctx context.Context, log *slog.Logger, fn func(channel, payload string), channels ...string) {
	backoff := time.Second
	for ctx.Err() == nil {
		err := db.listenOnce(ctx, fn, channels)
		if ctx.Err() != nil {
			return
		}
		log.Warn("database notifications interrupted; reconnecting", "error", err, "retry_in", backoff.String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func (db *DB) listenOnce(ctx context.Context, fn func(channel, payload string), channels []string) error {
	conn, err := pgx.Connect(ctx, db.url)
	if err != nil {
		return err
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		_ = conn.Close(cctx)
	}()
	byName := map[string]string{}
	for _, c := range channels {
		full := db.channel(c)
		byName[full] = c
		if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{full}.Sanitize()); err != nil {
			return err
		}
	}
	fn("", "")
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		if c, ok := byName[n.Channel]; ok {
			fn(c, n.Payload)
		} else if strings.HasPrefix(n.Channel, db.prefix+"_") {
			fn(strings.TrimPrefix(n.Channel, db.prefix+"_"), n.Payload)
		}
	}
}
