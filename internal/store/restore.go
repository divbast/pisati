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
	"slices"
	"strings"
	"time"

	"github.com/ncruces/go-sqlite3"
)

// ErrCorrupt is a database that PRAGMA integrity_check finds fault with.
var ErrCorrupt = errors.New("store: database failed its integrity check")

// restoreSuffix names the copy Restore builds beside the database it is
// to replace.
const restoreSuffix = ".restore"

// Restore replaces the database in the state directory that lock locks,
// DatabasePath of it, with a copy of the database at src. It first checks
// the copy, and refuses it, changing nothing in the state directory,
// unless PRAGMA integrity_check finds nothing wrong with it (ErrCorrupt)
// and its schema is one this Pisati knows: at least version 1, and no
// newer than Pisati's newest migration (ErrSchemaNewer).
//
// lock must hold its lock, as proof that no Pisati serves from the state
// directory; otherwise Restore returns ErrLockNotHeld. Migrate and Restore
// run one at a time under one Lock, and the lock cannot be released until
// Restore returns. No Store may have the database open meanwhile.
//
// Restore refuses, before it changes anything, a src that is one of the
// files it removes or replaces in the state directory, named by its path
// or reached through a link: the database, its write-ahead log, the log's
// index, its rollback journal, the copy Restore builds and their like. It
// would delete such a source before it had copied it.
//
// The copy is built beside the database, flushed to stable storage and
// renamed over it, so that the state directory holds the old database or
// the whole of the new one, never part of either. The old database's
// write-ahead log, its index and any rollback journal are removed first:
// SQLite would read them as the new database's own. The restored file has
// mode 0600. src must be absolute; Restore stops, failing, when ctx ends.
func Restore(ctx context.Context, src string, lock Lock) (err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("store: restore from %s: %w", src, err)
		}
	}()
	if err := absolute(src); err != nil {
		return err
	}
	dir, unhold, err := lock.hold()
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

	name := databaseFile
	tmp := name + restoreSuffix
	copied := append(sidecars(tmp), tmp)
	if err := refuseOwnFile(root, dir, src, slices.Concat([]string{name}, sidecars(name), copied)); err != nil {
		return err
	}
	// What a restore cut short may have left. The lock keeps any other
	// Restore from building a copy here meanwhile: one in another process
	// cannot take it, and one under a copy of the same Lock waits in hold.
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

// refuseOwnFile returns an error when src is one of the files names in
// root, whose path is dir: when its path, cleaned, is one of theirs, or
// when it is the same file as one of them, as through a symbolic link.
// Restore removes or replaces each of them, and would lose src with it.
func refuseOwnFile(root *os.Root, dir, src string, names []string) error {
	clean := filepath.Clean(src)
	// A src that cannot be read is reported when Restore copies it.
	srcInfo, statErr := os.Stat(src)
	for _, name := range names {
		own := filepath.Join(dir, name)
		if clean == own {
			return fmt.Errorf("it is %s, which Restore removes or replaces; restore from a copy kept elsewhere", own)
		}
		// Lstat, as removing a link to src would leave src itself alone.
		if info, err := root.Lstat(name); statErr == nil && err == nil && os.SameFile(srcInfo, info) {
			return fmt.Errorf("it is the same file as %s, which Restore removes or replaces; restore from a copy kept elsewhere", own)
		}
	}
	return nil
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
