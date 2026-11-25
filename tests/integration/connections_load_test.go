// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/blob/s3store"
)

// Reading under load (make load-connections): what a fetcher adds to a run,
// many reading runs at once, and how fast data comes through the gateway.
// Numbers for docs/perf.md; it fails only if a run fails or something is
// left behind. Skipped unless NBSANDBOX_LOAD=1.

func quantile(d []time.Duration, q float64) time.Duration {
	if len(d) == 0 {
		return 0
	}
	s := slices.Clone(d)
	slices.Sort(s)
	return s[min(len(s)-1, int(q*float64(len(s))))]
}

func TestConnectionsLoad(t *testing.T) {
	needConnections(t)
	if os.Getenv("NBSANDBOX_LOAD") != "1" {
		t.Skip("set NBSANDBOX_LOAD=1 (make load-connections)")
	}
	burst := 16
	if v, err := strconv.Atoi(os.Getenv("LOAD_RUNS")); err == nil && v > 0 {
		burst = v
	}
	// A space runs one notebook at a time, so every run at once has its own.
	spaces := make([]string, max(burst, 4))
	for i := range spaces {
		spaces[i] = readerSpace(t, codeNotebook(`import nbsandbox
nbsandbox.output(int(nbsandbox.sql("SELECT count(*) FROM crm.orders").iloc[0, 0]))`))
		upload(t, "alice", spaces[i], "", map[string]string{
			"plain.ipynb": codeNotebook(`import nbsandbox
nbsandbox.output(1000)`),
			"big.ipynb": codeNotebook(`import nbsandbox, time
t0 = time.time()
r = nbsandbox.sql("SELECT count(*) AS n, sum(amount)::BIGINT AS total FROM lake.csv('big.csv')")
nbsandbox.output({"n": int(r.n.iloc[0]), "seconds": time.time() - t0})`),
		})
	}
	// timed runs a notebook to the end: request to result.
	timed := func(t *testing.T, sp, nb string) (time.Duration, callerResult) {
		t0 := time.Now()
		run := startRun(t, "alice", sp, nb, nil)
		res := runResult(t, "alice", sp, run.ID)
		if res.Status != "succeeded" {
			t.Errorf("%s: %s %s", nb, res.Status, res.Output)
		}
		return time.Since(t0), res
	}
	// atOnce runs f in n parallel subtests, one space each, and returns the wall time.
	atOnce := func(name string, n int, f func(t *testing.T, sp string)) time.Duration {
		t0 := time.Now()
		t.Run(name, func(t *testing.T) {
			for i := range n {
				t.Run(strconv.Itoa(i), func(t *testing.T) {
					t.Parallel()
					f(t, spaces[i])
				})
			}
		})
		return time.Since(t0)
	}

	// 1. A fetcher's cost: the same one-cell notebook with and without a read.
	var plain, reading []time.Duration
	for range 5 {
		d, _ := timed(t, spaces[0], "plain.ipynb")
		plain = append(plain, d)
		d, _ = timed(t, spaces[0], "read.ipynb")
		reading = append(reading, d)
	}
	overhead := fmt.Sprintf("a run, median of 5: %.2f s without reading, %.2f s with one query (the fetcher started and attached first)",
		quantile(plain, 0.5).Seconds(), quantile(reading, 0.5).Seconds())

	// 2. A burst: many reading runs at once, queued by the concurrency limit.
	var mu sync.Mutex
	var each []time.Duration
	wall := atOnce("burst", burst, func(t *testing.T, sp string) {
		d, _ := timed(t, sp, "read.ipynb")
		mu.Lock()
		each = append(each, d)
		mu.Unlock()
	})
	burstNote := fmt.Sprintf("%d reading runs at once (%s at a time): all done in %.1f s; request to result p50 %.1f s, p95 %.1f s (queueing included)",
		burst, cmp.Or(os.Getenv("NBSANDBOX_RUN_CONCURRENCY"), "2"), wall.Seconds(), quantile(each, 0.5).Seconds(), quantile(each, 0.95).Seconds())

	// 3. Through the gateway: a CSV of about 64 MiB read from object storage.
	const rows = 2_000_000
	var csv strings.Builder
	csv.Grow(rows * 33)
	csv.WriteString("region,amount,note\n")
	for i := range rows {
		fmt.Fprintf(&csv, "%s,%d,row-%020d\n", []string{"eu", "us"}[i%2], i%100, i)
	}
	size := csv.Len()
	port := strings.TrimPrefix(docker(t, "port", srcS3, "9000/tcp"), "127.0.0.1:")
	st, err := s3store.New(context.Background(), s3store.Config{Endpoint: "127.0.0.1:" + port, Bucket: "lake", AccessKey: "reader", SecretKey: s3Secret})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Put(context.Background(), "big.csv", strings.NewReader(csv.String()), int64(size)); err != nil {
		t.Fatal(err)
	}
	mib := float64(size) / (1 << 20)
	through := func(t *testing.T, sp string) float64 {
		_, res := timed(t, sp, "big.ipynb")
		var out struct {
			N       int
			Seconds float64
		}
		_ = json.Unmarshal(res.Output, &out)
		if out.N != rows || out.Seconds <= 0 {
			t.Errorf("big read: %s", res.Output)
			return 0
		}
		return mib / out.Seconds
	}
	one := through(t, spaces[0])
	var agg float64
	atOnce("gateway", 4, func(t *testing.T, sp string) {
		v := through(t, sp)
		mu.Lock()
		agg += v
		mu.Unlock()
	})
	gwNote := fmt.Sprintf("a %.0f MiB CSV through the gateway: %.0f MiB/s for one run, %.0f MiB/s in all for four at once", mib, one, agg)

	assertNoReadLeftovers(t)
	got := overhead + "; " + burstNote + "; " + gwNote
	finding(t, "LOAD-connections", "reading runs measured alone, in a burst, and reading a large file through the gateway",
		"every run succeeds; nothing is left behind (the figures are for docs/perf.md)", got, !t.Failed())
	t.Log(got)
}
