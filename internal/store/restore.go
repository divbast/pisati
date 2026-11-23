// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ncruces/go-sqlite3"
)

// ErrCorrupt is a database that PRAGMA integrity_check finds fault with.
var ErrCorrupt = errors.New("store: database failed its integrity check")

// restoreSuffix names the copy Restore builds beside the database it is
// to replace.
const restoreSuffix = ".restore"

// Restore replaces the database at path with a copy of the database at
// src. It first checks the copy, and refuses it, changing nothing at path,
// unless PRAGMA integrity_check finds nothing wrong with it (ErrCorrupt)
// and its schema is one this Pisati knows: at least version 1, and no
// newer than Pisati's newest migration (ErrSchemaNewer).
//
// lock must hold the lock on path's directory, the state directory, as
// proof that no Pisati serves from it; otherwise Restore returns
// ErrLockNotHeld. The directory is compared as written, after
// filepath.Clean: the same path AcquireLock was given, not another that
// reaches it through a symbolic link. lock cannot be released until Restore returns. No Store
// may have path open meanwhile.
//
// The copy is built beside path, flushed to stable storage and renamed
// over path, so that path holds the old database or the whole of the new
// one, never part of either. The old database's write-ahead log, its
// index and any rollback journal are removed first: SQLite would read
// them as the new database's own. The restored file has mode 0600. src
// and path must be absolute; Restore stops, failing, when ctx ends.
func Restore(ctx context.Context, src, path string, lock Lock) (err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("store: restore %s from %s: %w", path, src, err)
		}
	}()
	if err := absolute(src); err != nil {
		return err
	}
	dir, name, err := splitPath(path)
	if err != nil {
		return err
	}
	unhold, err := lock.hold(dir)
	if err != nil {
		return err
	}
	defer unhold()
	if err := ctx.Err(); err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, root.Close())
	}()

	tmp := name + restoreSuffix
	copied := append(sidecars(tmp), tmp)
	// What a restore cut short may have left. The lock keeps any other
	// restore from building a copy here meanwhile.
	if err := removeFiles(root, copied...); err != nil {
		return err
	}
	if err := createFile(root, tmp); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, removeFiles(root, copied...))
		}
	}()
	tmpPath := filepath.Join(dir, tmp)
	if err := copyFrom(ctx, tmpPath, src); err != nil {
		return err
	}
	if err := check(ctx, tmpPath); err != nil {
		return err
	}
	// Closing the copy folded its write-ahead log into the file and
	// removed the log. Were one left, the rename would leave it behind,
	// and what it holds with it.
	if _, err := root.Stat(tmp + "-wal"); !errors.Is(err, fs.ErrNotExist) {
		return errors.Join(errors.New("the copy kept a write-ahead log"), err)
	}
	if err := syncFile(root, tmp); err != nil {
		return err
	}
	if err := removeFiles(root, sidecars(name)...); err != nil {
		return err
	}
	if err := root.Rename(tmp, name); err != nil {
		return err
	}
	return syncDir(root)
}

// copyFrom copies the database at src into the empty database file at
// dst, through SQLite's backup API. SQLite reads src as it reads any
// database, with the write-ahead log beside it if there is one, so the
// copy holds every transaction committed to src.
func copyFrom(ctx context.Context, dst, src string) (err error) {
	c, err := sqlite3.OpenContext(ctx, dst)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, c.Close())
	}()
	if err := c.BusyTimeout(busyTimeoutMillis * time.Millisecond); err != nil {
		return err
	}
	// The driver stops a call that is running through SQLite's interrupt,
	// which follows the context set here.
	old := c.SetInterrupt(ctx)
	defer c.SetInterrupt(old)
	return c.Restore("main", src)
}

// check opens the database at path, and returns an error unless PRAGMA
// integrity_check finds nothing wrong with it and its schema is one this
// Pisati knows.
func check(ctx context.Context, path string) (err error) {
	s, err := Open(ctx, path)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, s.Close())
	}()
	if err := s.checkIntegrity(ctx); err != nil {
		return err
	}
	p, err := newProvider(s.db, s.migrations)
	if err != nil {
		return err
	}
	version, newest, err := p.GetVersions(ctx)
	switch {
	case err != nil:
		return err
	case version < 1:
		return errors.New("it holds no Pisati schema")
	case version > newest:
		return fmt.Errorf("%w: it is at version %d, and the newest this Pisati knows is %d", ErrSchemaNewer, version, newest)
	}
	return nil
}

// checkIntegrity returns an error wrapping ErrCorrupt, which names the
// first ten problems, unless PRAGMA integrity_check reports none. The
// query is written by hand: sqlc cannot parse a PRAGMA.
func (s *Store) checkIntegrity(ctx context.Context) (err error) {
	rows, err := s.db.QueryContext(ctx, "PRAGMA integrity_check(10)")
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, rows.Close())
	}()
	var problems []string
	for rows.Next() {
		var problem string
		if err := rows.Scan(&problem); err != nil {
			return err
		}
		problems = append(problems, problem)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(problems) == 1 && problems[0] == "ok" {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrCorrupt, strings.Join(problems, "; "))
}
