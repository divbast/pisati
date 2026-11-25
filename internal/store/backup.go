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

	"github.com/ncruces/go-sqlite3"
	"github.com/ncruces/go-sqlite3/driver"

	"github.com/divbast/pisati/internal/explain"
)

// Backup writes a consistent copy of the database to dest, a new file it
// creates with mode 0600, through SQLite's online backup API. Writers
// carry on while it runs: the copy holds every transaction committed
// before it began, and none committed after. The copy is the one file,
// with a rollback journal rather than a write-ahead log, so that it can
// be read, by Restore among others, without anything written beside it.
//
// dest must be an absolute path in a directory that exists. Backup refuses
// a dest that exists, or a write-ahead log, index or journal beside it,
// with an error wrapping fs.ErrExist: SQLite would read those as the
// copy's own. It removes what it wrote when it fails, and stops, failing,
// when ctx ends.
func (s *Store) Backup(ctx context.Context, dest string) error {
	return s.BackupTracked(ctx, dest, nil)
}

// BackupTracked is Backup, for a caller that may have to return before
// Backup does, and must leave nothing of an unfinished copy behind. Once
// it has created dest, it calls made, unless made is nil, with a function
// that removes dest and every file SQLite may keep beside it. That
// function leaves alone a file already gone, may be called from any
// goroutine and more than once, and returns every failure to remove one.
// made is not called when Backup refuses dest, or fails before it
// creates it.
func (s *Store) BackupTracked(ctx context.Context, dest string, made func(remove func() error)) (err error) {
	if err := s.open(); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			err = explain.Wrap(err, "Pisati cannot back up the database %s to %s", s.path, dest)
		}
	}()
	dir, name, err := splitPath(dest)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return explain.New(err, "the directory %s does not exist. Back up to a file in a directory that exists",
			filepath.Clean(dir))
	case errors.Is(err, fs.ErrPermission):
		return explain.New(err, "the user that runs Pisati may not open the directory %s. Back up to a file in a "+
			"directory that this user can write to", filepath.Clean(dir))
	case err != nil:
		return explain.New(err, "Pisati cannot open the directory %s: %s", filepath.Clean(dir), pathless(err))
	}
	defer func() {
		err = errors.Join(err, root.Close())
	}()

	for _, sidecar := range sidecars(name) {
		switch _, err := root.Lstat(sidecar); {
		case err == nil:
			return explain.New(fs.ErrExist, "the file %s is there already, and SQLite would read it as part of the copy. "+
				"Remove it, or back up to another path", filepath.Join(dir, sidecar))
		case !errors.Is(err, fs.ErrNotExist):
			return explain.New(errors.Join(fs.ErrExist, err), "Pisati cannot tell whether the file %s is there, which SQLite "+
				"would read as part of the copy: %s", filepath.Join(dir, sidecar), pathless(err))
		}
	}
	// Created here, refusing one that exists: SQLite would open an
	// existing file and write over it.
	if err := createFile(root, name); errors.Is(err, fs.ErrExist) {
		return explain.New(err, "the file %s is there already, and a backup is written only to a new file. Back up to "+
			"a path where there is no file yet", dest)
	} else if errors.Is(err, fs.ErrPermission) {
		return explain.New(err, "the user that runs Pisati may not create the file %s. Back up to a file in a "+
			"directory that this user can write to", dest)
	} else if err != nil {
		return explain.New(err, "Pisati cannot create the file %s: %s", dest, pathless(err))
	}
	if made != nil {
		made(func() error { return removePaths(dir, append(sidecars(name), name)...) })
	}
	// With the file and its sidecars all new, every one that exists now is
	// Backup's own.
	defer func() {
		if err != nil {
			err = errors.Join(err, removeFiles(root, append(sidecars(name), name)...))
		}
	}()
	path := filepath.Join(dir, name)
	if err := s.copyTo(ctx, path); err != nil {
		return explain.Wrap(err, "Pisati cannot copy the database into %s", path)
	}
	if err := useRollbackJournal(ctx, path); err != nil {
		return explain.Wrap(err, "Pisati cannot finish the copy %s", path)
	}
	if err := syncDir(root); err != nil {
		return explain.New(err, "Pisati cannot make sure that the directory %s is written to disk: %s", filepath.Clean(dir),
			pathless(err))
	}
	return nil
}

// copyTo copies the database into the empty database file at dest
// through SQLite's backup API, on one connection from the pool. The
// driver opens dest as a URI only when it starts with "file:", which an
// absolute path never does, so no character in it is read as part of
// one.
func (s *Store) copyTo(ctx context.Context, dest string) (err error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, conn.Close())
	}()
	return conn.Raw(func(c any) error {
		sc, ok := c.(driver.Conn)
		if !ok {
			return fmt.Errorf("its connection is a %T, not the SQLite driver's, which is a bug in Pisati", c)
		}
		raw := sc.Raw()
		// The driver stops a call that is running through SQLite's
		// interrupt, which follows the context set here.
		old := raw.SetInterrupt(ctx)
		defer raw.SetInterrupt(old)
		// All pages in one step, so the copy is of one read transaction:
		// in WAL mode that blocks no writer.
		return raw.Backup("main", dest)
	})
}

// useRollbackJournal switches the database at path from a write-ahead log
// to a rollback journal. A backup copies its source's header, which says
// WAL, and opening a WAL database writes a log and its index beside it,
// even to read it; one with a rollback journal is read without writing.
func useRollbackJournal(ctx context.Context, path string) (err error) {
	c, err := sqlite3.OpenContext(ctx, path)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, c.Close())
	}()
	old := c.SetInterrupt(ctx)
	defer c.SetInterrupt(old)
	stmt, _, err := c.Prepare("PRAGMA journal_mode = DELETE")
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, stmt.Close())
	}()
	if !stmt.Step() {
		return errors.Join(errors.New("SQLite did not answer which journal mode the copy has"), stmt.Err())
	}
	// SQLite answers with the mode in force, which stays WAL when it
	// cannot switch.
	if mode := stmt.ColumnText(0); mode != "delete" {
		return fmt.Errorf("SQLite left the copy in the journal mode %s, not delete", mode)
	}
	return nil
}

// removePaths removes each of names from the directory dir, by its path,
// leaving alone one that is gone already, and returns every failure.
// Unlike removeFiles, it needs no open Root, so it can run once Backup has
// returned.
func removePaths(dir string, names ...string) error {
	var errs []error
	for _, name := range names {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
