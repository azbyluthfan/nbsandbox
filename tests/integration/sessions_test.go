// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Interactive sessions, end to end: a real kernel in a container, through
// the API and its WebSocket.

type sessionJSON struct {
	ID, Mode, Status, EndReason string
	Notebook                    string `json:"notebook"`
}

func openSession(t *testing.T, user, sp, notebook, mode string) sessionJSON {
	t.Helper()
	r := jsonCall(t, "POST", "/v1/spaces/"+sp+"/sessions", user, map[string]string{"notebook": notebook, "mode": mode}, nil)
	if r.Code != http.StatusCreated && r.Code != http.StatusOK {
		t.Fatalf("open session: %d %s", r.Code, r.Body)
	}
	var s sessionJSON
	r.json(t, &s)
	t.Cleanup(func() {
		call(t, "DELETE", "/v1/spaces/"+sp+"/sessions/"+s.ID, user, nil, nil)
		assertNoSessionContainer(t, s.ID)
	})
	return s
}

// kernelConn is a session's WebSocket.
type kernelConn struct {
	t *testing.T
	c *websocket.Conn
	n int
}

func connectSession(t *testing.T, user, sp, id string) *kernelConn {
	t.Helper()
	k := dialSession(t, user, sp, id)
	t.Cleanup(func() { _ = k.c.Close() })
	return k
}

// dialSession connects to a session's channel; the caller closes it.
func dialSession(t *testing.T, user, sp, id string) *kernelConn {
	t.Helper()
	h := http.Header{}
	req, _ := http.NewRequest("GET", baseURL, nil)
	req.SetBasicAuth(user, passwords[user])
	h.Set("Authorization", req.Header.Get("Authorization"))
	d := websocket.Dialer{HandshakeTimeout: 3 * time.Minute} // the first attach waits for the kernel
	c, resp, err := d.Dial("ws"+strings.TrimPrefix(baseURL, "http")+"/v1/spaces/"+sp+"/sessions/"+id+"/channel", h)
	if err != nil {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		t.Fatalf("connect to the session: %v (%d)", err, code)
	}
	if resp != nil {
		_ = resp.Body.Close()
	}
	k := &kernelConn{t: t, c: c}
	hello := k.next(func(m kmsg) bool { return m.Channel == "bridge" })
	if hello.Content["status"] != "attached" {
		t.Fatalf("first message %+v", hello)
	}
	return k
}

type kmsg struct {
	Channel string         `json:"channel"`
	Header  map[string]any `json:"header"`
	Parent  map[string]any `json:"parent_header"`
	Content map[string]any `json:"content"`
}

func (m kmsg) typ() string { s, _ := m.Header["msg_type"].(string); return s }

func (k *kernelConn) send(channel, msgType string, content map[string]any) string {
	k.t.Helper()
	k.n++
	id := "test-" + time.Now().Format("150405.000") + "-" + string(rune('a'+k.n%26))
	b, _ := json.Marshal(map[string]any{"channel": channel, "header": map[string]any{"msg_id": id, "msg_type": msgType, "session": "test"},
		"parent_header": map[string]any{}, "metadata": map[string]any{}, "content": content})
	if err := k.c.WriteMessage(websocket.TextMessage, b); err != nil {
		k.t.Fatal(err)
	}
	return id
}

func (k *kernelConn) next(match func(kmsg) bool) kmsg {
	k.t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		_ = k.c.SetReadDeadline(deadline)
		_, b, err := k.c.ReadMessage()
		if err != nil {
			k.t.Fatalf("session channel: %v", err)
		}
		var m kmsg
		if json.Unmarshal(b, &m) == nil && match(m) {
			return m
		}
	}
}

// run executes code and returns what it printed and its reply. Both must
// come: the output and idle status (iopub) and the reply (shell), which can
// arrive in either order. A cell that finishes without a reply fails at once
// (a silent shell channel would otherwise only show at the next completion).
func (k *kernelConn) run(code string) (stdout string, reply kmsg) {
	k.t.Helper()
	id := k.send("shell", "execute_request", map[string]any{"code": code, "silent": false})
	var out strings.Builder
	idle := false
	for !idle || reply.Channel == "" {
		if idle {
			_ = k.c.SetReadDeadline(time.Now().Add(30 * time.Second))
			_, b, err := k.c.ReadMessage()
			if err != nil {
				k.t.Fatalf("a cell finished (iopub idle) but no execute_reply came on the shell channel: %v", err)
			}
			var m kmsg
			if json.Unmarshal(b, &m) == nil && m.Parent["msg_id"] == id && m.Channel == "shell" && m.typ() == "execute_reply" {
				reply = m
			}
			continue
		}
		m := k.next(func(m kmsg) bool { return m.Parent["msg_id"] == id })
		switch {
		case m.Channel == "iopub" && m.typ() == "stream":
			out.WriteString(m.Content["text"].(string)) //nolint:forcetypeassert // a test: a panic is a failure
		case m.Channel == "iopub" && m.typ() == "error":
			out.WriteString("ERROR " + m.Content["ename"].(string)) //nolint:forcetypeassert // as above
		case m.Channel == "shell" && m.typ() == "execute_reply":
			reply = m
		case m.Channel == "iopub" && m.typ() == "status" && m.Content["execution_state"] == "idle":
			idle = true
		}
	}
	return out.String(), reply
}

func (k *kernelConn) complete(code string) []string {
	k.t.Helper()
	id := k.send("shell", "complete_request", map[string]any{"code": code, "cursor_pos": len(code)})
	m := k.next(func(m kmsg) bool { return m.Parent["msg_id"] == id && m.typ() == "complete_reply" })
	var out []string
	for _, x := range m.Content["matches"].([]any) { //nolint:forcetypeassert // a test: a panic is a failure
		out = append(out, strings.TrimPrefix(x.(string), ".")) //nolint:forcetypeassert // as above
	}
	return out
}

// assertNoSessionContainer checks a session's container (or Pod) is gone.
func assertNoSessionContainer(t *testing.T, id string) {
	t.Helper()
	list := func() string {
		if onKubernetes {
			return kubectl(t, "-n", "nbsandbox-runs", "get", "pods", "-l", "nbsandbox.session="+id, "-o", "name")
		}
		return docker(t, "ps", "-a", "--filter", "label=nbsandbox.session="+id, "--format", "{{.Names}}")
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		out := list()
		if out == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("session container left behind: %s", out)
			if onKubernetes {
				kubectl(t, "-n", "nbsandbox-runs", "delete", out, "--grace-period=0", "--force", "--ignore-not-found")
			} else {
				docker(t, "rm", "-f", out)
			}
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func TestSessionEndToEnd(t *testing.T) {
	sp := space(t, "alice")
	upload(t, "alice", sp, "", map[string]string{"nb.ipynb": codeNotebook("1")})
	upload(t, "alice", sp, "data", map[string]string{"sales.csv": "region,revenue\nnorth,10\nsouth,20\n"})
	s := openSession(t, "alice", sp, "nb.ipynb", "")
	if s.Mode != "read-write" {
		t.Fatalf("mode %q", s.Mode)
	}
	k := connectSession(t, "alice", sp, s.ID)

	// State carries over between cells; the space's files are there.
	k.run("import pandas as pd\ndf = pd.read_csv('data/sales.csv')")
	if out, _ := k.run("print(int(df.revenue.sum()))"); out != "30\n" {
		t.Fatalf("sum: %q", out)
	}
	// Completion from the live objects: the DataFrame's columns.
	if m := k.complete(`df["`); !contains(m, "region") || !contains(m, "revenue") {
		t.Fatalf(`df[" completes to %v`, m)
	}
	if m := k.complete("df.rev"); !contains(m, "revenue") {
		t.Fatalf("df.rev completes to %v", m)
	}
	// Docs.
	id := k.send("shell", "inspect_request", map[string]any{"code": "pd.read_csv(", "cursor_pos": 12, "detail_level": 0})
	if m := k.next(func(m kmsg) bool { return m.Parent["msg_id"] == id && m.typ() == "inspect_reply" }); m.Content["found"] != true {
		t.Fatalf("inspect %+v", m.Content)
	}
	// No network, as a run.
	out, _ := k.run("import socket\ntry:\n    socket.create_connection(('1.1.1.1', 443), timeout=3); print('CONNECTED')\nexcept OSError as e:\n    print('blocked')")
	if out != "blocked\n" {
		t.Fatalf("network: %q", out)
	}
	// input() can't wait for an answer that never comes.
	if out, _ := k.run("input('name? ')"); !strings.Contains(out, "StdinNotImplementedError") {
		t.Fatalf("input(): %q", out)
	}
	// Files it writes reach the space on sync.
	k.run("open('made-in-session.txt', 'w').write('hello')")
	r := call(t, "POST", "/v1/spaces/"+sp+"/sessions/"+s.ID+"/sync", "alice", nil, nil)
	if r.Code != http.StatusOK {
		t.Fatalf("sync: %d %s", r.Code, r.Body)
	}
	if r := call(t, "GET", "/v1/spaces/"+sp+"/files/made-in-session.txt", "alice", nil, nil); r.Code != http.StatusOK || string(r.Body) != "hello" {
		t.Fatalf("file after sync: %d %q", r.Code, r.Body)
	}
	// While it is open, no writing run.
	if r := jsonCall(t, "POST", "/v1/spaces/"+sp+"/runs", "alice", map[string]string{"notebook": "nb.ipynb"}, nil); r.Code != http.StatusConflict {
		t.Fatalf("run during the session: %d %s", r.Code, r.Body)
	}
	// Restart: the variables are gone, the files stay.
	if r := call(t, "POST", "/v1/spaces/"+sp+"/sessions/"+s.ID+"/restart", "alice", nil, nil); r.Code != http.StatusAccepted {
		t.Fatalf("restart: %d", r.Code)
	}
	k.next(func(m kmsg) bool { return m.Channel == "bridge" && m.Content["status"] == "ready" })
	if out, _ := k.run("print('df' in dir(), open('made-in-session.txt').read())"); out != "False hello\n" {
		t.Fatalf("after restart: %q", out)
	}
	// End: a last message, the container gone, changes written back.
	k.run("open('at-the-end.txt', 'w').write('bye')")
	if r := call(t, "DELETE", "/v1/spaces/"+sp+"/sessions/"+s.ID, "alice", nil, nil); r.Code != http.StatusOK {
		t.Fatalf("end: %d", r.Code)
	}
	k.next(func(m kmsg) bool { return m.Channel == "bridge" && m.Content["status"] == "ended" })
	assertNoSessionContainer(t, s.ID)
	if r := call(t, "GET", "/v1/spaces/"+sp+"/files/at-the-end.txt", "alice", nil, nil); r.Code != http.StatusOK || string(r.Body) != "bye" {
		t.Fatalf("file written before the end: %d %q", r.Code, r.Body)
	}
	// The space can run again.
	if r := jsonCall(t, "POST", "/v1/spaces/"+sp+"/runs", "alice", map[string]string{"notebook": "nb.ipynb"}, nil); r.Code != http.StatusAccepted &&
		r.Code != http.StatusCreated {
		t.Fatalf("run after the session: %d %s", r.Code, r.Body)
	}
}

func TestSessionInterruptAndReadOnly(t *testing.T) {
	sp := space(t, "alice")
	upload(t, "alice", sp, "", map[string]string{"a.ipynb": codeNotebook("1"), "b.ipynb": codeNotebook("1")})
	a := openSession(t, "alice", sp, "a.ipynb", "")
	b := openSession(t, "alice", sp, "b.ipynb", "")
	if a.Mode != "read-write" || b.Mode != "read-only" {
		t.Fatalf("modes %q %q: the second notebook's session is read-only", a.Mode, b.Mode)
	}
	k := connectSession(t, "alice", sp, b.ID)
	if out, _ := k.run("try:\n    open('x.txt', 'w')\nexcept OSError as e:\n    print(type(e).__name__)"); !strings.Contains(out, "Error") {
		t.Fatalf("a read-only session wrote the space: %q", out)
	}
	id := k.send("shell", "execute_request", map[string]any{"code": "import time\nwhile True: time.sleep(0.05)"})
	k.next(func(m kmsg) bool {
		return m.Parent["msg_id"] == id && m.typ() == "status" && m.Content["execution_state"] == "busy"
	})
	if r := call(t, "POST", "/v1/spaces/"+sp+"/sessions/"+b.ID+"/interrupt", "alice", nil, nil); r.Code != http.StatusAccepted {
		t.Fatalf("interrupt: %d", r.Code)
	}
	m := k.next(func(m kmsg) bool { return m.Parent["msg_id"] == id && m.typ() == "execute_reply" })
	if m.Content["ename"] != "KeyboardInterrupt" {
		t.Fatalf("after interrupt: %+v", m.Content)
	}
	// Bob can't reach alice's session.
	if r := call(t, "GET", "/v1/spaces/"+sp+"/sessions/"+b.ID, "bob", nil, nil); r.Code != http.StatusNotFound {
		t.Fatalf("bob: %d", r.Code)
	}
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
