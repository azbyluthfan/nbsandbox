// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"log/slog"

	"github.com/azbyluthfan/nbsandbox/internal/config"
	"github.com/azbyluthfan/nbsandbox/internal/connections"
	"github.com/azbyluthfan/nbsandbox/internal/isolation/kubernetes"
	"github.com/azbyluthfan/nbsandbox/internal/secrets"
	"github.com/azbyluthfan/nbsandbox/internal/store"
)

// newConnections builds the connections service with its secret provider
// (nil when connections are turned off).
func newConnections(cfg config.Settings, db *store.DB, logger *slog.Logger) (*connections.Service, error) {
	c := cfg.Connections
	if !c.Enabled {
		return nil, nil
	}
	var provider secrets.Provider
	switch c.Secrets.Provider {
	case config.SecretsKubernetes:
		client, _, err := kubernetes.Connect(cfg.Kubernetes.Kubeconfig)
		if err != nil {
			return nil, err
		}
		ns := c.Secrets.Namespace
		if ns == "" {
			ns = cfg.Kubernetes.Namespace // the runs namespace: a Pod mounts only its own namespace's Secrets
		}
		provider = secrets.NewKubernetes(client, ns)
	default:
		b, err := secrets.NewBuiltin(c.Secrets.MasterKey, db)
		if err != nil {
			return nil, err
		}
		if cfg.Backend == "kubernetes" {
			logger.Warn("connections keep their secrets in the built-in store, which is for development: use the kubernetes provider in production")
		}
		provider = b
	}
	logger.Info("connections on", "secrets", provider.Name(), "allowed", c.Allowed)
	return connections.New(connections.Deps{DB: db, Secrets: provider, Allowed: connections.NewAllowed(c.Allowed), Log: logger}), nil
}
