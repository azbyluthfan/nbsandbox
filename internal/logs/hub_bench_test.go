// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package logs

import (
	"fmt"
	"io"
	"sync"
	"testing"
)

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
func (discard) Close() error                { return nil }

var _ io.WriteCloser = discard{}

// Lines per second through the hub with 0, 1, 10 and 100 viewers
// reading as fast as they can. Results go in docs/perf.md.
func BenchmarkHubFanOut(b *testing.B) {
	line := "2026-11-06 12:00:00 epoch 12 loss 0.0345 accuracy 0.9812 lr 0.001"
	for _, subs := range []int{0, 1, 10, 100} {
		b.Run(fmt.Sprintf("%d viewers", subs), func(b *testing.B) {
			h := NewHub(discard{}, 1<<62)
			var wg sync.WaitGroup
			for i := 0; i < subs; i++ {
				_, sub := h.Subscribe(0)
				wg.Add(1)
				go func() {
					defer wg.Done()
					for range sub.C {
					}
				}()
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				h.Line(line)
			}
			b.StopTimer()
			_ = h.Close("succeeded")
			wg.Wait()
		})
	}
}
