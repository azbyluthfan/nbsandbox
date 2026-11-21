// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package sessions

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/isolation"
	"github.com/azbyluthfan/nbsandbox/internal/spaces"
	"github.com/azbyluthfan/nbsandbox/internal/store"
	"github.com/azbyluthfan/nbsandbox/internal/store/storetest"
)

// fakeBridge plays a session's bridge: ready at start, and a cell's usual
// messages for each execute_request ("hang" never finishes, until
// interrupted).
type fakeBridge struct {
	in      *io.PipeReader // what nbsandboxd writes
	inW     *io.PipeWriter
	out     *io.PipeReader // what nbsandboxd reads
	outW    *io.PipeWriter
	mu      sync.Mutex
	got     []string // msg_types received
	running string   // a hanging cell's msg_id
}

func newFakeBridge() *fakeBridge {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	b := &fakeBridge{in: inR, inW: inW, out: outR, outW: outW}
	go b.serve()
	return b
}

func (b *fakeBridge) emit(m map[string]any) {
	line, _ := json.Marshal(m)
	b.mu.Lock()
	w := b.outW
	b.mu.Unlock()
	_, _ = w.Write(append(line, '\n'))
}

func status(parent map[string]any, state string) map[string]any {
	return map[string]any{"channel": "iopub", "header": map[string]any{"msg_id": spaces.NewID(), "msg_type": "status"},
		"parent_header": parent, "content": map[string]any{"execution_state": state}}
}

func (b *fakeBridge) serve() {
	b.emit(map[string]any{"channel": "bridge", "content": map[string]any{"status": "ready"}})
	sc := bufio.NewScanner(b.in)
	for sc.Scan() {
		var m struct {
			Channel string         `json:"channel"`
			Header  map[string]any `json:"header"`
			Content map[string]any `json:"content"`
		}
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		mt, _ := m.Header["msg_type"].(string)
		b.mu.Lock()
		b.got = append(b.got, mt)
		b.mu.Unlock()
		switch mt {
		case "execute_request":
			b.emit(status(m.Header, "busy"))
			code, _ := m.Content["code"].(string)
			if code == "hang" {
				b.mu.Lock()
				b.running, _ = m.Header["msg_id"].(string)
				b.mu.Unlock()
				continue
			}
			b.emit(map[string]any{"channel": "iopub", "header": map[string]any{"msg_id": spaces.NewID(), "msg_type": "stream"},
				"parent_header": m.Header, "content": map[string]any{"name": "stdout", "text": "out:" + code + "\n"}})
			b.emit(map[string]any{"channel": "shell", "header": map[string]any{"msg_id": spaces.NewID(), "msg_type": "execute_reply"},
				"parent_header": m.Header, "content": map[string]any{"status": "ok"}})
			b.emit(status(m.Header, "idle"))
		case "interrupt_request":
			b.mu.Lock()
			id := b.running
			b.running = ""
			b.mu.Unlock()
			if id != "" {
				b.emit(status(map[string]any{"msg_id": id, "msg_type": "execute_request"}, "idle"))
			}
		}
	}
}

// field is m[k] as an object (nil if it isn't one).
func field(m map[string]any, k string) map[string]any {
	v, _ := m[k].(map[string]any)
	return v
}

type bridgeConn struct {
	b   *fakeBridge
	out *io.PipeReader // this attach's output
}

func (c bridgeConn) Read(p []byte) (int, error)  { return c.out.Read(p) }
func (c bridgeConn) Write(p []byte) (int, error) { return c.b.inW.Write(p) }
func (c bridgeConn) Close() error                { return c.out.CloseWithError(errors.New("detached")) }

// fakeBackend holds fake session containers.
type fakeBackend struct {
	mu       sync.Mutex
	bridges  map[string]*fakeBridge
	specs    []isolation.SessionSpec
	stopped  []string
	startErr error
}

func newFakeBackend() *fakeBackend { return &fakeBackend{bridges: map[string]*fakeBridge{}} }

func (f *fakeBackend) StartSession(_ context.Context, spec isolation.SessionSpec) (isolation.SessionConn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return nil, f.startErr
	}
	b := newFakeBridge()
	f.bridges[spec.SessionID] = b
	f.specs = append(f.specs, spec)
	return bridgeConn{b, b.out}, nil
}

func (f *fakeBackend) AttachSession(_ context.Context, id string) (isolation.SessionConn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b := f.bridges[id]
	if b == nil {
		return nil, errors.New("no such container")
	}
	// A fresh pipe for the new attach, served by the same bridge.
	outR, outW := io.Pipe()
	b.mu.Lock()
	b.out, b.outW = outR, outW
	b.mu.Unlock()
	return bridgeConn{b, outR}, nil
}

func (f *fakeBackend) StopSession(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if b := f.bridges[id]; b != nil {
		b.mu.Lock()
		_ = b.outW.CloseWithError(io.EOF)
		b.mu.Unlock()
		_ = b.inW.Close()
		delete(f.bridges, id)
	}
	f.stopped = append(f.stopped, id)
	return nil
}

func (f *fakeBackend) Sessions(context.Context) ([]isolation.SessionInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []isolation.SessionInfo
	for id := range f.bridges {
		out = append(out, isolation.SessionInfo{SessionID: id, Running: true})
	}
	return out, nil
}

// crash ends a session's container as if it died.
func (f *fakeBackend) crash(id string) {
	f.mu.Lock()
	b := f.bridges[id]
	delete(f.bridges, id)
	f.mu.Unlock()
	b.mu.Lock()
	_ = b.outW.CloseWithError(io.EOF)
	b.mu.Unlock()
}

func (f *fakeBackend) spec(id string) isolation.SessionSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.specs {
		if s.SessionID == id {
			return s
		}
	}
	return isolation.SessionSpec{}
}

func (f *fakeBackend) wasStopped(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Contains(f.stopped, id)
}

// fakeSpaces: one owner, files by space, working copies in a temp folder.
type fakeSpaces struct {
	mu        sync.Mutex
	root      string
	files     map[string]bool // spaceID/path
	synced    []string
	discarded []string
}

func (f *fakeSpaces) Get(_ context.Context, owner, id string) (spaces.Space, error) {
	if owner != "alice" {
		return spaces.Space{}, store.ErrNotFound
	}
	return spaces.Space{ID: id}, nil
}

func (f *fakeSpaces) Stat(_ context.Context, owner, id, p string) (spaces.File, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if owner != "alice" || !f.files[id+"/"+p] {
		return spaces.File{}, store.ErrNotFound
	}
	return spaces.File{Path: p, Type: spaces.TypeFile}, nil
}

func (f *fakeSpaces) Checkout(_ context.Context, spaceID, id string) (string, error) {
	dir := f.WorkPath(spaceID, id)
	return dir, os.MkdirAll(dir, 0o700)
}

func (f *fakeSpaces) Sync(_ context.Context, _, id string) ([]store.Conflict, []string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.synced = append(f.synced, id)
	return nil, nil, nil
}

func (f *fakeSpaces) Discard(_, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.discarded = append(f.discarded, id)
}

func (f *fakeSpaces) WorkPath(spaceID, id string) string { return filepath.Join(f.root, spaceID, id) }

func (f *fakeSpaces) count(list *[]string, id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, x := range *list {
		if x == id {
			n++
		}
	}
	return n
}

type env struct {
	db      *store.DB
	backend *fakeBackend
	spaces  *fakeSpaces
	m       *Manager
	space   string
	clock   *clock
	drain   atomic.Bool
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

func newEnv(t *testing.T, mod func(*Config)) *env {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, storetest.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	e := &env{db: db, backend: newFakeBackend(), clock: &clock{t: time.Now()},
		spaces: &fakeSpaces{root: t.TempDir(), files: map[string]bool{}}}
	e.space = spaces.NewID()
	if err := db.CreateSpaceCapped(ctx, store.Space{ID: e.space, Owner: "alice", CreatedAt: time.Now(), UpdatedAt: time.Now()}, 0); err != nil {
		t.Fatal(err)
	}
	for _, nb := range []string{"a.ipynb", "b.ipynb", "c.ipynb"} {
		e.spaces.files[e.space+"/"+nb] = true
	}
	cfg := Config{Enabled: true, Image: "runner:test", Idle: 30 * time.Minute, Max: 8 * time.Hour, WriteBack: 5 * time.Minute,
		Retention: time.Hour, PerUser: 2, PerWorker: 4, MaxMessage: 1 << 20, WorkerID: "w1", Tick: 20 * time.Millisecond,
		StartTimeout: 5 * time.Second}
	if mod != nil {
		mod(&cfg)
	}
	e.m = New(cfg, Deps{DB: db, Spaces: e.spaces, Backend: e.backend, Now: e.clock.Now,
		Draining: e.drain.Load, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err := e.m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.m.Shutdown(context.Background()) })
	return e
}

func (e *env) open(t *testing.T, nb, mode string) store.Session {
	t.Helper()
	s, _, err := e.m.Open(context.Background(), "alice", e.space, nb, mode)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (e *env) attach(t *testing.T, id string) *Client {
	t.Helper()
	c, _, err := e.m.Attach(context.Background(), "alice", e.space, id)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (e *env) session(t *testing.T, id string) store.Session {
	t.Helper()
	s, err := e.db.GetSession(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// next reads the client's next message whose channel and type match.
func next(t *testing.T, c *Client, channel, msgType string) map[string]any {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case line, ok := <-c.Messages():
			if !ok {
				t.Fatalf("client closed (%s) waiting for %s/%s", c.Reason(), channel, msgType)
			}
			var m map[string]any
			_ = json.Unmarshal(line, &m)
			h, _ := m["header"].(map[string]any)
			mt, _ := h["msg_type"].(string)
			if m["channel"] == channel && (msgType == "" || mt == msgType) {
				return m
			}
		case <-timeout:
			t.Fatalf("no %s/%s", channel, msgType)
		}
	}
}

func execute(code string) []byte {
	b, _ := json.Marshal(map[string]any{"channel": "shell", "header": map[string]any{"msg_id": spaces.NewID(),
		"msg_type": "execute_request"}, "content": map[string]any{"code": code}})
	return b
}

func TestOpenChecks(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	for _, tc := range []struct{ owner, nb, mode string }{
		{"alice", "a.ipynb", "sideways"}, {"alice", "notes.txt", ""}, {"alice", "missing.ipynb", ""}, {"bob", "a.ipynb", ""},
	} {
		if _, _, err := e.m.Open(ctx, tc.owner, e.space, tc.nb, tc.mode); err == nil {
			t.Errorf("%+v: opened", tc)
		}
	}
	e.drain.Store(true)
	if _, _, err := e.m.Open(ctx, "alice", e.space, "a.ipynb", ""); !errors.Is(err, ErrDraining) {
		t.Fatalf("draining: %v", err)
	}
	off := New(Config{}, Deps{})
	if _, _, err := off.Open(ctx, "alice", e.space, "a.ipynb", ""); !errors.Is(err, ErrDisabled) {
		t.Fatalf("disabled: %v", err)
	}
}

func TestSessionRunsCellsForItsClients(t *testing.T) {
	e := newEnv(t, nil)
	s := e.open(t, "a.ipynb", "")
	if s.Mode != store.SessionReadWrite || s.Status != store.SessionStarting {
		t.Fatalf("%+v", s)
	}
	c := e.attach(t, s.ID) // waits for it to start
	hello := next(t, c, "bridge", "")
	if content := field(hello, "content"); content["status"] != "attached" || content["mode"] != "read-write" {
		t.Fatalf("hello %v", hello)
	}
	if got := e.session(t, s.ID); got.Status != store.SessionReady || got.Worker != "w1" {
		t.Fatalf("%+v", got)
	}
	if err := c.Send(execute("1+1")); err != nil {
		t.Fatal(err)
	}
	out := next(t, c, "iopub", "stream")
	if field(out, "content")["text"] != "out:1+1\n" {
		t.Fatalf("%v", out)
	}
	next(t, c, "shell", "execute_reply")
	// A second client sees the same session's messages.
	c2 := e.attach(t, s.ID)
	_ = c.Send(execute("2"))
	next(t, c2, "iopub", "stream")
	// The container: the space's working copy, read-write.
	spec := e.backend.spec(s.ID)
	if spec.SpaceReadOnly || spec.SpaceDir != e.spaces.WorkPath(e.space, s.ID) || spec.Image != "runner:test" {
		t.Fatalf("%+v", spec)
	}
}

func TestClientsCantSendEverything(t *testing.T) {
	e := newEnv(t, nil)
	c := e.attach(t, e.open(t, "a.ipynb", "").ID)
	for _, msg := range []string{
		`not json`,
		`{"channel":"shell","header":{"msg_id":"1","msg_type":"history_request"},"content":{}}`,
		`{"channel":"control","header":{"msg_id":"1","msg_type":"shutdown_request"},"content":{"restart":true}}`,
		`{"channel":"stdin","header":{"msg_id":"1","msg_type":"input_reply"},"content":{}}`,
		`{"channel":"shell","header":{"msg_type":"execute_request"},"content":{}}`,
		`{"channel":"shell","header":{"msg_id":"1","msg_type":"comm_open"},"content":{}}`,
		`{"channel":"shell","header":{"msg_id":"1","msg_type":"execute_request"},"content":{},"buffers":["AAAA"]}`,
	} {
		var refused *RefusedError
		if err := c.Send([]byte(msg)); !errors.As(err, &refused) {
			t.Errorf("%s: %v", msg, err)
		}
	}
	// Pretty-printed JSON goes to the bridge as one line.
	if err := c.Send([]byte("{\n \"channel\": \"shell\",\n \"header\": {\"msg_id\": \"x\", \"msg_type\": \"kernel_info_request\"},\n \"content\": {}\n}")); err != nil {
		t.Fatal(err)
	}
}

func TestReadOnlyAndTheWriteLock(t *testing.T) {
	e := newEnv(t, nil)
	rw := e.open(t, "a.ipynb", "")
	ro := e.open(t, "b.ipynb", store.SessionReadWrite)
	if rw.Mode != store.SessionReadWrite || ro.Mode != store.SessionReadOnly {
		t.Fatalf("%s %s", rw.Mode, ro.Mode)
	}
	e.attach(t, ro.ID)
	if spec := e.backend.spec(ro.ID); !spec.SpaceReadOnly {
		t.Fatalf("read-only session mounted read-write: %+v", spec)
	}
	if _, err := e.m.SyncFiles(context.Background(), "alice", e.space, ro.ID); err == nil {
		t.Fatal("synced a read-only session")
	}
}

func TestPerUserLimit(t *testing.T) {
	e := newEnv(t, nil)
	e.open(t, "a.ipynb", "")
	e.open(t, "b.ipynb", "")
	if _, _, err := e.m.Open(context.Background(), "alice", e.space, "c.ipynb", ""); !errors.Is(err, ErrLimit) {
		t.Fatalf("third session: %v", err)
	}
}

func TestEndStopsWritesBackAndTellsClients(t *testing.T) {
	e := newEnv(t, nil)
	s := e.open(t, "a.ipynb", "")
	c := e.attach(t, s.ID)
	got, err := e.m.End(context.Background(), "alice", e.space, s.ID, "alice")
	if err != nil || got.Open() || got.EndedBy != "alice" {
		t.Fatalf("%+v %v", got, err)
	}
	bye := next(t, c, "bridge", "")
	for field(bye, "content")["status"] != "ended" {
		bye = next(t, c, "bridge", "")
	}
	<-c.Done()
	if !e.backend.wasStopped(s.ID) || e.spaces.count(&e.spaces.synced, s.ID) != 1 || e.spaces.count(&e.spaces.discarded, s.ID) != 1 {
		t.Fatalf("stopped %v synced %v discarded %v", e.backend.stopped, e.spaces.synced, e.spaces.discarded)
	}
	if err := c.Send(execute("1")); err == nil {
		t.Fatal("sent to an ended session")
	}
	if _, _, err := e.m.Attach(context.Background(), "alice", e.space, s.ID); !errors.Is(err, ErrEnded) {
		t.Fatalf("attach to an ended session: %v", err)
	}
	// Someone else's: not found.
	if _, err := e.m.End(context.Background(), "bob", e.space, s.ID, "bob"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal(err)
	}
}

// Colab's rule: the idle clock runs only while no cell runs and nobody is
// connected.
func TestIdleRule(t *testing.T) {
	e := newEnv(t, nil)
	s := e.open(t, "a.ipynb", "")
	c := e.attach(t, s.ID)
	e.clock.add(time.Hour) // connected: not idle
	time.Sleep(100 * time.Millisecond)
	if !e.session(t, s.ID).Open() {
		t.Fatal("ended while a client was connected")
	}
	_ = c.Send(execute("hang"))
	next(t, c, "iopub", "status")
	c.Close()
	e.clock.add(time.Hour) // a cell runs: not idle
	time.Sleep(100 * time.Millisecond)
	if !e.session(t, s.ID).Open() {
		t.Fatal("ended while a cell ran")
	}
	if err := e.m.Interrupt(context.Background(), "alice", e.space, s.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the cell to stop", func() bool { return !e.m.get(s.ID).quiet().IsZero() })
	e.clock.add(29 * time.Minute)
	time.Sleep(100 * time.Millisecond)
	if !e.session(t, s.ID).Open() {
		t.Fatal("ended before the idle time")
	}
	e.clock.add(2 * time.Minute)
	waitFor(t, "the idle session to end", func() bool { return !e.session(t, s.ID).Open() })
	if r := e.session(t, s.ID).EndReason; !strings.Contains(r, "idle") {
		t.Fatalf("reason %q", r)
	}
}

func TestMaximumLength(t *testing.T) {
	e := newEnv(t, nil)
	s := e.open(t, "a.ipynb", "")
	_ = e.attach(t, s.ID)
	e.clock.add(8*time.Hour + time.Second)
	waitFor(t, "the session to end", func() bool { return !e.session(t, s.ID).Open() })
	if r := e.session(t, s.ID).EndReason; !strings.Contains(r, "maximum") {
		t.Fatalf("reason %q", r)
	}
}

func TestWriteBackEveryFewMinutes(t *testing.T) {
	e := newEnv(t, nil)
	s := e.open(t, "a.ipynb", "")
	_ = e.attach(t, s.ID)
	e.clock.add(6 * time.Minute)
	waitFor(t, "a write-back", func() bool { return e.spaces.count(&e.spaces.synced, s.ID) >= 1 })
	if _, err := e.m.SyncFiles(context.Background(), "alice", e.space, s.ID); err != nil {
		t.Fatal(err)
	}
}

func TestEndedElsewhereStopsHere(t *testing.T) {
	e := newEnv(t, nil)
	s := e.open(t, "a.ipynb", "")
	c := e.attach(t, s.ID)
	// As another API node does: only the record.
	if _, err := e.db.EndSession(context.Background(), s.ID, "ended by an administrator", "root", time.Now()); err != nil {
		t.Fatal(err)
	}
	<-c.Done()
	waitFor(t, "the container to stop", func() bool { return e.backend.wasStopped(s.ID) })
}

func TestContainerThatDiesEndsTheSession(t *testing.T) {
	e := newEnv(t, nil)
	s := e.open(t, "a.ipynb", "")
	c := e.attach(t, s.ID)
	e.backend.crash(s.ID)
	<-c.Done()
	waitFor(t, "the session to end", func() bool { return !e.session(t, s.ID).Open() })
	if r := e.session(t, s.ID).EndReason; !strings.Contains(r, "container stopped") {
		t.Fatalf("reason %q", r)
	}
	if e.spaces.count(&e.spaces.synced, s.ID) != 1 {
		t.Fatal("changes not written back")
	}
}

func TestStartFailureEndsTheSession(t *testing.T) {
	e := newEnv(t, nil)
	e.backend.startErr = errors.New("no such image")
	s := e.open(t, "a.ipynb", "")
	if _, _, err := e.m.Attach(context.Background(), "alice", e.space, s.ID); !errors.Is(err, ErrEnded) {
		t.Fatalf("attach: %v", err)
	}
	if r := e.session(t, s.ID).EndReason; !strings.Contains(r, "no such image") {
		t.Fatalf("reason %q", r)
	}
}

func TestPerWorkerLimit(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.PerWorker, c.PerUser = 1, 0 })
	a := e.open(t, "a.ipynb", store.SessionReadOnly)
	e.attach(t, a.ID)
	b := e.open(t, "b.ipynb", store.SessionReadOnly)
	time.Sleep(100 * time.Millisecond)
	if got := e.session(t, b.ID); got.Worker != "" {
		t.Fatalf("a full worker took a session: %+v", got)
	}
	if _, err := e.m.End(context.Background(), "alice", e.space, a.ID, "alice"); err != nil {
		t.Fatal(err)
	}
	e.attach(t, b.ID) // now there's room
}

// nbsandboxd restarts: its sessions keep running and are attached again;
// one whose container is gone ends, with its changes kept.
func TestRestartReattaches(t *testing.T) {
	e := newEnv(t, nil)
	keep := e.open(t, "a.ipynb", "")
	lost := e.open(t, "b.ipynb", "")
	e.attach(t, keep.ID)
	e.attach(t, lost.ID)
	e.m.Shutdown(context.Background())
	if e.backend.wasStopped(keep.ID) {
		t.Fatal("shutdown stopped a session")
	}
	e.backend.mu.Lock()
	delete(e.backend.bridges, lost.ID)                                          // its container went away meanwhile
	e.backend.bridges["99999999-9999-4999-8999-999999999999"] = newFakeBridge() // and one nobody holds
	e.backend.mu.Unlock()

	m2 := New(e.m.cfg, e.m.d)
	if err := m2.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer m2.Shutdown(context.Background())
	c, _, err := m2.Attach(context.Background(), "alice", e.space, keep.ID)
	if err != nil {
		t.Fatal(err)
	}
	next(t, c, "bridge", "")
	_ = c.Send(execute("after"))
	next(t, c, "iopub", "stream")
	if got := e.session(t, lost.ID); got.Open() || !strings.Contains(got.EndReason, "interrupted") {
		t.Fatalf("lost session: %+v", got)
	}
	if !e.backend.wasStopped("99999999-9999-4999-8999-999999999999") {
		t.Fatal("a container nobody holds was left")
	}
}

func TestEndSpace(t *testing.T) {
	e := newEnv(t, nil)
	s := e.open(t, "a.ipynb", "")
	e.attach(t, s.ID)
	if err := e.m.EndSpace(context.Background(), e.space, "alice"); err != nil {
		t.Fatal(err)
	}
	if got := e.session(t, s.ID); got.Open() || got.EndReason != "the space was deleted" {
		t.Fatalf("%+v", got)
	}
}

// A recreated container gets a new host name, so a new worker ID: the
// sessions whose containers are on its Docker are adopted, not removed.
func TestRestartUnderAnotherWorkerID(t *testing.T) {
	e := newEnv(t, nil)
	s := e.open(t, "a.ipynb", "")
	c := e.attach(t, s.ID)
	_ = c.Send(execute("x"))
	next(t, c, "iopub", "stream")
	e.m.Shutdown(context.Background())

	cfg := e.m.cfg
	cfg.WorkerID = "w2"
	m2 := New(cfg, e.m.d)
	if err := m2.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer m2.Shutdown(context.Background())
	if e.backend.wasStopped(s.ID) {
		t.Fatal("the session's container was removed")
	}
	if got := e.session(t, s.ID); !got.Open() || got.Worker != "w2" {
		t.Fatalf("%+v", got)
	}
	c2, _, err := m2.Attach(context.Background(), "alice", e.space, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	next(t, c2, "bridge", "")
	_ = c2.Send(execute("again"))
	next(t, c2, "iopub", "stream")
}
