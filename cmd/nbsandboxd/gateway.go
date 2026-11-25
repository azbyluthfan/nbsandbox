// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/azbyluthfan/nbsandbox/internal/config"
	"github.com/azbyluthfan/nbsandbox/internal/gateway"
	"github.com/azbyluthfan/nbsandbox/internal/obs"
)

// gatewayCmd runs the egress gateway: the only way out for runs' fetchers.
// It needs only the key nbsandboxd signs run tokens with.
func gatewayCmd(args []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	return runGateway(ctx, args, os.Getenv, os.Stderr)
}

func runGateway(ctx context.Context, args []string, getenv func(string) string, logOut io.Writer) error {
	fs := flag.NewFlagSet("gateway", flag.ContinueOnError)
	listen := fs.String("listen", envOr(getenv, "NBSANDBOX_GATEWAY_LISTEN", ":3128"), "listen address (env NBSANDBOX_GATEWAY_LISTEN)")
	level := fs.String("log-level", envOr(getenv, "NBSANDBOX_LOG_LEVEL", "info"), "log level (env NBSANDBOX_LOG_LEVEL)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	key, err := config.GatewayKey(getenv("NBSANDBOX_GATEWAY_KEY"))
	if err != nil {
		return fmt.Errorf("NBSANDBOX_GATEWAY_KEY: %w", err)
	}
	logger, err := obs.NewLogger(logOut, *level)
	if err != nil {
		return err
	}
	logger.Info("nbsandboxd gateway starting", "version", version)
	return gateway.Serve(ctx, *listen, &gateway.Server{Key: key, Log: logger})
}

func envOr(getenv func(string) string, k, def string) string {
	if v := getenv(k); v != "" {
		return v
	}
	return def
}
