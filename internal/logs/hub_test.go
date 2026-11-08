// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package logs

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type buf struct {
	mu sync.Mutex
	bytes.Buffer
	closed bool
	fail   bool
}

func (b *buf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.fail {
		return 0, errors.New("disk full")
	}
	return b.Buffer.Write(p)
}
func (b *buf) Close() error { b.closed = true; return nil }
func (b *buf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.String()
}

func drain(t *testing.T, sub *Subscription) (lines []Event, done *Event) {
	t.Helper()
	for ev := range sub.C {
		if ev.Done {
			e := ev
			done = &e
			continue
		}
		lines = append(lines, ev)
	}
	return lines, done
}

func TestCapAndTruncation(t *testing.T) {
	var b buf
	h := NewHub(&b, 12)
	h.Line("hello") // 6 bytes
	h.Line("world") // 12: exactly at the cap
	h.Line("more")  // over: notice, then dropped
	h.Line("again")
	_, n, truncated, err := h.Stats()
	if n != 12+int64(len("[nbsandbox] log truncated at 12 bytes; further output was dropped (the run continues)"))+1 || !truncated || err != nil {
		t.Fatalf("stats %d %v %v", n, truncated, err)
	}
	got := b.String()
	if !strings.HasPrefix(got, "hello\nworld\n[nbsandbox] log truncated at 12 bytes") || strings.Count(got, "[nbsandbox]") != 1 || strings.Contains(got, "again") {
		t.Fatalf("log = %q", got)
	}
	backlog, _ := h.Subscribe(0)
	if len(backlog) != 3 || backlog[2].Seq != 3 || !strings.HasPrefix(backlog[2].Text, "[nbsandbox] log truncated") {
		t.Fatalf("the notice is a numbered line like any other: %+v", backlog)
	}
	_ = h.Close("succeeded")
	if !b.closed {
		t.Fatal("log.txt not closed")
	}
}

// A viewer joining or resuming at any point sees each
// line exactly once, in order.
func TestSubscribeNoGapsNoDuplicates(t *testing.T) {
	for _, after := range []int64{0, 3, 10} {
		t.Run(fmt.Sprint("after ", after), func(t *testing.T) {
			var b buf
			h := NewHub(&b, 1<<20)
			for i := 1; i <= 10; i++ {
				h.Line(fmt.Sprint("line ", i))
			}
			backlog, sub := h.Subscribe(after)
			go func() {
				for i := 11; i <= 20; i++ {
					h.Line(fmt.Sprint("line ", i))
				}
				_ = h.Close("succeeded")
			}()
			live, done := drain(t, sub)
			next := after + 1
			for _, ev := range append(backlog, live...) {
				if ev.Seq != next || ev.Text != fmt.Sprint("line ", ev.Seq) {
					t.Fatalf("got seq %d %q, want %d", ev.Seq, ev.Text, next)
				}
				next++
			}
			if next != 21 || done == nil || done.Status != "succeeded" {
				t.Fatalf("ended at %d, done %+v", next-1, done)
			}
		})
	}
}

// Concurrent writer and many subscribers joining at random points: every
// subscriber's backlog + live lines are exactly the lines after its point.
func TestConcurrentSubscribersSeeExactSequence(t *testing.T) {
	var b buf
	h := NewHub(&b, 1<<20)
	const total = 500
	var wg sync.WaitGroup
	errs := make(chan error, 50)
	start := make(chan struct{})
	for s := 0; s < 20; s++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			backlog, sub := h.Subscribe(int64(s * 7))
			live, done := drain(t, sub)
			all := append(backlog, live...)
			next := int64(s*7) + 1
			for _, ev := range all {
				if ev.Seq != next {
					errs <- fmt.Errorf("subscriber %d: got seq %d, want %d", s, ev.Seq, next)
					return
				}
				next++
			}
			if next != total+1 || done == nil {
				errs <- fmt.Errorf("subscriber %d: ended at %d, done %v", s, next-1, done)
			}
		}()
	}
	close(start)
	for i := 1; i <= total; i++ {
		h.Line(fmt.Sprint(i))
	}
	_ = h.Close("succeeded")
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// A slow viewer is dropped; the writer never blocks.
func TestSlowSubscriberIsDropped(t *testing.T) {
	var b buf
	h := NewHub(&b, 1<<30)
	_, slow := h.Subscribe(0)
	_, fast := h.Subscribe(0)
	var fastGot atomic.Int64
	done := make(chan struct{})
	go func() {
		for ev := range fast.C {
			if !ev.Done {
				fastGot.Add(1)
			}
		}
		close(done)
	}()
	// Write in half-buffer batches and let the fast viewer catch up after
	// each, so only `slow` (which never reads) can fall behind, however
	// loaded the machine is.
	batch := SubscriberBuffer / 2
	for written := 0; written < SubscriberBuffer*3; {
		for i := 0; i < batch; i++ {
			h.Line("x") // must not block although `slow` never reads
		}
		written += batch
		for deadline := time.Now().Add(5 * time.Second); fastGot.Load() < int64(written) && time.Now().Before(deadline); {
			time.Sleep(time.Millisecond)
		}
	}
	_ = h.Close("succeeded")
	<-done
	if !slow.Evicted() || fast.Evicted() || fastGot.Load() != SubscriberBuffer*3 {
		t.Fatalf("slow evicted %v, fast evicted %v, fast got %d", slow.Evicted(), fast.Evicted(), fastGot.Load())
	}
	n := 0
	for range slow.C {
		n++
	}
	if n != SubscriberBuffer {
		t.Fatalf("slow subscriber kept %d buffered lines, want %d", n, SubscriberBuffer)
	}
}

// Joining after the end gives the end at once.
func TestSubscribeAfterClose(t *testing.T) {
	var b buf
	h := NewHub(&b, 1<<20)
	h.Line("a")
	_ = h.Close("failed")
	backlog, sub := h.Subscribe(0)
	_, done := drain(t, sub)
	if len(backlog) != 1 || backlog[0].Text != "a" || done == nil || done.Status != "failed" {
		t.Fatalf("backlog %v done %+v", backlog, done)
	}
	h.Line("after close")
	if strings.Contains(b.String(), "after close") {
		t.Fatal("written after close")
	}
	if err := h.Close("x"); err != nil {
		t.Fatal(err)
	}
}

func TestUnsubscribeAndRegistry(t *testing.T) {
	var b buf
	r := NewRegistry()
	h := r.Open("run1", &b, 1<<20)
	if r.Get("run1") != h || r.Get("other") != nil {
		t.Fatal("registry lookup")
	}
	_, sub := h.Subscribe(0)
	if h.Subscribers() != 1 {
		t.Fatal("not subscribed")
	}
	h.Unsubscribe(sub)
	h.Unsubscribe(sub)
	if _, ok := <-sub.C; ok || h.Subscribers() != 0 {
		t.Fatal("unsubscribe did not close the channel")
	}
	h.Line("still written")
	r.Remove("run1")
	if r.Get("run1") != nil {
		t.Fatal("not removed")
	}
}

func TestWriteError(t *testing.T) {
	b := buf{fail: true}
	h := NewHub(&b, 100)
	h.Line("x")
	if _, _, _, err := h.Stats(); err == nil {
		t.Fatal("write error not reported")
	}
}

func TestConcurrentLinesStayWhole(t *testing.T) {
	var b buf
	h := NewHub(&b, 1<<20)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				h.Line("0123456789")
			}
		}()
	}
	wg.Wait()
	for _, l := range strings.Split(strings.TrimSuffix(b.String(), "\n"), "\n") {
		if l != "0123456789" {
			t.Fatalf("broken line %q", l)
		}
	}
}

// A reader goes at its own pace: it never misses a line, however far behind,
// waits for more, and sees the end.
func TestReadAtOwnPace(t *testing.T) {
	var b buf
	h := NewHub(&b, 1<<20)
	lines, done, _, more := h.Read(0, 10)
	if len(lines) != 0 || done || more == nil {
		t.Fatalf("empty log: %v %v %v", lines, done, more)
	}
	for i := range SubscriberBuffer * 3 { // far more than a subscriber may fall behind
		h.Line(fmt.Sprint(i + 1))
	}
	select {
	case <-more:
	default:
		t.Fatal("no wake-up for new lines")
	}
	var after int64
	for {
		lines, done, _, _ := h.Read(after, 100)
		if len(lines) == 0 || done {
			break
		}
		for _, ev := range lines {
			if ev.Seq != after+1 || ev.Text != fmt.Sprint(ev.Seq) {
				t.Fatalf("line %+v after %d", ev, after)
			}
			after = ev.Seq
		}
	}
	if after != SubscriberBuffer*3 {
		t.Fatalf("read %d lines", after)
	}
	_, done, _, more = h.Read(after, 100)
	if done || more == nil {
		t.Fatal("ended early")
	}
	_ = h.Close("succeeded")
	<-more
	lines, done, status, _ := h.Read(after, 100)
	if len(lines) != 0 || !done || status != "succeeded" {
		t.Fatalf("end: %v %v %q", lines, done, status)
	}
	// After the end the lines are still there for a reader holding the hub.
	if lines, _, _, _ := h.Read(-5, 2); len(lines) != 2 || lines[0].Seq != 1 {
		t.Fatalf("reread %v", lines)
	}
}
