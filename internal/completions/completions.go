// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

// Package completions shares the runner image's completion index (the names,
// signatures and docs of its importable modules, for editors) between the
// worker that read it out of the image and the API processes that serve it.
// The worker stores it in object storage under its SHA-256; the runtime
// info names that hash; any API process opens it from there.
package completions

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"regexp"
	"sync"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/blob"
)

// Ref names a stored index.
type Ref struct {
	SHA256 string `json:"sha256"` // of the gzipped JSON
	Bytes  int64  `json:"bytes"`  // gzipped
}

// MaxJSON bounds an index once uncompressed.
const MaxJSON = 64 << 20

// ErrNone: the runner image has no completion index (or it isn't stored yet).
var ErrNone = errors.New("the runner image has no completion index")

const prefix = "runtime/completions/"

var hexSHA = regexp.MustCompile(`^[0-9a-f]{64}$`)

// key is where an index is stored.
func key(sha string) string { return prefix + sha + ".json.gz" }

// Publisher stores the index once per content.
type Publisher struct {
	ctx   context.Context // stores outlive requests, not the process
	blobs blob.Store
	log   *slog.Logger

	mu     sync.Mutex
	last   []byte // the index last hashed (same backing array: same index)
	ref    Ref
	stored bool
	busy   bool
}

// NewPublisher stores indexes in blobs, until ctx ends.
func NewPublisher(ctx context.Context, blobs blob.Store, log *slog.Logger) *Publisher {
	return &Publisher{ctx: ctx, blobs: blobs, log: log}
}

// Ref returns the stored index's reference, or nil while it is being stored
// (in the background) or when there is none.
func (p *Publisher) Ref(index []byte) *Ref {
	if len(index) == 0 {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !sameSlice(p.last, index) {
		sum := sha256.Sum256(index)
		p.last, p.ref, p.stored = index, Ref{SHA256: hex.EncodeToString(sum[:]), Bytes: int64(len(index))}, false
	}
	if p.stored {
		r := p.ref
		return &r
	}
	if !p.busy {
		p.busy = true
		go p.store(index, p.ref)
	}
	return nil
}

func (p *Publisher) store(index []byte, ref Ref) {
	ctx, cancel := context.WithTimeout(p.ctx, 2*time.Minute)
	defer cancel()
	err := p.blobs.Put(ctx, key(ref.SHA256), bytes.NewReader(index), int64(len(index)))
	p.mu.Lock()
	defer p.mu.Unlock()
	p.busy = false
	if err != nil {
		p.log.Warn("store the completion index", "error", err)
		return
	}
	if p.ref == ref {
		p.stored = true
	}
}

func sameSlice(a, b []byte) bool {
	return len(a) == len(b) && len(a) > 0 && &a[0] == &b[0]
}

// Open opens a stored index (gzipped JSON).
func Open(ctx context.Context, blobs blob.Store, ref Ref) (io.ReadCloser, error) {
	if !hexSHA.MatchString(ref.SHA256) {
		return nil, ErrNone
	}
	rc, err := blobs.Get(ctx, key(ref.SHA256))
	if errors.Is(err, blob.ErrNotFound) {
		return nil, ErrNone
	}
	return rc, err
}
