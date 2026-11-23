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
func (s *Store) Backup(ctx context.Context, dest string) (err error) {
	if err := s.open(); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			err = fmt.Errorf("store: back up %s to %s: %w", s.path, dest, err)
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
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, root.Close())
	}()

	for _, sidecar := range sidecars(name) {
		if _, err := root.Lstat(sidecar); !errors.Is(err, fs.ErrNotExist) {
			return errors.Join(fmt.Errorf("%s: %w", sidecar, fs.ErrExist), err)
		}
	}
	// Created here, refusing one that exists: SQLite would open an
	// existing file and write over it.
	if err := createFile(root, name); err != nil {
		return err
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
		return err
	}
	if err := useRollbackJournal(ctx, path); err != nil {
		return err
	}
	return syncDir(root)
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
			return fmt.Errorf("the connection is a %T, not the SQLite driver's", c)
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
		return errors.Join(errors.New("PRAGMA journal_mode returned no mode"), stmt.Err())
	}
	// SQLite answers with the mode in force, which stays WAL when it
	// cannot switch.
	if mode := stmt.ColumnText(0); mode != "delete" {
		return fmt.Errorf("the copy's journal mode stayed %s", mode)
	}
	return nil
}
