// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package obs

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// NewLogger returns a JSON slog logger at the given level ("debug", "info",
// "warn", "error") that redacts sensitive attributes.
func NewLogger(w io.Writer, level string) (*slog.Logger, error) {
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("log level: %w", err)
	}
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lv, ReplaceAttr: redact})
	return slog.New(h), nil
}

// sensitiveKeys are attribute names whose values never reach the log
// : credentials, and notebook parameters, which may hold user data.
var sensitiveKeys = map[string]bool{
	"authorization": true,
	"password":      true,
	"parameters":    true,
	"cookie":        true,
}

func redact(_ []string, a slog.Attr) slog.Attr {
	if sensitiveKeys[strings.ToLower(a.Key)] {
		return slog.String(a.Key, "[redacted]")
	}
	return a
}

type ctxKey struct{}

// WithLogger stores a logger in the context.
func WithLogger(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, l)
}

// Logger returns the context's logger, or slog.Default().
func Logger(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(ctxKey{}).(*slog.Logger); ok {
		return l
	}
	return slog.Default()
}
