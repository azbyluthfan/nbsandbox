// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// Server is the gateway: HTTP CONNECT tunnels (TLS, databases) and plain
// HTTP forwarding (object storage over http), each allowed by the caller's
// run token.
type Server struct {
	Key []byte
	Log *slog.Logger
	// Resolver looks names up (default: net.DefaultResolver).
	Resolver interface {
		LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
	}
	// Dial connects to an address (default: a net.Dialer with a timeout).
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	Now  func() time.Time
	// AllowInternal lets tests reach loopback addresses. Never in service.
	AllowInternal bool
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Server) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.New(slog.DiscardHandler)
}

// token reads the run token: "Bearer <token>", or Basic with the token as
// the password (for clients that only speak a proxy's basic auth).
func token(r *http.Request) string {
	h := r.Header.Get("Proxy-Authorization")
	if t, ok := strings.CutPrefix(h, "Bearer "); ok {
		return strings.TrimSpace(t)
	}
	if b, ok := strings.CutPrefix(h, "Basic "); ok {
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b))
		if err == nil {
			if _, pass, ok := strings.Cut(string(raw), ":"); ok {
				return pass
			}
		}
	}
	return ""
}

// internal reports addresses no run may reach, whatever its token says:
// loopback, link-local (cloud metadata), unspecified, multicast.
func internal(ip netip.Addr) bool {
	ip = ip.Unmap()
	return ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}

// resolve looks host up and returns an address that isn't internal.
func (s *Server) resolve(ctx context.Context, hostport string) (string, error) {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return "", err
	}
	var addrs []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{ip}
	} else {
		var r interface {
			LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
		} = net.DefaultResolver
		if s.Resolver != nil {
			r = s.Resolver
		}
		if addrs, err = r.LookupNetIP(ctx, "ip", host); err != nil {
			return "", err
		}
	}
	for _, a := range addrs {
		if !internal(a) || s.AllowInternal {
			return net.JoinHostPort(a.Unmap().String(), port), nil
		}
	}
	return "", fmt.Errorf("%w: %s resolves only to internal addresses", errInternal, host)
}

// lookupReason says why a name couldn't be looked up, for the run's user:
// not the resolver's address, which the error carries (the log has it).
func lookupReason(to string, err error) string {
	host, _, _ := net.SplitHostPort(to)
	var dns *net.DNSError
	if errors.As(err, &dns) && dns.IsNotFound {
		return host + " doesn't resolve (no such host)"
	}
	return "can't look up " + host
}

// errInternal is a target refused on purpose (loopback, link-local, ...),
// unlike one that can't be looked up.
var errInternal = errors.New("refused")

func (s *Server) dial(ctx context.Context, addr string) (net.Conn, error) {
	if s.Dial != nil {
		return s.Dial(ctx, "tcp", addr)
	}
	d := net.Dialer{Timeout: 10 * time.Second}
	return d.DialContext(ctx, "tcp", addr)
}

// target is where a request wants to go, as host:port.
func target(r *http.Request) string {
	if r.Method == http.MethodConnect {
		return strings.ToLower(r.Host)
	}
	host := strings.ToLower(r.URL.Host)
	if _, _, err := net.SplitHostPort(host); err != nil {
		port := "80"
		if r.URL.Scheme == "https" {
			port = "443"
		}
		host = net.JoinHostPort(host, port)
	}
	return host
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	to := target(r)
	c, err := Verify(s.Key, token(r), s.now())
	if err != nil {
		s.log().Warn("gateway refused", "to", to, "from", r.RemoteAddr, "reason", "no valid run token")
		w.Header().Set("Proxy-Authenticate", `Bearer realm="nbsandbox"`)
		http.Error(w, "a valid run token is needed", http.StatusProxyAuthRequired)
		return
	}
	if r.Method != http.MethodConnect && (r.URL.Host == "" || r.URL.Scheme != "http") {
		http.Error(w, "CONNECT, or an http:// URL", http.StatusBadRequest)
		return
	}
	if !c.Allows(to) {
		s.log().Warn("gateway refused", "run", c.Run, "to", to, "reason", "not one of the run's endpoints")
		http.Error(w, "not one of this run's endpoints", http.StatusForbidden)
		return
	}
	addr, err := s.resolve(r.Context(), to)
	if err != nil {
		// Refused on purpose: 403. A name that doesn't resolve: 502, the
		// lookup's error as the reason (the fetcher shows it to the user).
		code, reason := http.StatusBadGateway, lookupReason(to, err)
		if errors.Is(err, errInternal) {
			code, reason = http.StatusForbidden, err.Error()
		}
		s.log().Warn("gateway refused", "run", c.Run, "to", to, "reason", err.Error())
		http.Error(w, reason, code)
		return
	}
	if r.Method == http.MethodConnect {
		s.tunnel(w, r, c.Run, to, addr)
		return
	}
	s.forward(w, r, c.Run, to, addr)
}

func (s *Server) tunnel(w http.ResponseWriter, r *http.Request, run, to, addr string) {
	up, err := s.dial(r.Context(), addr)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		_ = up.Close()
		http.Error(w, "tunnels need HTTP/1.1", http.StatusInternalServerError)
		return
	}
	down, buf, err := hj.Hijack()
	if err != nil {
		_ = up.Close()
		return
	}
	start := time.Now()
	s.log().Info("gateway tunnel", "run", run, "to", to, "addr", addr)
	_, _ = down.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
	var wg sync.WaitGroup
	var sent, received int64
	wg.Add(1)
	go func() {
		defer wg.Done()
		sent, _ = io.Copy(up, buf) // what the client already buffered, then the rest
		closeWrite(up)
	}()
	received, _ = io.Copy(down, up)
	closeWrite(down)
	wg.Wait()
	_ = up.Close()
	_ = down.Close()
	s.log().Info("gateway tunnel closed", "run", run, "to", to, "sent", sent, "received", received, "seconds", time.Since(start).Seconds())
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = c.Close()
}

func (s *Server) forward(w http.ResponseWriter, r *http.Request, run, to, addr string) {
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host = "http", to
			pr.Out.Host = r.Host
			pr.Out.Header.Del("Proxy-Authorization")
			pr.Out.Header.Del("Proxy-Connection")
		},
		Transport: &http.Transport{
			Proxy:             nil,
			DisableKeepAlives: true, // one transport per request: keep nothing open
			// Connect to the address checked above, not a fresh lookup.
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return s.dial(ctx, addr) },
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			http.Error(w, err.Error(), http.StatusBadGateway)
		},
	}
	s.log().Info("gateway request", "run", run, "to", to, "method", r.Method, "path", r.URL.Path)
	rp.ServeHTTP(w, r)
}

// Serve runs the gateway on addr until ctx ends.
func Serve(ctx context.Context, addr string, s *Server) error {
	srv := &http.Server{Addr: addr, Handler: s, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	s.log().Info("gateway listening", "addr", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
