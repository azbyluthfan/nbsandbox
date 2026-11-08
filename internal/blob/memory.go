// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package blob

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"time"
)

// Memory is an in-memory Store for unit tests. It passes the same test
// suite as the real adapters.
type Memory struct {
	mu      sync.RWMutex
	objects map[string][]byte
	times   map[string]time.Time
	// Now is the clock (tests set it).
	Now func() time.Time
}

// NewMemory returns an empty store.
func NewMemory() *Memory {
	return &Memory{objects: map[string][]byte{}, times: map[string]time.Time{}, Now: time.Now}
}

// Ping implements the readiness check (always reachable).
func (m *Memory) Ping(context.Context) error { return nil }

// Put implements Store.
func (m *Memory) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	if !ValidKey(key) {
		return &KeyError{Key: key}
	}
	if size < 0 {
		return ErrSizeMismatch
	}
	// Read one byte more than size, to notice a reader that yields too much.
	b, err := io.ReadAll(io.LimitReader(contextReader{ctx, r}, size+1))
	if err != nil {
		return err
	}
	if int64(len(b)) != size {
		return ErrSizeMismatch
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = b
	m.times[key] = m.Now()
	return nil
}

// Get implements Store.
func (m *Memory) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if !ValidKey(key) {
		return nil, &KeyError{Key: key}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	b, ok := m.objects[key]
	if !ok {
		return nil, ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

// Head implements Store.
func (m *Memory) Head(ctx context.Context, key string) (Info, error) {
	if !ValidKey(key) {
		return Info{}, &KeyError{Key: key}
	}
	if err := ctx.Err(); err != nil {
		return Info{}, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	b, ok := m.objects[key]
	if !ok {
		return Info{}, ErrNotFound
	}
	return Info{Key: key, Size: int64(len(b)), Modified: m.times[key]}, nil
}

// Delete implements Store.
func (m *Memory) Delete(ctx context.Context, key string) error {
	if !ValidKey(key) {
		return &KeyError{Key: key}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, key)
	delete(m.times, key)
	return nil
}

// List implements Store.
func (m *Memory) List(ctx context.Context, prefix string, fn func(Info) error) error {
	if !ValidPrefix(prefix) {
		return &KeyError{Key: prefix}
	}
	m.mu.RLock()
	var found []Info
	for k, b := range m.objects {
		if strings.HasPrefix(k, prefix) {
			found = append(found, Info{Key: k, Size: int64(len(b)), Modified: m.times[k]})
		}
	}
	m.mu.RUnlock()
	for _, info := range found {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := fn(info); err != nil {
			return err
		}
	}
	return nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (c contextReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
