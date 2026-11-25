// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package sessions

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/store"
)

const testSecret = "0123456789abcdef-secret"

// cluster is a worker holding sessions behind its internal handler, and an
// API node (no backend) reaching it through the shared database.
type cluster struct {
	*env
	api  *Manager
	addr string // the worker's internal listener
}

func newCluster(t *testing.T) *cluster {
	t.Helper()
	e := newEnv(t, func(c *Config) { c.Shared, c.Secret, c.WorkerID = true, testSecret, "w1" })
	srv := httptest.NewServer(e.m.InternalHandler())
	t.Cleanup(srv.Close)
	c := &cluster{env: e, addr: strings.TrimPrefix(srv.URL, "http://")}
	e.setWorkers([]store.Worker{{ID: "w1", Up: true, SessionAddr: c.addr}})
	cfg := e.m.cfg
	cfg.NoExecute, cfg.WorkerID = true, "api-1"
	c.api = New(cfg, Deps{DB: e.db, Spaces: e.spaces, Now: e.clock.Now, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Workers: e.liveWorkers})
	if err := c.api.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.api.Shutdown(context.Background()) })
	return c
}

func TestAPINodeReachesTheWorkersSession(t *testing.T) {
	c := newCluster(t)
	ctx := context.Background()
	s, _, err := c.api.Open(ctx, "alice", c.space, "a.ipynb", "", "")
	if err != nil {
		t.Fatal(err)
	}
	c.m.Wake() // the database's notification, in production
	conn, got, err := c.api.Attach(ctx, "alice", c.space, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if got.Worker != "w1" {
		t.Fatalf("%+v", got)
	}
	msgs := conn.Messages()
	nextOf := func(channel, typ string) {
		t.Helper()
		timeout := time.After(5 * time.Second)
		for {
			select {
			case line, ok := <-msgs:
				if !ok {
					t.Fatalf("closed waiting for %s/%s: %s", channel, typ, conn.Reason())
				}
				if strings.Contains(string(line), `"channel":"`+channel+`"`) && strings.Contains(string(line), typ) {
					return
				}
			case <-timeout:
				t.Fatalf("no %s/%s", channel, typ)
			}
		}
	}
	nextOf("bridge", "attached")
	if err := conn.Send(execute("via the api node")); err != nil {
		t.Fatal(err)
	}
	nextOf("iopub", "out:via the api node")
	// Refused on the API node already.
	var refused *RefusedError
	if err := conn.Send([]byte(`{"channel":"shell","header":{"msg_id":"1","msg_type":"history_request"}}`)); !errors.As(err, &refused) {
		t.Fatalf("history_request: %v", err)
	}
	// Actions go to the worker.
	if err := c.api.Interrupt(ctx, "alice", c.space, s.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := c.api.SyncFiles(ctx, "alice", c.space, s.ID); err != nil {
		t.Fatal(err)
	}
	if c.spaces.count(&c.spaces.synced, s.ID) != 1 {
		t.Fatal("the worker didn't sync")
	}
	// Ending it on the API node: the worker ends it before the answer, its
	// changes written back, so a client reading the space next sees them.
	ended, err := c.api.End(ctx, "alice", c.space, s.ID, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if ended.Open() || ended.EndReason != "ended by alice" || ended.EndedBy != "alice" {
		t.Fatalf("ended %+v", ended)
	}
	if !c.backend.wasStopped(s.ID) || c.spaces.count(&c.spaces.synced, s.ID) != 2 {
		t.Fatalf("stopped %v, synced %d times", c.backend.wasStopped(s.ID), c.spaces.count(&c.spaces.synced, s.ID))
	}
}

// A worker that can't be reached: the record ends anyway, and the worker
// stops the session when it next looks.
func TestEndWhenTheWorkerCantBeReached(t *testing.T) {
	c := newCluster(t)
	ctx := context.Background()
	s, _, err := c.api.Open(ctx, "alice", c.space, "a.ipynb", "", "")
	if err != nil {
		t.Fatal(err)
	}
	c.m.Wake()
	if _, got, err := c.api.Attach(ctx, "alice", c.space, s.ID); err != nil || got.Worker != "w1" {
		t.Fatalf("%+v %v", got, err)
	}
	c.setWorkers([]store.Worker{{ID: "w1", Up: true, SessionAddr: "127.0.0.1:1"}}) // nothing listens there
	ended, err := c.api.End(ctx, "alice", c.space, s.ID, "alice")
	if err != nil || ended.Open() {
		t.Fatalf("%+v %v", ended, err)
	}
	c.m.Wake()
	waitFor(t, "the worker to stop it", func() bool { return c.backend.wasStopped(s.ID) })
}

func TestInternalHandlerNeedsTheSecret(t *testing.T) {
	c := newCluster(t)
	s, _, err := c.api.Open(context.Background(), "alice", c.space, "a.ipynb", "", "")
	if err != nil {
		t.Fatal(err)
	}
	c.m.Wake()
	if _, err := c.m.waitLive(context.Background(), s.ID); err != nil {
		t.Fatal(err)
	}
	for _, auth := range []string{"", "Bearer wrong", "Basic " + testSecret, testSecret} {
		req, _ := http.NewRequest("POST", "http://"+c.addr+InternalPrefix+s.ID+"/interrupt", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%q: %d", auth, resp.StatusCode)
		}
	}
	// An API node with the wrong secret can't reach it either.
	cfg := c.api.cfg
	cfg.Secret = "another-secret-entirely"
	wrong := New(cfg, c.api.d)
	if err := wrong.Interrupt(context.Background(), "alice", c.space, s.ID); !errors.Is(err, ErrNotHere) {
		t.Fatalf("wrong secret: %v", err)
	}
}

// A worker that's gone for good: its sessions end on the other nodes.
func TestSessionsOfAGoneWorkerEnd(t *testing.T) {
	c := newCluster(t)
	ctx := context.Background()
	s, _, err := c.api.Open(ctx, "alice", c.space, "a.ipynb", "", "")
	if err != nil {
		t.Fatal(err)
	}
	c.m.Wake()
	if _, err := c.m.waitLive(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	c.setWorkers(nil) // w1 stopped publishing itself
	time.Sleep(200 * time.Millisecond)
	if !c.session(t, s.ID).Open() {
		t.Fatal("ended at once: a restarting worker comes back within a minute or two")
	}
	c.clock.add(3 * time.Minute)
	waitFor(t, "the session to end", func() bool { return !c.session(t, s.ID).Open() })
	if r := c.session(t, s.ID).EndReason; !strings.Contains(r, "worker is gone") {
		t.Fatalf("reason %q", r)
	}
}

// A session no worker takes ends, rather than counting against its owner.
func TestUnclaimedSessionEnds(t *testing.T) {
	c := newCluster(t)
	// No worker takes it: the worker is stopped first (draining it instead
	// races with a claim already on its way to the database).
	c.m.Shutdown(context.Background())
	s, _, err := c.api.Open(context.Background(), "alice", c.space, "a.ipynb", "", "")
	if err != nil {
		t.Fatal(err)
	}
	c.clock.add(10 * time.Minute)
	waitFor(t, "the session to end", func() bool { return !c.session(t, s.ID).Open() })
	if got := c.session(t, s.ID); got.Worker != "" || got.EndReason != "no worker could start it" {
		t.Fatalf("%+v", got)
	}
}
