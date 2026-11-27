// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"log/slog"
	"net"

	"github.com/azbyluthfan/nbsandbox/internal/config"
	"github.com/azbyluthfan/nbsandbox/internal/connections"
	"github.com/azbyluthfan/nbsandbox/internal/isolation/kubernetes"
	"github.com/azbyluthfan/nbsandbox/internal/secrets"
	"github.com/azbyluthfan/nbsandbox/internal/sessions"
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
	case config.SecretsVault:
		v := c.Secrets.Vault
		vp, err := secrets.NewVault(secrets.VaultConfig{Address: v.Address, Mount: v.Mount, Prefix: v.Prefix, AuthMount: v.AuthMount,
			Role: v.Role, FetcherRole: v.FetcherRole, Token: v.Token, References: v.References, CACertFile: v.CAFile})
		if err != nil {
			return nil, err
		}
		provider = vp
	default:
		b, err := secrets.NewBuiltin(c.Secrets.MasterKey, db)
		if err != nil {
			return nil, err
		}
		if cfg.Backend == "kubernetes" {
			logger.Warn("connections keep their secrets in the built-in store, which is for development: use the kubernetes provider in production")
		}
		if config.PublicDevKey(c.Secrets.MasterKey) {
			logger.Warn("connections.secrets.master_key is the repository's development key, which is public: anyone can decrypt the stored secrets; for local development only, never deploy it")
		}
		provider = b
	}
	logger.Info("connections on", "secrets", provider.Name(), "allowed", c.Allowed)
	key, err := config.GatewayKey(c.Gateway.Key)
	if err != nil {
		return nil, err
	}
	if config.PublicDevKey(c.Gateway.Key) {
		logger.Warn("connections.gateway.key is the repository's development key, which is public: anyone can sign gateway tokens; for local development only, never deploy it")
	}
	// Docker joins the gateway's container (the address's host) to each
	// run's network; on Kubernetes the worker resolves the whole address.
	gw, _, _ := net.SplitHostPort(c.Gateway.Address)
	if cfg.Backend == "kubernetes" {
		gw = c.Gateway.Address
	}
	fs := connections.FetcherSettings{Image: c.FetcherImage, GatewayAddress: c.Gateway.Address, GatewayContainer: gw, GatewayKey: key,
		MemoryBytes: int64(c.FetcherMemory), NanoCPUs: int64(c.FetcherCPUs * 1e9),
		ServiceAccount: c.Secrets.Vault.FetcherServiceAccount, FetcherRole: c.Secrets.Vault.FetcherRole}
	return connections.New(connections.Deps{DB: db, Secrets: provider, Allowed: connections.NewAllowed(c.Allowed), Fetcher: fs, Log: logger}), nil
}

// sessionData is the connections service for sessions (a nil interface,
// not a nil pointer, when connections are off).
func sessionData(c *connections.Service) sessions.DataAccess {
	if c == nil {
		return nil
	}
	return c
}
