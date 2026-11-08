// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

// Package blob is nbsandbox's object storage: where file contents, published
// function versions and run outputs live, so that nbsandboxd nodes keep
// nothing they can't lose.
//
// Objects are immutable. File contents are stored once under their SHA-256
// (ContentKey) and everything that needs consistency (which path has which
// contents, versions, renames, quotas) lives in the database. So the
// interface needs no conditional writes, renames or fast listings: put, get,
// head, and delete and list for cleanup. That is the lowest common
// denominator of S3-compatible stores (RustFS, Ceph, AWS S3, ...) and
// Azure Blob Storage; every adapter passes the same test suite
// (blobtest.Run).
package blob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"
)

// Store holds immutable objects by key.
type Store interface {
	// Put stores exactly size bytes from r under key. Putting the same key
	// again with the same bytes is harmless (content keys make that the
	// norm). r yielding fewer or more bytes than size is an error and
	// stores nothing.
	Put(ctx context.Context, key string, r io.Reader, size int64) error
	// Get opens an object. ErrNotFound if there is none.
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	// Head returns an object's size. ErrNotFound if there is none.
	Head(ctx context.Context, key string) (Info, error)
	// Delete removes an object; deleting a missing one is not an error.
	// For cleanup only.
	Delete(ctx context.Context, key string) error
	// List calls fn for every object whose key starts with prefix, in no
	// particular order, until fn returns an error (which List returns).
	// For cleanup only: slow on large stores.
	List(ctx context.Context, prefix string, fn func(Info) error) error
}

// Info describes an object.
type Info struct {
	Key  string
	Size int64
	// Modified is when the object was last written (cleanup only removes
	// unreferenced objects that are old enough).
	Modified time.Time
}

// ErrNotFound: no object with that key.
var ErrNotFound = errors.New("blob: object not found")

// ErrSizeMismatch: Put's reader didn't yield exactly the size given.
var ErrSizeMismatch = errors.New("blob: content length differs from the size given")

// MaxKeyLen bounds keys (S3 allows 1024 bytes, Azure 1024 characters).
const MaxKeyLen = 512

// Keys are lowercase ASCII paths: segments of [a-z0-9._-] separated by "/".
// The same rule everywhere avoids differences between stores in case,
// escaping and special characters.
var keyRE = regexp.MustCompile(`^[a-z0-9._-]+(/[a-z0-9._-]+)*$`)

// ValidKey reports whether key may be used: a non-empty, relative path of
// lowercase ASCII segments, no "." or ".." segments, at most MaxKeyLen bytes.
func ValidKey(key string) bool {
	if len(key) > MaxKeyLen || !keyRE.MatchString(key) {
		return false
	}
	for _, seg := range strings.Split(key, "/") {
		if seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// ValidPrefix reports whether prefix may be listed: "" (everything), or a
// valid key optionally followed by "/".
func ValidPrefix(prefix string) bool {
	return prefix == "" || ValidKey(strings.TrimSuffix(prefix, "/"))
}

// KeyError reports an invalid key or prefix.
type KeyError struct{ Key string }

func (e *KeyError) Error() string { return fmt.Sprintf("blob: invalid key %q", e.Key) }

// ContentKey is where contents with this SHA-256 (lowercase hex) are stored:
// sha256/ab/cd/abcd…, spread over prefixes so no listing is huge.
func ContentKey(sum string) string {
	return "sha256/" + sum[:2] + "/" + sum[2:4] + "/" + sum
}

// RefreshAfter: contents already stored but older than this are written
// again when reused, so cleanup (which removes only unreferenced contents
// older than its grace period, longer than this) never removes contents an
// upload is about to record.
const RefreshAfter = 10 * time.Minute

// PutContent stores r's bytes under their SHA-256 and returns the sum and
// size. Contents already stored aren't uploaded again. The bytes are spooled
// to a temporary file in tmpDir ("" = the system default) first, since the
// key is known only after reading them all.
func PutContent(ctx context.Context, s Store, r io.Reader, tmpDir string) (sum string, size int64, err error) {
	f, err := os.CreateTemp(tmpDir, "nbsandbox-blob-*")
	if err != nil {
		return "", 0, err
	}
	defer func() {
		_ = f.Close()
		_ = os.Remove(f.Name())
	}()
	h := sha256.New()
	size, err = io.Copy(io.MultiWriter(f, h), r)
	if err != nil {
		return "", 0, err
	}
	sum = hex.EncodeToString(h.Sum(nil))
	key := ContentKey(sum)
	if info, err := s.Head(ctx, key); err == nil && info.Size == size && time.Since(info.Modified) < RefreshAfter {
		return sum, size, nil // already there, and recent
	} else if err != nil && !errors.Is(err, ErrNotFound) {
		return "", 0, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", 0, err
	}
	if err := s.Put(ctx, key, f, size); err != nil {
		return "", 0, err
	}
	return sum, size, nil
}

// ExactReader yields exactly size bytes from r and fails (ErrSizeMismatch)
// when r has fewer or more: before handing over the last bytes it checks r
// has nothing after them. Adapters wrap Put's reader in one, so an upload
// with the wrong length fails instead of storing a truncated or cut-off
// object.
type ExactReader struct {
	r        io.Reader
	left     int64
	mismatch bool
}

// Read implements io.Reader.
func (e *ExactReader) Read(p []byte) (int, error) {
	if e.left == 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > e.left {
		p = p[:e.left]
	}
	n, err := e.r.Read(p)
	e.left -= int64(n)
	// On a mismatch the bytes just read are withheld (0, error): callers
	// such as io.ReadFull ignore an error that arrives with the last bytes
	// they asked for, and would go on to store a wrong object.
	if e.left > 0 && errors.Is(err, io.EOF) {
		e.mismatch = true
		return 0, ErrSizeMismatch
	}
	if e.left == 0 && (err == nil || errors.Is(err, io.EOF)) {
		var one [1]byte
		if m, _ := io.ReadFull(e.r, one[:]); m > 0 {
			e.mismatch = true
			return 0, ErrSizeMismatch
		}
		return n, io.EOF
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return n, err
	}
	return n, nil
}

// NewExactReader wraps r to yield exactly size bytes.
func NewExactReader(r io.Reader, size int64) *ExactReader { return &ExactReader{r: r, left: size} }

// Mismatch reports whether r had fewer or more bytes than the size.
func (e *ExactReader) Mismatch() bool { return e.mismatch }
