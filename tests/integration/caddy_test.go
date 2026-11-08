// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// Through Caddy, log events arrive one by one, not
// buffered; and only /v1 is exposed. Runs against the production stack
// when NBSANDBOX_CADDY_URL is set (e.g. https://localhost:8443).
func TestStreamThroughCaddy(t *testing.T) {
	caddy := os.Getenv("NBSANDBOX_CADDY_URL")
	if caddy == "" {
		t.Skip("set NBSANDBOX_CADDY_URL to test through Caddy")
	}
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // Caddy's internal CA in tests
	}}
	do := func(method, path, body string) (int, string) {
		req, _ := http.NewRequest(method, caddy+path, strings.NewReader(body))
		req.SetBasicAuth("alice", passwords["alice"])
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	// Internal endpoints stay internal.
	for _, p := range []string{"/metrics", "/readyz", "/healthz", "/ui/api/users", "/"} {
		if code, _ := do("GET", p, ""); code != http.StatusNotFound {
			t.Errorf("%s through Caddy: %d, want 404", p, code)
		}
	}

	sp := space(t, "alice")
	upload(t, "alice", sp, "", map[string]string{"stream.ipynb": ""})
	run := startRun(t, "alice", sp, "stream.ipynb", nil)

	req, _ := http.NewRequest("GET", caddy+"/v1/spaces/"+sp+"/runs/"+run.ID+"/log/stream", nil)
	req.SetBasicAuth("alice", passwords["alice"])
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	s := &stream{resp: resp, r: bufio.NewReaderSize(resp.Body, 64<<10)}
	defer s.close()
	lines, end, err := s.readToEnd()
	if err != nil {
		t.Fatal(err)
	}
	var times []time.Time
	for _, ev := range lines {
		if strings.HasPrefix(ev.Data, "line ") {
			times = append(times, ev.Received)
		}
	}
	spread := time.Duration(0)
	if len(times) == 10 {
		spread = times[9].Sub(times[0])
	}
	ok := end.Name == "done" && len(times) == 10 && spread > 8*time.Second && sequential(lines, 1) == nil
	finding(t, "through-proxy", "follow the live log through Caddy (TLS)", "events arrive individually, not buffered",
		fmt.Sprintf("10 lines spread over %.1fs; %s", spread.Seconds(), end.Data), ok)
	if !ok {
		t.Fatalf("lines %d spread %v end %+v", len(times), spread, end)
	}
}
