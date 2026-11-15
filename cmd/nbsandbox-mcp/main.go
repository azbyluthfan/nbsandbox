// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

// Command nbsandbox-mcp is a Model Context Protocol server for nbsandbox:
// AI agents (Claude, IDE assistants) use its tools to write notebooks into
// a space, run them in the sandbox and read the results, as one user.
//
// Local, over stdio (the agent starts it), as the user in the environment:
//
//	NBSANDBOX_URL=https://nbsandbox.example.com NBSANDBOX_TOKEN=... nbsandbox-mcp
//
// Shared, over HTTP (streamable HTTP, stateless), where every request
// carries its caller's own Authorization header and nbsandbox checks it:
//
//	NBSANDBOX_URL=http://nbsandbox-api:8080 nbsandbox-mcp -http 127.0.0.1:8095
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/azbyluthfan/nbsandbox/internal/apiclient"
	"github.com/azbyluthfan/nbsandbox/internal/mcpserver"
)

var version = "dev"

func main() {
	httpAddr := flag.String("http", "", "serve MCP over streamable HTTP on this address (e.g. 127.0.0.1:8095) instead of stdio; each request must carry its caller's Authorization header")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	// stdout is the protocol on stdio: logs go to stderr.
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(*httpAddr, log); err != nil {
		log.Error("nbsandbox-mcp", "error", err)
		os.Exit(1)
	}
}

func run(httpAddr string, log *slog.Logger) error {
	base := strings.TrimSuffix(os.Getenv("NBSANDBOX_URL"), "/")
	u, err := url.Parse(base)
	if base == "" || err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("set NBSANDBOX_URL to nbsandbox's address, e.g. https://nbsandbox.example.com")
	}
	if u.Scheme == "http" && !privateHost(u.Hostname()) {
		return fmt.Errorf("refusing to send credentials over plain http to %s: use https", u.Host)
	}
	client := &apiclient.Client{BaseURL: base, HTTP: &http.Client{Timeout: 90 * time.Second}}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if httpAddr == "" {
		who, err := envIdentity()
		if err != nil {
			return err
		}
		s := mcpserver.New(mcpserver.Options{Client: client, Identity: who, Version: version})
		return s.Run(ctx, &mcp.StdioTransport{})
	}

	if os.Getenv("NBSANDBOX_TOKEN") != "" || os.Getenv("NBSANDBOX_PASSWORD") != "" {
		return errors.New("over HTTP each caller sends its own credentials: unset NBSANDBOX_TOKEN and NBSANDBOX_PASSWORD")
	}
	s := mcpserver.New(mcpserver.Options{Client: client, Version: version})
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, Logger: log})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.Handle("/mcp", requireAuth(mcpHandler))
	srv := &http.Server{Addr: httpAddr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", httpAddr)
	if err != nil {
		return err
	}
	log.Info("serving MCP over HTTP", "addr", ln.Addr().String(), "path", "/mcp", "nbsandbox", base)
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(sctx)
	}
}

// requireAuth refuses requests without credentials before any MCP work:
// nbsandbox checks them on every tool call; here only their presence.
func requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="nbsandbox"`)
			http.Error(w, "send nbsandbox credentials in the Authorization header", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// envIdentity is the stdio user: NBSANDBOX_TOKEN, or NBSANDBOX_USER and
// NBSANDBOX_PASSWORD.
func envIdentity() (*apiclient.Identity, error) {
	tok, user, pw := os.Getenv("NBSANDBOX_TOKEN"), os.Getenv("NBSANDBOX_USER"), os.Getenv("NBSANDBOX_PASSWORD")
	switch {
	case tok != "" && pw == "":
		return &apiclient.Identity{Token: tok}, nil
	case tok == "" && user != "" && pw != "":
		return &apiclient.Identity{Name: user, Password: pw}, nil
	}
	return nil, errors.New("set NBSANDBOX_TOKEN, or NBSANDBOX_USER and NBSANDBOX_PASSWORD")
}

// privateHost: plain http is accepted for loopback and in-cluster names
// (a Service such as nbsandbox-api, or *.svc.cluster.local).
func privateHost(h string) bool {
	if h == "localhost" || strings.HasSuffix(h, ".localhost") || !strings.Contains(h, ".") || strings.HasSuffix(h, ".svc") ||
		strings.HasSuffix(h, ".svc.cluster.local") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate())
}
