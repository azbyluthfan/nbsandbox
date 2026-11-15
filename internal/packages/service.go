// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package packages

import (
	"archive/tar"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/blob"
	"github.com/azbyluthfan/nbsandbox/internal/isolation"
	"github.com/azbyluthfan/nbsandbox/internal/store"
)

// Config configures package sets.
type Config struct {
	Enabled      bool
	IndexURL     string
	Allowed      []string
	MaxSize      int64 // a set, unpacked
	BuildTimeout time.Duration
	Retention    time.Duration
	CacheSize    int64 // the sets a worker keeps unpacked
	BuilderImage string
	Network      string
	Runtime      string           // the runs' runtime (runc or runsc)
	Limits       isolation.Limits // the build container's (the runs')
	DataDir      string           // tmp/ and packages/ under it
	WorkerID     string
	Platform     string // e.g. linux/amd64: part of the key
}

// Store is what the service keeps in the database (store.DB).
type Store interface {
	RequestPackageSet(ctx context.Context, r store.PackageRequest, retryFailed time.Duration, now time.Time) (store.PackageSet, error)
	GetPackageSet(ctx context.Context, key string) (store.PackageSet, error)
	ClaimPackageBuild(ctx context.Context, worker string, lease time.Duration, now time.Time) (store.PackageSet, error)
	RenewPackageBuild(ctx context.Context, key, worker string, until time.Time) (bool, error)
	FinishPackageBuild(ctx context.Context, key, worker string, o store.BuildOutcome, now time.Time) (bool, error)
	TouchPackageSet(ctx context.Context, key string, now time.Time) error
	UnusedPackageSets(ctx context.Context, before time.Time) ([]string, error)
	DeletePackageSet(ctx context.Context, key string) error
	SpacePackages(ctx context.Context, spaceID string) (requirements, lock string, err error)
	SetSpacePackages(ctx context.Context, spaceID, requirements, lock string) error
}

// The build lease, renewed every third of it while building; a failed set
// may be asked for again after retryFailed.
const (
	buildLease  = 2 * time.Minute
	retryFailed = 5 * time.Minute
	maxLog      = 64 << 10
)

// Service builds package sets and keeps them on this worker's disk.
type Service struct {
	cfg     Config
	db      Store
	blobs   blob.Store
	builder isolation.PackageBuilder // nil on nodes that don't build (API only)
	log     *slog.Logger
	now     func() time.Time

	// OnFinished is called when this worker finishes a build (to wake
	// workers for runs waiting on a ready set, or to fail them).
	OnFinished func(key string, ps store.PackageSet)

	kick chan struct{}

	cacheMu sync.Mutex // one unpack at a time; eviction
}

// New returns the service. builder is nil on API-only nodes.
func New(cfg Config, db Store, blobs blob.Store, builder isolation.PackageBuilder, log *slog.Logger) *Service {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Service{cfg: cfg, db: db, blobs: blobs, builder: builder, log: log, now: time.Now, kick: make(chan struct{}, 1)}
}

// Enabled reports whether spaces may have their own packages.
func (s *Service) Enabled() bool { return s != nil && s.cfg.Enabled }

// InvalidError is a requirements.txt (or lock) nbsandbox refuses.
type InvalidError struct{ Msg string }

func (e *InvalidError) Error() string { return e.Msg }

// FailedError is a set whose build failed recently: the run is refused at
// once rather than queued to fail.
type FailedError struct {
	Key, Reason, Log string
}

func (e *FailedError) Error() string {
	return "the space's packages could not be built: " + e.Reason
}

// ForSpace asks for the set a space's requirements.txt needs, built against
// the runner image imageID, and returns its key. The space's last lock is
// tried first when the requirements haven't changed (no drift).
func (s *Service) ForSpace(ctx context.Context, spaceID string, requirementsTxt []byte, imageID string) (string, error) {
	rs, err := Parse(requirementsTxt)
	if err != nil {
		return "", &InvalidError{Msg: err.Error()}
	}
	if len(rs) == 0 {
		return "", nil
	}
	if bad := Allowed(rs.Names(), s.cfg.Allowed); len(bad) > 0 {
		return "", &InvalidError{Msg: "requirements.txt asks for packages this nbsandbox doesn't allow: " + strings.Join(bad, ", ")}
	}
	canonical := rs.Canonical()
	req := store.PackageRequest{Key: Key(canonical, imageID, s.cfg.Platform), Requirements: canonical}
	if last, lock, err := s.db.SpacePackages(ctx, spaceID); err == nil && last == canonical {
		req.PreferLock = lock
	}
	return s.request(ctx, req)
}

// KeyForSpace is the key of the set a space's requirements.txt needs,
// without asking for it ("" when it has no requirements).
func (s *Service) KeyForSpace(requirementsTxt []byte, imageID string) (string, error) {
	rs, err := Parse(requirementsTxt)
	if err != nil {
		return "", &InvalidError{Msg: err.Error()}
	}
	if len(rs) == 0 {
		return "", nil
	}
	return Key(rs.Canonical(), imageID, s.cfg.Platform), nil
}

// ForLock asks for the set of a published version's lock (exactly those
// packages), built against imageID.
func (s *Service) ForLock(ctx context.Context, lock, imageID string) (string, error) {
	if strings.TrimSpace(lock) == "" {
		return "", nil
	}
	rs, err := Parse([]byte(lock))
	if err != nil {
		return "", &InvalidError{Msg: "the published version's package lock: " + err.Error()}
	}
	canonical := rs.Canonical()
	return s.request(ctx, store.PackageRequest{Key: Key("lock\n"+canonical, imageID, s.cfg.Platform), Requirements: canonical, FromLock: true})
}

func (s *Service) request(ctx context.Context, req store.PackageRequest) (string, error) {
	ps, err := s.db.RequestPackageSet(ctx, req, retryFailed, s.now())
	if err != nil {
		return "", err
	}
	if ps.Status == store.PackagesFailed {
		return ps.Key, &FailedError{Key: ps.Key, Reason: ps.Error, Log: ps.Log}
	}
	s.Kick()
	return ps.Key, nil
}

// Status returns a set.
func (s *Service) Status(ctx context.Context, key string) (store.PackageSet, error) {
	return s.db.GetPackageSet(ctx, key)
}

// RecordSpaceLock remembers a space's built requirements and lock, so a
// rebuild of the same requirements installs the same versions.
func (s *Service) RecordSpaceLock(ctx context.Context, spaceID string, ps store.PackageSet) {
	if ps.Status != store.PackagesReady || ps.FromLock {
		return
	}
	if err := s.db.SetSpacePackages(ctx, spaceID, ps.Requirements, ps.Lock); err != nil {
		s.log.Warn("record the space's package lock", "space_id", spaceID, "error", err)
	}
}

// Kick wakes the build loop (a set was asked for).
func (s *Service) Kick() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// Run builds pending sets, one at a time, until ctx ends; it also removes
// sets nobody used for the retention.
func (s *Service) Run(ctx context.Context) {
	if s.builder == nil || !s.cfg.Enabled {
		return
	}
	if err := s.builder.RemoveBuildOrphans(ctx, s.cfg.WorkerID); err != nil {
		s.log.Warn("remove leftover build containers", "error", err)
	}
	poll := time.NewTicker(5 * time.Second)
	defer poll.Stop()
	sweep := time.NewTicker(time.Hour)
	defer sweep.Stop()
	s.sweep(ctx)
	for {
		for s.buildOne(ctx) {
		}
		select {
		case <-ctx.Done():
			return
		case <-s.kick:
		case <-poll.C:
		case <-sweep.C:
			s.sweep(ctx)
		}
	}
}

// buildOne claims and builds one pending set; false if there was none.
func (s *Service) buildOne(ctx context.Context) bool {
	ps, err := s.db.ClaimPackageBuild(ctx, s.cfg.WorkerID, buildLease, s.now())
	if errors.Is(err, store.ErrNotFound) {
		return false
	}
	if err != nil {
		if ctx.Err() == nil {
			s.log.Warn("claim a package build", "error", err)
		}
		return false
	}
	s.log.Info("building a package set", "key", ps.Key, "from_lock", ps.FromLock)
	started := s.now()
	o := s.build(ctx, ps)
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if held, err := s.db.FinishPackageBuild(fctx, ps.Key, s.cfg.WorkerID, o, s.now()); err != nil || !held {
		s.log.Warn("record a package build", "key", ps.Key, "held", held, "error", err)
		return true
	}
	s.log.Info("package set built", "key", ps.Key, "ok", o.OK, "bytes", o.Size, "seconds", s.now().Sub(started).Seconds(), "reason", o.Reason)
	if s.OnFinished != nil {
		if done, err := s.db.GetPackageSet(fctx, ps.Key); err == nil {
			s.OnFinished(ps.Key, done)
		}
	}
	return true
}

var errBuildTimeout = errors.New("the build timed out")

// request.json for build_packages.py.
type buildRequest struct {
	IndexURL     string `json:"index_url"`
	Requirements string `json:"requirements"`
	FromLock     bool   `json:"from_lock"`
	PreferLock   string `json:"prefer_lock"`
}

// result.json from build_packages.py.
type buildResult struct {
	OK      bool     `json:"ok"`
	Error   string   `json:"error"`
	Lock    string   `json:"lock"`
	Used    string   `json:"used"`
	Modules []string `json:"modules"`
}

// build runs the build container and stores the set.
func (s *Service) build(ctx context.Context, ps store.PackageSet) store.BuildOutcome {
	fail := func(log, reason string) store.BuildOutcome { return store.BuildOutcome{Log: log, Reason: reason} }
	logs := &tailLog{max: maxLog}
	dir, err := os.MkdirTemp(filepath.Join(s.cfg.DataDir, "tmp"), "build-")
	if err != nil {
		return fail("", "nbsandboxd couldn't prepare the build: "+err.Error())
	}
	defer func() { _ = os.RemoveAll(dir) }()
	in, out := filepath.Join(dir, "in"), filepath.Join(dir, "out")
	for _, d := range []string{in, out} {
		if err := os.Mkdir(d, 0o755); err != nil { //nolint:gosec // read by the build container's uid 1000, maybe not nbsandboxd's
			return fail("", "nbsandboxd couldn't prepare the build: "+err.Error())
		}
	}
	// The build container runs as uid 1000, perhaps not nbsandboxd's.
	if err := os.Chmod(out, 0o777); err != nil { //nolint:gosec // a private temporary folder under data_dir/tmp
		return fail("", "nbsandboxd couldn't prepare the build: "+err.Error())
	}
	b, _ := json.Marshal(buildRequest{IndexURL: s.cfg.IndexURL, Requirements: ps.Requirements, FromLock: ps.FromLock, PreferLock: ps.PreferLock})
	if err := os.WriteFile(filepath.Join(in, "request.json"), b, 0o644); err != nil { //nolint:gosec // read by the build container
		return fail("", "nbsandboxd couldn't prepare the build: "+err.Error())
	}

	// Keep the build's lease while it runs; give up if it was lost.
	bctx, cancel := context.WithTimeoutCause(ctx, s.cfg.BuildTimeout, errBuildTimeout)
	defer cancel()
	go func() {
		t := time.NewTicker(buildLease / 3)
		defer t.Stop()
		for {
			select {
			case <-bctx.Done():
				return
			case <-t.C:
				if held, err := s.db.RenewPackageBuild(bctx, ps.Key, s.cfg.WorkerID, s.now().Add(buildLease)); err == nil && !held {
					cancel()
					return
				}
			}
		}
	}()
	id := ps.Key[:16] + "-" + randomHex(4)
	res, err := s.builder.BuildPackages(bctx, isolation.BuildSpec{ID: id, Worker: s.cfg.WorkerID, InDir: in, OutDir: out,
		Image: s.cfg.BuilderImage, Runtime: s.cfg.Runtime, Network: s.cfg.Network, Limits: s.cfg.Limits}, logs)
	logText := logs.String()
	switch {
	case errors.Is(context.Cause(bctx), errBuildTimeout):
		return fail(logText, fmt.Sprintf("the build took longer than %s", s.cfg.BuildTimeout))
	case err != nil:
		return fail(logText, "the build couldn't run: "+err.Error())
	case res.Stopped:
		return fail(logText, "the build was stopped")
	case res.OOMKilled:
		return fail(logText, "the build ran out of memory")
	}
	var r buildResult
	rb, err := os.ReadFile(filepath.Join(out, "result.json")) //nolint:gosec // our own temporary folder
	if err != nil || json.Unmarshal(rb, &r) != nil {
		return fail(logText, fmt.Sprintf("the build ended without a result (exit code %d)", res.ExitCode))
	}
	if !r.OK {
		return fail(logText, r.Error)
	}
	locked, err := Parse([]byte(r.Lock))
	if err != nil {
		return fail(logText, "the build's lock can't be read: "+err.Error())
	}
	if bad := Allowed(locked.Names(), s.cfg.Allowed); len(bad) > 0 {
		return fail(logText, "the requirements pull in packages this nbsandbox doesn't allow: "+strings.Join(bad, ", "))
	}
	site := filepath.Join(out, "site-packages")
	size, err := treeSize(site)
	if err != nil {
		return fail(logText, "the build's output can't be read: "+err.Error())
	}
	if size > s.cfg.MaxSize {
		return fail(logText, fmt.Sprintf("the packages take %d MiB, more than the %d MiB allowed", size>>20, s.cfg.MaxSize>>20))
	}
	if err := s.store(ctx, ps.Key, site, filepath.Join(dir, "set.tar")); err != nil {
		return fail(logText, "nbsandboxd couldn't store the packages: "+err.Error())
	}
	// Built here: keep it unpacked for this worker's runs.
	s.cacheMu.Lock()
	if err := os.Rename(site, s.cacheDir(ps.Key)); err != nil && !errors.Is(err, fs.ErrExist) {
		s.log.Debug("keep the built set", "key", ps.Key, "error", err)
	}
	s.cacheMu.Unlock()
	return store.BuildOutcome{OK: true, Lock: r.Lock, Modules: r.Modules, Size: size, Log: logText}
}

// store tars the set and puts it in object storage.
func (s *Service) store(ctx context.Context, key, site, tmp string) error {
	f, err := os.Create(tmp) //nolint:gosec // under data_dir/tmp
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if err := writeTar(f, site); err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return s.blobs.Put(ctx, blobKey(key), f, st.Size())
}

func blobKey(key string) string { return "packages/" + key + ".tar" }

func (s *Service) cacheRoot() string        { return filepath.Join(s.cfg.DataDir, "packages") }
func (s *Service) cacheDir(k string) string { return filepath.Join(s.cacheRoot(), k) }

// Ensure returns the folder of a ready set on this worker's disk,
// downloading and unpacking it the first time, and records the use.
func (s *Service) Ensure(ctx context.Context, key string) (string, error) {
	dir := s.cacheDir(key)
	_ = s.db.TouchPackageSet(ctx, key, s.now())
	if _, err := os.Stat(dir); err == nil {
		now := s.now()
		_ = os.Chtimes(dir, now, now) // most recently used: evicted last
		return dir, nil
	}
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if _, err := os.Stat(dir); err == nil {
		return dir, nil
	}
	if err := os.MkdirAll(s.cacheRoot(), 0o755); err != nil { //nolint:gosec // mounted into runs (uid 1000, maybe not nbsandboxd's)
		return "", err
	}
	rc, err := s.blobs.Get(ctx, blobKey(key))
	if err != nil {
		return "", fmt.Errorf("the space's packages are missing from storage: %w", err)
	}
	defer func() { _ = rc.Close() }()
	tmp, err := os.MkdirTemp(s.cacheRoot(), ".unpack-")
	if err != nil {
		return "", err
	}
	if err := readTar(rc, tmp, s.cfg.MaxSize); err != nil {
		_ = os.RemoveAll(tmp)
		return "", fmt.Errorf("unpack the space's packages: %w", err)
	}
	if err := os.Rename(tmp, dir); err != nil {
		_ = os.RemoveAll(tmp)
		return "", err
	}
	s.evict(key)
	return dir, nil
}

// evict removes the least recently used sets until the cache fits
// cache_size (never keep, the one just unpacked).
func (s *Service) evict(keep string) {
	entries, err := os.ReadDir(s.cacheRoot())
	if err != nil {
		return
	}
	type set struct {
		key  string
		used time.Time
		size int64
	}
	var sets []set
	var total int64
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		size, _ := treeSize(filepath.Join(s.cacheRoot(), e.Name()))
		sets = append(sets, set{e.Name(), info.ModTime(), size})
		total += size
	}
	sort.Slice(sets, func(i, j int) bool { return sets[i].used.Before(sets[j].used) })
	for _, st := range sets {
		if total <= s.cfg.CacheSize {
			return
		}
		if st.key == keep {
			continue
		}
		// A run using it keeps its open files; new runs unpack it again.
		if err := os.RemoveAll(filepath.Join(s.cacheRoot(), st.key)); err == nil {
			total -= st.size
		}
	}
}

// sweep removes sets nobody used for the retention: their row, their
// object, and this worker's copy.
func (s *Service) sweep(ctx context.Context) {
	keys, err := s.db.UnusedPackageSets(ctx, s.now().Add(-s.cfg.Retention))
	if err != nil {
		return
	}
	for _, k := range keys {
		if err := s.blobs.Delete(ctx, blobKey(k)); err != nil {
			continue
		}
		_ = s.db.DeletePackageSet(ctx, k)
		_ = os.RemoveAll(s.cacheDir(k))
		s.log.Info("removed an unused package set", "key", k)
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// tailLog keeps the last max bytes of a build's output.
type tailLog struct {
	mu  sync.Mutex
	max int
	buf []byte
	cut bool
}

func (t *tailLog) Line(s string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, s...)
	t.buf = append(t.buf, '\n')
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
		t.cut = true
	}
}

func (t *tailLog) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cut {
		return "[earlier output cut]\n" + string(t.buf)
	}
	return string(t.buf)
}

func treeSize(dir string) (int64, error) {
	var n int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			n += info.Size()
		}
		return nil
	})
	return n, err
}

// writeTar writes dir's regular files, folders and symlinks (relative ones,
// inside dir) as a tar.
func writeTar(w io.Writer, dir string) error {
	tw := tar.NewWriter(w)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil || rel == "." {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		link := ""
		if d.Type()&fs.ModeSymlink != 0 {
			if link, err = os.Readlink(p); err != nil {
				return err
			}
		}
		h, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		h.Name, h.Uid, h.Gid, h.Uname, h.Gname = filepath.ToSlash(rel), 0, 0, "", ""
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		f, err := os.Open(p) //nolint:gosec // inside the build's output
		if err != nil {
			return err
		}
		_, err = io.Copy(tw, f)
		_ = f.Close()
		return err
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

// readTar unpacks a set into dir: regular files and folders only, no paths
// leaving dir, at most max bytes in all. Files are read-only.
func readTar(r io.Reader, dir string, maxBytes int64) error {
	tr := tar.NewReader(r)
	var total int64
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(filepath.FromSlash(h.Name))
		if !filepath.IsLocal(name) {
			return fmt.Errorf("unsafe path %q", h.Name)
		}
		p := filepath.Join(dir, name)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(p, 0o755); err != nil { //nolint:gosec // mounted into runs (uid 1000, maybe not nbsandboxd's)
				return err
			}
		case tar.TypeReg:
			total += h.Size
			if total > maxBytes {
				return fmt.Errorf("larger than %d bytes", maxBytes)
			}
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil { //nolint:gosec // as above
				return err
			}
			f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o444) //nolint:gosec // inside the cache folder
			if err != nil {
				return err
			}
			_, err = io.Copy(f, io.LimitReader(tr, h.Size))
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		default:
			// Symlinks and anything else are skipped: a package set needs
			// none, and they could point outside.
		}
	}
}
