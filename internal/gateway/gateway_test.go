// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"
)

var testKey = []byte("0123456789abcdef0123456789abcdef")

func TestToken(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	allow := []string{"DB.Example.com:5432"}
	tok := Sign(testKey, Claims{Run: "r1", Allow: allow, Exp: now.Unix() + 60})
	if allow[0] != "DB.Example.com:5432" {
		t.Fatal("Sign changed the caller's slice")
	}
	c, err := Verify(testKey, tok, now)
	if err != nil || c.Run != "r1" || !c.Allows("db.example.COM:5432") || c.Allows("db.example.com:5433") {
		t.Fatalf("verify: %+v %v", c, err)
	}
	if _, err := Verify(testKey, tok, now.Add(2*time.Minute)); !errors.Is(err, ErrToken) {
		t.Fatal("an expired token passed")
	}
	if _, err := Verify([]byte("another key, another key, anothe"), tok, now); !errors.Is(err, ErrToken) {
		t.Fatal("a token signed with another key passed")
	}
	// Claims changed after signing.
	p, s, _ := strings.Cut(tok, ".")
	b, _ := base64.RawURLEncoding.DecodeString(p)
	forged := base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(string(b), "5432", "22", 1))) + "." + s
	if _, err := Verify(testKey, forged, now); !errors.Is(err, ErrToken) {
		t.Fatal("changed claims passed")
	}
	for _, bad := range []string{"", "x", "a.b", "!!.??", Sign(testKey, Claims{Exp: now.Unix() + 60})} {
		if _, err := Verify(testKey, bad, now); !errors.Is(err, ErrToken) {
			t.Errorf("Verify(%q) passed", bad)
		}
	}
	if _, err := Verify(nil, tok, now); !errors.Is(err, ErrToken) {
		t.Fatal("an empty key verified")
	}
}

func TestInternal(t *testing.T) {
	for _, a := range []string{"127.0.0.1", "::1", "169.254.169.254", "fe80::1", "0.0.0.0", "::", "224.0.0.1", "::ffff:127.0.0.1"} {
		if !internal(netip.MustParseAddr(a)) {
			t.Errorf("%s not internal", a)
		}
	}
	for _, a := range []string{"10.0.0.5", "192.168.1.1", "8.8.8.8", "2001:db8::1"} {
		if internal(netip.MustParseAddr(a)) {
			t.Errorf("%s internal", a)
		}
	}
}

type fakeResolver map[string][]netip.Addr

func (f fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	if a, ok := f[host]; ok {
		return a, nil
	}
	return nil, errors.New("no such host")
}

// echo listens on loopback and echoes each connection, upper-cased.
func echo(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				b, _ := io.ReadAll(c)
				_, _ = c.Write([]byte(strings.ToUpper(string(b))))
			}()
		}
	}()
	return l.Addr().String()
}

func connect(t *testing.T, gw, to, auth string) (net.Conn, *bufio.Reader, int) {
	t.Helper()
	c, err := net.Dial("tcp", gw)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	req := "CONNECT " + to + " HTTP/1.1\r\nHost: " + to + "\r\n"
	if auth != "" {
		req += "Proxy-Authorization: " + auth + "\r\n"
	}
	if _, err := c.Write([]byte(req + "\r\n")); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect}) //nolint:bodyclose // a CONNECT's answer: what follows is the tunnel
	if err != nil {
		t.Fatal(err)
	}
	return c, br, resp.StatusCode
}

func TestTunnel(t *testing.T) {
	up := echo(t)
	_, port, _ := net.SplitHostPort(up)
	s := &Server{Key: testKey, AllowInternal: true, Resolver: fakeResolver{"db.internal": {netip.MustParseAddr("127.0.0.1")}}}
	gw := httptest.NewServer(s)
	defer gw.Close()
	gwAddr := strings.TrimPrefix(gw.URL, "http://")
	exp := time.Now().Add(time.Minute).Unix()
	to := "db.internal:" + port
	tok := Sign(testKey, Claims{Run: "r1", Allow: []string{to}, Exp: exp})

	c, br, code := connect(t, gwAddr, to, "Bearer "+tok)
	if code != http.StatusOK {
		t.Fatalf("CONNECT: %d", code)
	}
	_, _ = c.Write([]byte("select 1"))
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
	}
	got, _ := io.ReadAll(br)
	if string(got) != "SELECT 1" {
		t.Fatalf("through the tunnel: %q", got)
	}

	// Basic auth carries the token as the password.
	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte("run:"+tok))
	if _, _, code := connect(t, gwAddr, to, basic); code != http.StatusOK {
		t.Fatalf("basic: %d", code)
	}
	for name, tc := range map[string]struct {
		to, auth string
		code     int
	}{
		"no token":         {to, "", http.StatusProxyAuthRequired},
		"forged token":     {to, "Bearer " + Sign([]byte("not the key not the key not the k"), Claims{Run: "r1", Allow: []string{to}, Exp: exp}), http.StatusProxyAuthRequired},
		"other endpoint":   {"db.internal:1", "Bearer " + tok, http.StatusForbidden},
		"unresolvable":     {"nope.internal:" + port, "Bearer " + Sign(testKey, Claims{Run: "r1", Allow: []string{"nope.internal:" + port}, Exp: exp}), http.StatusBadGateway},
		"upstream refused": {"127.0.0.1:1", "Bearer " + Sign(testKey, Claims{Run: "r1", Allow: []string{"127.0.0.1:1"}, Exp: exp}), http.StatusBadGateway},
	} {
		if _, _, code := connect(t, gwAddr, tc.to, tc.auth); code != tc.code {
			t.Errorf("%s: %d, want %d", name, code, tc.code)
		}
	}
}

// A name that doesn't resolve: the user is told so, not the resolver's
// address.
func TestLookupReason(t *testing.T) {
	err := &net.DNSError{Err: "no such host", Name: "crm.internal", Server: "10.96.0.10:53", IsNotFound: true}
	if got := lookupReason("crm.internal:5432", fmt.Errorf("x: %w", err)); got != "crm.internal doesn't resolve (no such host)" {
		t.Fatalf("%q", got)
	}
	if got := lookupReason("crm.internal:5432", errors.New("lookup crm.internal on 10.96.0.10:53: i/o timeout")); strings.Contains(got, "10.96") {
		t.Fatalf("the resolver's address: %q", got)
	}
}

func TestRefusesInternal(t *testing.T) {
	up := echo(t)
	_, port, _ := net.SplitHostPort(up)
	s := &Server{Key: testKey, Resolver: fakeResolver{
		"metadata.evil": {netip.MustParseAddr("169.254.169.254")},
		"loop.evil":     {netip.MustParseAddr("127.0.0.1")},
	}}
	gw := httptest.NewServer(s)
	defer gw.Close()
	gwAddr := strings.TrimPrefix(gw.URL, "http://")
	exp := time.Now().Add(time.Minute).Unix()
	for _, to := range []string{"127.0.0.1:" + port, "loop.evil:" + port, "metadata.evil:80", "[::1]:" + port} {
		tok := Sign(testKey, Claims{Run: "r1", Allow: []string{to}, Exp: exp})
		if _, _, code := connect(t, gwAddr, to, "Bearer "+tok); code != http.StatusForbidden {
			t.Errorf("%s: %d, want 403", to, code)
		}
	}
}

func TestForward(t *testing.T) {
	var gotAuth, gotHost string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotHost = r.Header.Get("Proxy-Authorization"), r.Host
		_, _ = io.WriteString(w, "object "+r.URL.Path)
	}))
	defer up.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(up.URL, "http://"))
	s := &Server{Key: testKey, AllowInternal: true, Resolver: fakeResolver{"minio.internal": {netip.MustParseAddr("127.0.0.1")}}}
	gw := httptest.NewServer(s)
	defer gw.Close()
	to := "minio.internal:" + port
	tok := Sign(testKey, Claims{Run: "r1", Allow: []string{to}, Exp: time.Now().Add(time.Minute).Unix()})

	get := func(target, password string) (int, string) {
		pu, _ := url.Parse(gw.URL)
		if password != "" {
			pu.User = url.UserPassword("run", password)
		}
		cl := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu)}}
		resp, err := cl.Get(target)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	code, body := get("http://"+to+"/bucket/a.parquet", tok)
	if code != http.StatusOK || body != "object /bucket/a.parquet" {
		t.Fatalf("forward: %d %q", code, body)
	}
	if gotAuth != "" || gotHost != to {
		t.Fatalf("upstream saw auth %q, host %q", gotAuth, gotHost)
	}
	if code, _ := get("http://other.internal:"+port+"/x", tok); code != http.StatusForbidden {
		t.Fatalf("other host: %d", code)
	}
	if code, _ := get("http://"+to+"/x", ""); code != http.StatusProxyAuthRequired {
		t.Fatalf("no token: %d", code)
	}
}

func TestServe(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, "127.0.0.1:0", &Server{Key: testKey}) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve didn't stop")
	}
}
