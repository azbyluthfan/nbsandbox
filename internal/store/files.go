// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// FileRow is one entry of a space's file index. Paths are relative and
// "/"-separated; the spaces package validates them before they get here.
type FileRow struct {
	Path       string
	Kind       string // file | folder
	SHA256     string // contents, for files
	Size       int64
	ModifiedAt time.Time
}

// Errors from file index changes.
var (
	ErrExists   = errors.New("store: a file or folder already exists at that path")
	ErrNotEmpty = errors.New("store: the folder is not empty")
	ErrNotDir   = errors.New("store: a parent of that path is a file")
	ErrIsDir    = errors.New("store: that path is a folder")
	ErrQuota    = errors.New("store: the space's storage quota would be exceeded")
)

// VersionMismatch: the file's contents aren't the ones the caller expected.
// Current is "" when the file doesn't exist (any more).
type VersionMismatch struct {
	Path, Current string
}

func (e *VersionMismatch) Error() string {
	return fmt.Sprintf("store: %s changed (current contents %q)", e.Path, e.Current)
}

// FileWrite is one file to store. If is the expected current SHA-256: nil =
// whatever is there, "" = the file must not exist yet.
type FileWrite struct {
	Path   string
	SHA256 string
	Size   int64
	If     *string
}

func (db *DB) scanFiles(rows *sql.Rows) ([]FileRow, error) {
	defer func() { _ = rows.Close() }()
	var out []FileRow
	for rows.Next() {
		var f FileRow
		var at int64
		if err := rows.Scan(&f.Path, &f.Kind, &f.SHA256, &f.Size, &at); err != nil {
			return nil, err
		}
		f.ModifiedAt = time.UnixMilli(at)
		out = append(out, f)
	}
	return out, rows.Err()
}

const fileCols = "path, kind, sha256, size, modified_at"

// ListFiles returns a space's files and folders, by path.
func (db *DB) ListFiles(ctx context.Context, spaceID string) ([]FileRow, error) {
	rows, err := db.r.QueryContext(ctx, "SELECT "+fileCols+" FROM files WHERE space_id = ? ORDER BY path", spaceID)
	if err != nil {
		return nil, err
	}
	return db.scanFiles(rows)
}

// GetFile returns one entry; ErrNotFound if there is none.
func (db *DB) GetFile(ctx context.Context, spaceID, path string) (FileRow, error) {
	rows, err := db.r.QueryContext(ctx, "SELECT "+fileCols+" FROM files WHERE space_id = ? AND path = ?", spaceID, path)
	if err != nil {
		return FileRow{}, err
	}
	fs, err := db.scanFiles(rows)
	if err != nil {
		return FileRow{}, err
	}
	if len(fs) == 0 {
		return FileRow{}, ErrNotFound
	}
	return fs[0], nil
}

// SpaceFileStats returns the total size and number of a space's files.
func (db *DB) SpaceFileStats(ctx context.Context, spaceID string) (bytes, count int64, err error) {
	err = db.r.QueryRowContext(ctx, "SELECT COALESCE(SUM(size), 0), COUNT(*) FILTER (WHERE kind = 'file') FROM files WHERE space_id = ?",
		spaceID).Scan(&bytes, &count)
	return bytes, count, err
}

// SpaceFileBytes is the total size of a space's files.
func (db *DB) SpaceFileBytes(ctx context.Context, spaceID string) (int64, error) {
	var n int64
	err := db.r.QueryRowContext(ctx, "SELECT COALESCE(SUM(size), 0) FROM files WHERE space_id = ?", spaceID).Scan(&n)
	return n, err
}

// lockSpace serialises changes to one space's files across every nbsandboxd
// (a row lock held until the transaction ends). ErrNotFound if the space is
// gone.
func lockSpace(ctx context.Context, tx txn, spaceID string) error {
	var id string
	err := tx.QueryRowContext(ctx, "SELECT id FROM spaces WHERE id = ? FOR UPDATE", spaceID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func getFileTx(ctx context.Context, tx txn, spaceID, path string) (FileRow, bool, error) {
	var f FileRow
	var at int64
	err := tx.QueryRowContext(ctx, "SELECT "+fileCols+" FROM files WHERE space_id = ? AND path = ?", spaceID, path).
		Scan(&f.Path, &f.Kind, &f.SHA256, &f.Size, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return FileRow{}, false, nil
	}
	f.ModifiedAt = time.UnixMilli(at)
	return f, err == nil, err
}

// ensureParents creates the folders above path; ErrNotDir if one is a file.
func ensureParents(ctx context.Context, tx txn, spaceID, path string, at int64) error {
	parts := strings.Split(path, "/")
	for i := 1; i < len(parts); i++ {
		dir := strings.Join(parts[:i], "/")
		f, ok, err := getFileTx(ctx, tx, spaceID, dir)
		if err != nil {
			return err
		}
		if ok {
			if f.Kind != "folder" {
				return ErrNotDir
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO files(space_id, path, kind, modified_at) VALUES (?, ?, 'folder', ?)",
			spaceID, dir, at); err != nil {
			return err
		}
	}
	return nil
}

func checkQuota(ctx context.Context, tx txn, spaceID string, quota int64) error {
	if quota <= 0 {
		return nil
	}
	var n int64
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(SUM(size), 0) FROM files WHERE space_id = ?", spaceID).Scan(&n); err != nil {
		return err
	}
	if n > quota {
		return ErrQuota
	}
	return nil
}

// PutFiles records several files at once (an upload, a save, a run's
// changes): all or none. Parent folders are created. A file's If is checked
// against its current contents (VersionMismatch). quota (> 0) bounds the
// space's total file bytes afterwards (ErrQuota).
func (db *DB) PutFiles(ctx context.Context, spaceID string, files []FileWrite, at time.Time, quota int64) error {
	tx, err := db.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockSpace(ctx, tx, spaceID); err != nil {
		return err
	}
	ms := at.UnixMilli()
	for _, w := range files {
		cur, exists, err := getFileTx(ctx, tx, spaceID, w.Path)
		if err != nil {
			return err
		}
		if exists && cur.Kind == "folder" {
			return ErrIsDir
		}
		if w.If != nil {
			have := ""
			if exists {
				have = cur.SHA256
			}
			if have != *w.If {
				return &VersionMismatch{Path: w.Path, Current: have}
			}
		}
		if err := ensureParents(ctx, tx, spaceID, w.Path, ms); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO files(space_id, path, kind, sha256, size, modified_at) VALUES (?, ?, 'file', ?, ?, ?)
			ON CONFLICT (space_id, path) DO UPDATE SET sha256 = EXCLUDED.sha256, size = EXCLUDED.size, modified_at = EXCLUDED.modified_at`,
			spaceID, w.Path, w.SHA256, w.Size, ms); err != nil {
			return err
		}
	}
	if err := checkQuota(ctx, tx, spaceID, quota); err != nil {
		return err
	}
	return tx.Commit()
}

// CreateFolder records a folder (and its parents). ErrExists if anything is
// at that path.
func (db *DB) CreateFolder(ctx context.Context, spaceID, path string, at time.Time) error {
	tx, err := db.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockSpace(ctx, tx, spaceID); err != nil {
		return err
	}
	if _, exists, err := getFileTx(ctx, tx, spaceID, path); err != nil {
		return err
	} else if exists {
		return ErrExists
	}
	ms := at.UnixMilli()
	if err := ensureParents(ctx, tx, spaceID, path, ms); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO files(space_id, path, kind, modified_at) VALUES (?, ?, 'folder', ?)", spaceID, path, ms); err != nil {
		return err
	}
	return tx.Commit()
}

// likePrefix escapes a folder path for "LIKE prefix/%".
func likePrefix(dir string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(dir) + "/%"
}

// MovePath renames a file, or a folder with everything in it, in one
// transaction. ErrNotFound if from is missing, ErrExists if to is taken.
// ifSHA (non-nil, files only) must match from's current contents.
func (db *DB) MovePath(ctx context.Context, spaceID, from, to string, ifSHA *string, at time.Time) (FileRow, error) {
	tx, err := db.w.BeginTx(ctx, nil)
	if err != nil {
		return FileRow{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockSpace(ctx, tx, spaceID); err != nil {
		return FileRow{}, err
	}
	src, ok, err := getFileTx(ctx, tx, spaceID, from)
	if err != nil {
		return FileRow{}, err
	}
	if !ok {
		return FileRow{}, ErrNotFound
	}
	if ifSHA != nil && src.SHA256 != *ifSHA {
		return FileRow{}, &VersionMismatch{Path: from, Current: src.SHA256}
	}
	if _, taken, err := getFileTx(ctx, tx, spaceID, to); err != nil {
		return FileRow{}, err
	} else if taken {
		return FileRow{}, ErrExists
	}
	if src.Kind == "folder" && strings.HasPrefix(to+"/", from+"/") {
		return FileRow{}, fmt.Errorf("store: can't move a folder into itself")
	}
	ms := at.UnixMilli()
	if err := ensureParents(ctx, tx, spaceID, to, ms); err != nil {
		return FileRow{}, err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE files SET path = ?, modified_at = ? WHERE space_id = ? AND path = ?", to, ms, spaceID, from); err != nil {
		return FileRow{}, err
	}
	if src.Kind == "folder" {
		// Everything inside: replace the leading "from/" with "to/".
		if _, err := tx.ExecContext(ctx, `
			UPDATE files SET path = ? || substr(path, ?) WHERE space_id = ? AND path LIKE ? ESCAPE '\'`,
			to, len(from)+1, spaceID, likePrefix(from)); err != nil {
			return FileRow{}, err
		}
	}
	src.Path, src.ModifiedAt = to, time.UnixMilli(ms)
	return src, tx.Commit()
}

// DeletePath removes a file, or a folder (with recursive, everything in it;
// without, only when empty: ErrNotEmpty). ErrNotFound if nothing is there.
func (db *DB) DeletePath(ctx context.Context, spaceID, path string, recursive bool) error {
	tx, err := db.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockSpace(ctx, tx, spaceID); err != nil {
		return err
	}
	f, ok, err := getFileTx(ctx, tx, spaceID, path)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	if f.Kind == "folder" {
		var inside int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM files WHERE space_id = ? AND path LIKE ? ESCAPE '\\'",
			spaceID, likePrefix(path)).Scan(&inside); err != nil {
			return err
		}
		if inside > 0 && !recursive {
			return ErrNotEmpty
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM files WHERE space_id = ? AND path LIKE ? ESCAPE '\\'", spaceID, likePrefix(path)); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM files WHERE space_id = ? AND path = ?", spaceID, path); err != nil {
		return err
	}
	return tx.Commit()
}

// ReferencedContent calls fn with every SHA-256 still in use: by a space's
// files or a published function version. Contents in object storage that
// aren't listed (and are older than a grace period) can be removed.
func (db *DB) ReferencedContent(ctx context.Context, fn func(sha string) error) error {
	rows, err := db.r.QueryContext(ctx, `
		SELECT sha256 FROM files WHERE kind = 'file'
		UNION SELECT sha256 FROM function_version_files
		UNION SELECT sha256 FROM run_files`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return err
		}
		if err := fn(s); err != nil {
			return err
		}
	}
	return rows.Err()
}

// FileDelete removes a file only if its contents are still If (a folder,
// with Folder set, only if it's empty).
type FileDelete struct {
	Path   string
	If     string
	Folder bool
}

// Conflict is a run's change that met a newer edit: the edit wins, and the
// run's version (for a write) is kept at SavedAs.
type Conflict struct {
	Path    string
	SavedAs string // "" for a delete that was skipped
}

// ConflictPath is where a run's version of path goes when path was edited
// while the run worked: before the extension, so it still opens as the same
// kind of file ("report.conflict-1a2b3c4d.csv").
func ConflictPath(path, tag string) string {
	dir, base := "", path
	if i := strings.LastIndex(path, "/"); i >= 0 {
		dir, base = path[:i+1], path[i+1:]
	}
	if i := strings.LastIndex(base, "."); i > 0 {
		return dir + base[:i] + ".conflict-" + tag + base[i:]
	}
	return dir + base + ".conflict-" + tag
}

// ApplyRunChanges writes a run's changes back to the space in one
// transaction. Each write's If is the contents the run started from ("" for
// a file it created); if the file has changed since, the run's version is
// stored at ConflictPath(path, tag) instead. Deletes whose file changed are
// skipped. folders are empty folders the run created. quota (> 0) bounds the
// space's file bytes afterwards (ErrQuota: nothing is applied).
func (db *DB) ApplyRunChanges(ctx context.Context, spaceID string, writes []FileWrite, deletes []FileDelete, folders []string,
	tag string, at time.Time, quota int64, lease Lease) ([]Conflict, error) {
	tx, err := db.w.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if lease.RunID != "" {
		if err := fence(ctx, tx, lease.RunID, lease.Token); err != nil {
			return nil, err
		}
	}
	if err := lockSpace(ctx, tx, spaceID); err != nil {
		return nil, err
	}
	ms := at.UnixMilli()
	var conflicts []Conflict
	upsert := func(path, sha string, size int64) error {
		if err := ensureParents(ctx, tx, spaceID, path, ms); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO files(space_id, path, kind, sha256, size, modified_at) VALUES (?, ?, 'file', ?, ?, ?)
			ON CONFLICT (space_id, path) DO UPDATE SET sha256 = EXCLUDED.sha256, size = EXCLUDED.size, modified_at = EXCLUDED.modified_at`,
			spaceID, path, sha, size, ms)
		return err
	}
	for _, w := range writes {
		cur, exists, err := getFileTx(ctx, tx, spaceID, w.Path)
		if err != nil {
			return nil, err
		}
		have := ""
		if exists {
			have = cur.SHA256
		}
		clean := w.If == nil || have == *w.If
		if exists && cur.Kind == "folder" {
			clean = false
		}
		if err := func() error {
			if clean {
				return upsert(w.Path, w.SHA256, w.Size)
			}
			if have == w.SHA256 {
				return nil // edited to the same contents: nothing to keep
			}
			saved := ConflictPath(w.Path, tag)
			conflicts = append(conflicts, Conflict{Path: w.Path, SavedAs: saved})
			return upsert(saved, w.SHA256, w.Size)
		}(); err != nil {
			if errors.Is(err, ErrNotDir) {
				// A parent became a file meanwhile: keep the run's version at the top level.
				saved := ConflictPath(w.Path[strings.LastIndex(w.Path, "/")+1:], tag)
				conflicts = append(conflicts, Conflict{Path: w.Path, SavedAs: saved})
				if err := upsert(saved, w.SHA256, w.Size); err != nil {
					return nil, err
				}
				continue
			}
			return nil, err
		}
	}
	for _, f := range folders {
		if _, exists, err := getFileTx(ctx, tx, spaceID, f); err != nil {
			return nil, err
		} else if exists {
			continue
		}
		if err := ensureParents(ctx, tx, spaceID, f, ms); err != nil {
			if errors.Is(err, ErrNotDir) {
				continue
			}
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO files(space_id, path, kind, modified_at) VALUES (?, ?, 'folder', ?)", spaceID, f, ms); err != nil {
			return nil, err
		}
	}
	// Files before folders, deepest folders first, so emptied folders can go.
	files, dirs := []FileDelete{}, []FileDelete{}
	for _, d := range deletes {
		if d.Folder {
			dirs = append(dirs, d)
		} else {
			files = append(files, d)
		}
	}
	for _, d := range files {
		res, err := tx.ExecContext(ctx, "DELETE FROM files WHERE space_id = ? AND path = ? AND kind = 'file' AND sha256 = ?", spaceID, d.Path, d.If)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			if _, exists, _ := getFileTx(ctx, tx, spaceID, d.Path); exists {
				conflicts = append(conflicts, Conflict{Path: d.Path})
			}
		}
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		d := dirs[i]
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM files WHERE space_id = ? AND path = ? AND kind = 'folder'
			AND NOT EXISTS (SELECT 1 FROM files f WHERE f.space_id = ? AND f.path LIKE ? ESCAPE '\')`,
			spaceID, d.Path, spaceID, likePrefix(d.Path)); err != nil {
			return nil, err
		}
	}
	if err := checkQuota(ctx, tx, spaceID, quota); err != nil {
		return nil, err
	}
	return conflicts, tx.Commit()
}

// ContentRef is a path with its contents (a run's file, or a file of a
// published function version).
type ContentRef struct {
	Path   string
	SHA256 string
	Size   int64
}

// PutRunFiles records files of a run (replacing any with the same path).
//
// With a token, only while the run's worker holds that lease (ErrFenced
// otherwise); 0 records them unconditionally.
func (db *DB) PutRunFiles(ctx context.Context, runID string, token int64, files []ContentRef) error {
	tx, err := db.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if token != 0 {
		if err := fence(ctx, tx, runID, token); err != nil {
			return err
		}
	}
	for _, f := range files {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO run_files(run_id, path, sha256, size) VALUES (?, ?, ?, ?)
			ON CONFLICT (run_id, path) DO UPDATE SET sha256 = EXCLUDED.sha256, size = EXCLUDED.size`,
			runID, f.Path, f.SHA256, f.Size); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// RunFiles returns a run's recorded files, by path.
func (db *DB) RunFiles(ctx context.Context, runID string) ([]ContentRef, error) {
	return db.refs(ctx, "SELECT path, sha256, size FROM run_files WHERE run_id = ? ORDER BY path", runID)
}

// RunFile returns one of a run's files; ErrNotFound if there is none.
func (db *DB) RunFile(ctx context.Context, runID, path string) (ContentRef, error) {
	refs, err := db.refs(ctx, "SELECT path, sha256, size FROM run_files WHERE run_id = ? AND path = ?", runID, path)
	if err != nil {
		return ContentRef{}, err
	}
	if len(refs) == 0 {
		return ContentRef{}, ErrNotFound
	}
	return refs[0], nil
}

// SpaceRunBytes is the total size of the recorded files of a space's runs.
func (db *DB) SpaceRunBytes(ctx context.Context, spaceID string) (int64, error) {
	var n int64
	err := db.r.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(f.size), 0) FROM run_files f JOIN runs r ON r.id = f.run_id WHERE r.space_id = ?`, spaceID).Scan(&n)
	return n, err
}

// SpacePublishedBytes is the total size of a space's published versions.
func (db *DB) SpacePublishedBytes(ctx context.Context, spaceID string) (int64, error) {
	var n int64
	err := db.r.QueryRowContext(ctx, "SELECT COALESCE(SUM(size), 0) FROM function_version_files WHERE space_id = ?", spaceID).Scan(&n)
	return n, err
}

// FunctionVersionFiles returns the files of a published version, by path.
func (db *DB) FunctionVersionFiles(ctx context.Context, spaceID, function string, version int) ([]ContentRef, error) {
	return db.refs(ctx, `SELECT path, sha256, size FROM function_version_files
		WHERE space_id = ? AND function = ? AND version = ? ORDER BY path`, spaceID, function, version)
}

func (db *DB) refs(ctx context.Context, q string, args ...any) ([]ContentRef, error) {
	rows, err := db.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ContentRef
	for rows.Next() {
		var f ContentRef
		if err := rows.Scan(&f.Path, &f.SHA256, &f.Size); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}
