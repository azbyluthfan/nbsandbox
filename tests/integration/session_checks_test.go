// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The isolation report's checks of interactive sessions: a session is a
// notebook's live kernel for as long as it lasts, so it must be held to a
// run's isolation the whole time.

// No network from a session: the outside, the metadata service, the API.
func TestSessionNetwork(t *testing.T) {
	sp := space(t, "alice")
	upload(t, "alice", sp, "", map[string]string{"nb.ipynb": codeNotebook("1")})
	s := openSession(t, "alice", sp, "nb.ipynb", "")
	k := connectSession(t, "alice", sp, s.ID)
	out, _ := k.run(`import socket
results = []
for host, port in [("1.1.1.1", 443), ("169.254.169.254", 80), ("nbsandboxd", 8080), ("host.docker.internal", 8080), ("kubernetes.default", 443)]:
    try:
        socket.create_connection((host, port), timeout=3).close()
        results.append(f"{host}:open")
    except OSError:
        results.append(f"{host}:blocked")
try:
    socket.getaddrinfo("example.com", 443)
    results.append("dns:resolves")
except OSError:
    results.append("dns:none")
print(" ".join(results))`)
	out = strings.TrimSpace(out)
	ok := out != "" && !strings.Contains(out, ":open") && !strings.Contains(out, "dns:resolves")
	finding(t, "session-network", "a session's kernel connects to the internet, the cloud metadata address, nbsandboxd and the cluster API, and resolves a name",
		"every connection and lookup fails", out, ok)
	if !ok {
		t.Errorf("network from a session: %s", out)
	}
}

// The same isolation as a run's container: another user, no privileges,
// a read-only system, nothing of nbsandboxd's.
func TestSessionIsolation(t *testing.T) {
	sp := space(t, "alice")
	upload(t, "alice", sp, "", map[string]string{"nb.ipynb": codeNotebook("1"), "mine.txt": "alice's"})
	other := space(t, "bob")
	upload(t, "bob", other, "", map[string]string{"secret.txt": "SECRET-of-bob"})
	s := openSession(t, "alice", sp, "nb.ipynb", "")
	k := connectSession(t, "alice", sp, s.ID)
	out, _ := k.run(`import os
cap = [l.split()[1] for l in open('/proc/self/status') if l.startswith('CapEff')][0]
try:
    open('/usr/x', 'w'); rofs = 'writable'
except OSError:
    rofs = 'read-only'
found = []
for root, dirs, files in os.walk('/'):
    if root == '/':
        dirs[:] = [d for d in dirs if d not in ('proc', 'sys', 'dev')]
    found += [os.path.join(root, f) for f in files if f == 'secret.txt']
env = sorted(k for k in os.environ if k.startswith('NBSANDBOX_') and k not in ('NBSANDBOX_SESSION', 'NBSANDBOX_SESSION_ID', 'NBSANDBOX_SESSION_MAX_MESSAGE', 'NBSANDBOX_RUN_DIR', 'NBSANDBOX_OUTPUT_DIR', 'NBSANDBOX_NPROC'))
print(os.getuid(), cap, rofs, sorted(os.listdir('/space')), found, env)`)
	out = strings.TrimSpace(out)
	ok := strings.HasPrefix(out, "1000 0000000000000000 read-only") && strings.Contains(out, "'mine.txt'") &&
		strings.Contains(out, "] [] []") && !strings.Contains(out, "SECRET")
	finding(t, "session-isolation", "a session looks at its user, capabilities, root filesystem, /space, other spaces' files and nbsandboxd's settings",
		"uid 1000, no capabilities, a read-only system, only its own space's files, nothing of nbsandboxd's", out, ok)
	if !ok {
		t.Errorf("isolation: %s", out)
	}
	// And another user can't open it or reach it.
	r := call(t, "GET", "/v1/spaces/"+sp+"/sessions/"+s.ID, "bob", nil, nil)
	if r.Code != http.StatusNotFound {
		t.Errorf("bob sees alice's session: %d", r.Code)
	}
}

// A kernel past the memory limit dies, and nothing else is affected. On
// Docker the bridge starts a new kernel and the session goes on; on
// Kubernetes the kubelet kills the whole container (memory.oom.group), so
// the session ends, saying why.
func TestSessionMemoryLimit(t *testing.T) {
	sp := space(t, "alice")
	upload(t, "alice", sp, "", map[string]string{"nb.ipynb": codeNotebook("1")})
	s := openSession(t, "alice", sp, "nb.ipynb", "")
	k := connectSession(t, "alice", sp, s.ID)
	k.send("shell", "execute_request", map[string]any{"code": "x = []\nwhile True: x.append(bytearray(64 << 20))"})
	start := time.Now()
	m := k.next(func(m kmsg) bool {
		st := m.Content["status"]
		return m.Channel == "bridge" && (st == "dead" || st == "restarting" || st == "ended")
	})
	var got string
	var ok bool
	if m.Content["status"] == "ended" {
		ready := call(t, "GET", "/readyz", "", nil, nil)
		got = fmt.Sprintf("the session ended after %.0fs (%v); nbsandboxd ready %d", time.Since(start).Seconds(), m.Content["reason"], ready.Code)
		ok = onKubernetes && ready.Code == http.StatusOK
	} else {
		k.next(func(m kmsg) bool { return m.Channel == "bridge" && m.Content["status"] == "ready" })
		back := time.Since(start)
		out, _ := k.run("print('alive')")
		ready := call(t, "GET", "/readyz", "", nil, nil)
		got = fmt.Sprintf("kernel %s, ready again after %.0fs; next cell printed %q; nbsandboxd ready %d", m.Content["status"], back.Seconds(),
			strings.TrimSpace(out), ready.Code)
		ok = strings.TrimSpace(out) == "alive" && ready.Code == http.StatusOK
	}
	finding(t, "session-memory-limit", "a session's cell allocates memory without bound",
		"the kernel is killed at the limit (Docker: started again, the session goes on; Kubernetes: the session ends); nbsandboxd goes on", got, ok)
	if !ok {
		t.Error(got)
	}
}

// A read-only session can't change the space, and holds no lock: a
// writing run can start meanwhile.
func TestSessionReadOnlyCantWrite(t *testing.T) {
	sp := space(t, "alice")
	upload(t, "alice", sp, "", map[string]string{"nb.ipynb": codeNotebook("1"), "keep.txt": "original"})
	s := openSession(t, "alice", sp, "nb.ipynb", "read-only")
	k := connectSession(t, "alice", sp, s.ID)
	out, _ := k.run("import os\nfor op in (lambda: open('keep.txt','w').write('changed'), lambda: os.remove('keep.txt'), lambda: open('new.txt','w')):\n    try:\n        op(); print('wrote')\n    except OSError as e:\n        print(type(e).__name__)")
	run := jsonCall(t, "POST", "/v1/spaces/"+sp+"/runs", "alice", map[string]string{"notebook": "nb.ipynb"}, nil)
	file := call(t, "GET", "/v1/spaces/"+sp+"/files/keep.txt", "alice", nil, nil)
	got := fmt.Sprintf("%s; a writing run meanwhile: %d; keep.txt: %q", strings.Join(strings.Fields(out), " "), run.Code, file.Body)
	ok := !strings.Contains(out, "wrote") && (run.Code == http.StatusCreated || run.Code == http.StatusAccepted) && string(file.Body) == "original"
	finding(t, "session-read-only", "a read-only session changes, deletes and creates files in /space",
		"every write fails; the space is unchanged; a writing run may start meanwhile", got, ok)
	if !ok {
		t.Error(got)
	}
}

// nbsandboxd restarts: the session's kernel keeps running, and its
// variables are there when the client reconnects.
func TestSessionReattach(t *testing.T) {
	needCompose(t)
	sp := space(t, "alice")
	upload(t, "alice", sp, "", map[string]string{"nb.ipynb": codeNotebook("1")})
	s := openSession(t, "alice", sp, "nb.ipynb", "")
	k := connectSession(t, "alice", sp, s.ID)
	k.run("kept = 41 + 1")
	compose(t, "restart", "nbsandboxd")
	if err := waitReady(time.Minute); err != nil {
		t.Fatal(err)
	}
	k2 := connectSession(t, "alice", sp, s.ID)
	out, _ := k2.run("print(kept)")
	got := fmt.Sprintf("after the restart, the variable is %q", strings.TrimSpace(out))
	ok := strings.TrimSpace(out) == "42"
	finding(t, "session-reattach", "restart nbsandboxd while a session holds a variable; reconnect",
		"the session's kernel kept running and is reached again, with its variables", got, ok)
	if !ok {
		t.Error(got)
	}
}
