// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package spaces

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/azbyluthfan/nbsandbox/internal/blob"
	"github.com/azbyluthfan/nbsandbox/internal/store"
)

// Domain errors. The API maps them to status codes; nothing here knows HTTP.
var (
	// ErrNotFound: missing, or owned by someone else.
	ErrNotFound = errors.New("not found")
	// ErrActiveRun: the space has a queued or running run.
	ErrActiveRun = errors.New("the space has an active run")
)

// InvalidError is a request the caller must fix (400).
type InvalidError struct{ Msg string }

func (e *InvalidError) Error() string { return e.Msg }

// LimitError is a size or count limit being exceeded (413).
type LimitError struct{ Msg string }

func (e *LimitError) Error() string { return e.Msg }

func invalid(format string, args ...any) error {
	return &InvalidError{Msg: fmt.Sprintf(format, args...)}
}

// MaxNameBytes limits a space's display name.
const MaxNameBytes = 100

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// ValidID reports whether id is a canonical lowercase UUID.
func ValidID(id string) bool { return uuidRE.MatchString(id) }

// NewID returns a random (version 4) UUID.
func NewID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// Config configures the service.
type Config struct {
	DataDir           string
	MaxFile           int64 // per uploaded file
	MaxSpace          int64 // per space, including run outputs
	MaxFilesPerUpload int   // parts in one upload request (default 100)
	// MaxSpaces bounds how many spaces one owner may have (0: no limit).
	MaxSpaces int
	// Owners overrides MaxSpaces and MaxSpace for particular owner IDs.
	Owners map[string]OwnerLimits
}

// OwnerLimits are one owner's limits; zero fields keep the defaults.
type OwnerLimits struct {
	MaxSpaces     int
	MaxSpaceBytes int64
}

// limitsFor is owner's space count limit and per-space quota.
func (s *Service) limitsFor(owner string) (maxSpaces int, maxSpaceBytes int64) {
	maxSpaces, maxSpaceBytes = s.cfg.MaxSpaces, s.cfg.MaxSpace
	if o, ok := s.cfg.Owners[owner]; ok {
		if o.MaxSpaces != 0 {
			maxSpaces = o.MaxSpaces
		}
		if o.MaxSpaceBytes != 0 {
			maxSpaceBytes = o.MaxSpaceBytes
		}
	}
	return maxSpaces, maxSpaceBytes
}

// quota is a space's quota: its owner's.
func (s *Service) quota(ctx context.Context, id string) int64 {
	if len(s.cfg.Owners) == 0 {
		return s.cfg.MaxSpace
	}
	owner, err := s.db.SpaceOwner(ctx, id)
	if err != nil {
		return s.cfg.MaxSpace
	}
	_, q := s.limitsFor(owner)
	return q
}

// Service owns spaces and their files. A space's files are entries in the
// file index (database) whose contents are in object storage; on this host
// it keeps only run outputs, published bundles, the working copies of
// active runs and temporary files.
type Service struct {
	cfg    Config
	db     *store.DB
	blobs  blob.Store
	log    *slog.Logger
	now    func() time.Time
	locks  sync.Map // space id -> *sync.Mutex: serialises writes within a space
	usageM sync.Mutex
	usage  map[string]usageEntry // cached usage per space
}

type usageEntry struct {
	bytes int64 // on this host: run outputs, bundles, what working copies add
}

// New creates the service and its directories.
func New(cfg Config, db *store.DB, blobs blob.Store, log *slog.Logger) (*Service, error) {
	if cfg.MaxFilesPerUpload == 0 {
		cfg.MaxFilesPerUpload = 100
	}
	s := &Service{cfg: cfg, db: db, blobs: blobs, log: log, now: time.Now, usage: map[string]usageEntry{}}
	for _, d := range []string{s.spacesDir(), s.tmpDir()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, fmt.Errorf("data dir: %w", err)
		}
	}
	return s, nil
}

// tmpDir holds files being uploaded or downloaded (never mounted into runs).
func (s *Service) tmpDir() string { return filepath.Join(s.cfg.DataDir, "tmp") }

func (s *Service) spacesDir() string { return filepath.Join(s.cfg.DataDir, "spaces") }

// Dir returns a space's directory on the host.
func (s *Service) Dir(id string) string { return filepath.Join(s.spacesDir(), id) }

// WorkPath is the working copy of the space for one run (mounted at /space
// for that run): written before it starts, read back after it ends.
func (s *Service) WorkPath(spaceID, runID string) string {
	return filepath.Join(s.Dir(spaceID), WorkDir, runID)
}

// RunPath returns the host folder holding one run's outputs (mounted at /run
// for that run only).
func (s *Service) RunPath(spaceID, runID string) string {
	return filepath.Join(s.Dir(spaceID), RunsDir, runID)
}

func (s *Service) lock(id string) func() {
	m, _ := s.locks.LoadOrStore(id, &sync.Mutex{})
	mu, _ := m.(*sync.Mutex) // only *sync.Mutex values are stored
	mu.Lock()
	return mu.Unlock
}

// Space is a space with its usage.
type Space struct {
	ID        string
	Name      string
	Owner     string // set only for administrators' views
	CreatedAt time.Time
	UpdatedAt time.Time
	Usage     Usage
}

// Usage is what a space holds.
type Usage struct {
	BytesUsed  int64 // user files, run outputs, published versions, and what active runs have added
	QuotaBytes int64
	FileCount  int64 // user files only
	RunCount   int
}

func validName(name string) error {
	switch {
	case len(name) > MaxNameBytes:
		return invalid("name is longer than %d bytes", MaxNameBytes)
	case !utf8.ValidString(name):
		return invalid("name is not valid UTF-8")
	case strings.IndexFunc(name, unicode.IsControl) >= 0:
		return invalid("name contains a control character")
	}
	return nil
}

// Create makes a new, empty space owned by owner.
func (s *Service) Create(ctx context.Context, owner, name string) (Space, error) {
	name = strings.TrimSpace(name)
	if err := validName(name); err != nil {
		return Space{}, err
	}
	id := NewID()
	dir := s.Dir(id)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return Space{}, fmt.Errorf("create space dir: %w", err)
	}
	for _, sub := range []string{RunsDir, WorkDir} {
		if err := os.Mkdir(filepath.Join(dir, sub), 0o700); err != nil {
			_ = os.RemoveAll(dir)
			return Space{}, fmt.Errorf("create %s dir: %w", sub, err)
		}
	}
	now := s.now()
	maxSpaces, quota := s.limitsFor(owner)
	if err := s.db.CreateSpaceCapped(ctx, store.Space{ID: id, Owner: owner, Name: name, CreatedAt: now, UpdatedAt: now}, maxSpaces); err != nil {
		_ = os.RemoveAll(dir)
		if errors.Is(err, store.ErrSpaceLimit) {
			return Space{}, &LimitError{Msg: fmt.Sprintf("you already have %d spaces, the most allowed; delete one first", maxSpaces)}
		}
		return Space{}, err
	}
	return Space{ID: id, Name: name, CreatedAt: now, UpdatedAt: now, Usage: Usage{QuotaBytes: quota}}, nil
}

// Get returns owner's space with its usage.
func (s *Service) Get(ctx context.Context, owner, id string) (Space, error) {
	sp, err := s.get(ctx, owner, id)
	if err != nil {
		return Space{}, err
	}
	u, err := s.Usage(ctx, id)
	if err != nil {
		return Space{}, err
	}
	out := fromStore(sp)
	out.Usage = u
	return out, nil
}

func (s *Service) get(ctx context.Context, owner, id string) (store.Space, error) {
	if !ValidID(id) {
		return store.Space{}, ErrNotFound // the API rejects malformed IDs with 400 before this
	}
	sp, err := s.db.GetSpace(ctx, owner, id)
	if errors.Is(err, store.ErrNotFound) {
		return store.Space{}, ErrNotFound
	}
	return sp, err
}

func fromStore(sp store.Space) Space {
	return Space{ID: sp.ID, Name: sp.Name, CreatedAt: sp.CreatedAt, UpdatedAt: sp.UpdatedAt}
}

// List returns one page of owner's spaces (without usage).
func (s *Service) List(ctx context.Context, owner, cursor string, limit int) ([]Space, string, error) {
	page, err := s.db.ListSpaces(ctx, owner, cursor, limit)
	if errors.Is(err, store.ErrBadCursor) {
		return nil, "", invalid("invalid cursor")
	}
	if err != nil {
		return nil, "", err
	}
	out := make([]Space, 0, len(page.Items))
	for _, sp := range page.Items {
		out = append(out, fromStore(sp))
	}
	return out, page.NextCursor, nil
}

// Rename changes the display name.
func (s *Service) Rename(ctx context.Context, owner, id, name string) (Space, error) {
	name = strings.TrimSpace(name)
	if err := validName(name); err != nil {
		return Space{}, err
	}
	if _, err := s.get(ctx, owner, id); err != nil {
		return Space{}, err
	}
	if err := s.db.RenameSpace(ctx, owner, id, name, s.now()); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return Space{}, ErrNotFound
		}
		return Space{}, err
	}
	return s.Get(ctx, owner, id)
}

// Delete removes the space with all files and runs. It
// refuses while a run is active. The database row goes first, in one
// statement with the active-run check; the directory is then moved aside
// and removed. A crash in between leaves only a directory without a row,
// which the startup consistency check removes.
func (s *Service) Delete(ctx context.Context, owner, id string) error {
	if _, err := s.get(ctx, owner, id); err != nil {
		return err
	}
	unlock := s.lock(id)
	defer unlock()
	switch err := s.db.DeleteSpace(ctx, owner, id); {
	case errors.Is(err, store.ErrNotFound):
		return ErrNotFound
	case errors.Is(err, store.ErrActiveRun):
		return ErrActiveRun
	case err != nil:
		return err
	}
	trash := filepath.Join(s.spacesDir(), ".trash-"+id)
	if err := os.Rename(s.Dir(id), trash); err != nil && !errors.Is(err, fs.ErrNotExist) {
		s.log.Error("move deleted space to trash", "space_id", id, "error", err)
		return nil // the row is gone; the consistency check finishes the job
	}
	if err := os.RemoveAll(trash); err != nil {
		s.log.Error("remove deleted space", "space_id", id, "error", err)
	}
	s.forgetUsage(id)
	return nil
}

// Usage returns a space's usage: the file index's bytes, plus what is on
// this host (run outputs, published bundles, and what active runs have added
// to their working copies). The host part is cached until the next write.
func (s *Service) Usage(ctx context.Context, id string) (Usage, error) {
	u := Usage{QuotaBytes: s.quota(ctx, id)}
	runs, err := s.db.CountRuns(ctx, id)
	if err != nil {
		return u, err
	}
	u.RunCount = runs
	fb, fc, err := s.db.SpaceFileStats(ctx, id)
	if err != nil {
		return u, err
	}
	rb, err := s.db.SpaceRunBytes(ctx, id)
	if err != nil {
		return u, err
	}
	fb += rb + s.publishedBytes(ctx, id)
	e, err := s.usageOf(id)
	if err != nil {
		return u, err
	}
	u.BytesUsed, u.FileCount = fb+e.bytes, fc
	return u, nil
}

// fileQuota is how many bytes the space's indexed files may take: the quota
// less what is on this host.
func (s *Service) fileQuota(ctx context.Context, id string) int64 {
	q := s.quota(ctx, id)
	e, err := s.usageOf(id)
	if err != nil {
		return q
	}
	return max(q-e.bytes, 1)
}

// usageOf walks the space's folder on this host without following links. A
// run's working copy counts only what it adds to the copy it started with.
func (s *Service) usageOf(id string) (usageEntry, error) {
	s.usageM.Lock()
	e, ok := s.usage[id]
	s.usageM.Unlock()
	if ok {
		return e, nil
	}
	root, err := os.OpenRoot(s.Dir(id))
	if errors.Is(err, fs.ErrNotExist) {
		return usageEntry{}, nil
	}
	if err != nil {
		return usageEntry{}, err
	}
	defer func() { _ = root.Close() }()
	err = fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // a file removed mid-walk is not an error
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if path.Dir(p) == WorkDir && strings.HasSuffix(p, ".json") {
			return nil // a working copy's record, not the space's data
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		e.bytes += info.Size()
		return nil
	})
	if err != nil {
		return usageEntry{}, err
	}
	e.bytes -= s.checkedOutBytes(id)
	s.usageM.Lock()
	s.usage[id] = e
	s.usageM.Unlock()
	return e, nil
}

// ForgetUsage drops the cached usage (call after a run writes outputs).
func (s *Service) forgetUsage(id string) {
	s.usageM.Lock()
	delete(s.usage, id)
	s.usageM.Unlock()
}

// InvalidateUsage is called by other packages (runs) after writing to a space.
func (s *Service) InvalidateUsage(id string) { s.forgetUsage(id) }

// publishedBytes is the size of the space's published function versions.
func (s *Service) publishedBytes(ctx context.Context, id string) int64 {
	n, err := s.db.SpacePublishedBytes(ctx, id)
	if err != nil {
		s.log.Warn("usage: published versions", "space_id", id, "error", err)
	}
	return n
}
