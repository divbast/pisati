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
	"sync"

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
// Backup does, and must leave nothing of an unfinished copy behind. Unless
// made is nil, BackupTracked first calls it, before it touches any file,
// with a function, remove, that removes the copy: dest and every file
// SQLite may keep beside it. remove works in the directory BackupTracked
// opened for dest, whatever dest's path names by the time it runs; it may
// be called from any goroutine, at any time, and more than once, also once
// BackupTracked has returned. It leaves alone a file already gone, and a
// dest that BackupTracked refused or never created, and returns every
// failure to remove a file:
//   - called before BackupTracked has created dest, it keeps BackupTracked
//     from creating it: BackupTracked then fails, and nothing is made;
//   - called while BackupTracked copies, or once it has finished the copy,
//     it removes what is there;
//   - called once BackupTracked has failed, which removed the copy itself,
//     it returns what that removal could not remove.
//
// From when it creates dest, BackupTracked holds dest's directory open for
// remove, until remove has removed every file or BackupTracked fails. A
// caller that keeps a finished copy, and so never calls remove, leaves
// that directory open.
func (s *Store) BackupTracked(ctx context.Context, dest string, made func(remove func() error)) (err error) {
	if err := s.open(); err != nil {
		return err
	}
	// The copy goes whatever stopped the backup, so its removals run
	// under a context that never ends.
	cleanup := context.WithoutCancel(ctx)
	t := &tracked{}
	if made != nil {
		made(func() error { return t.remove(cleanup) })
	}
	defer func() {
		if err == nil {
			return
		}
		if left := t.failed(cleanup); left != nil {
			err = errors.Join(err, left)
		}
		err = explain.Wrap(err, "Pisati cannot back up the database %s to %s", s.path, dest)
	}()
	dir, name, err := splitPath(dest, "store.Store.Backup")
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
		return explain.New(err, "Pisati cannot open the directory %s: %s. Check that it is a directory that the user "+
			"that runs Pisati can enter", filepath.Clean(dir), pathless(err))
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
	// existing file and write over it. With the file and its sidecars all
	// new, every one that exists from now on is the copy's own.
	switch err := t.create(ctx, root, name); {
	case err == nil:
	case errors.Is(err, errRemoved), ctx.Err() != nil && errors.Is(err, ctx.Err()):
		return err
	case errors.Is(err, fs.ErrExist):
		return explain.New(err, "the file %s is there already, and a backup is written only to a new file. Back up to "+
			"a path where there is no file yet", dest)
	case errors.Is(err, fs.ErrPermission):
		return explain.New(err, "the user that runs Pisati may not create the file %s. Back up to a file in a "+
			"directory that this user can write to", dest)
	default:
		return explain.New(err, "Pisati cannot create the file %s: %s. Check that the user that runs Pisati can write "+
			"to the directory %s", dest, pathless(err), filepath.Clean(dir))
	}
	path := filepath.Join(dir, name)
	if err := s.copyTo(ctx, path); err != nil {
		if stop := stoppedBy(ctx, err, "Pisati stopped before it had copied the database into %s", path); stop != nil {
			return stop
		}
		return explain.Wrap(err, "Pisati cannot copy the database into %s", path)
	}
	if err := useRollbackJournal(ctx, path); err != nil {
		if stop := stoppedBy(ctx, err, "Pisati stopped before it had finished the copy %s", path); stop != nil {
			return stop
		}
		return explain.Wrap(err, "Pisati cannot finish the copy %s", path)
	}
	if err := syncDir(ctx, root); err != nil {
		return explain.New(err, "Pisati cannot make sure that the directory %s is written to disk: %s", filepath.Clean(dir),
			pathless(err))
	}
	return nil
}

// errRemoved is a backup whose caller removed the copy before it was
// made: BackupTracked makes none after that.
var errRemoved = errors.New("the backup was called off, and its copy removed, before Pisati had made it")

// tracked is a copy that BackupTracked makes, for the function it hands
// its caller that removes it. Its methods are safe for concurrent use.
type tracked struct {
	mu sync.Mutex
	// removed is set once remove has been called: no copy is made after.
	removed bool
	// dir is the copy's directory, opened for the removal from when the
	// copy is created until every file of it is removed; nil otherwise.
	dir   *os.Root
	names []string
	// left is what the removal BackupTracked made, as it failed, could not
	// remove.
	left error
}

// create creates the file name in root, as the copy, unless remove has
// been called already, which returns errRemoved.
func (t *tracked) create(ctx context.Context, root *os.Root, name string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.removed {
		if err := ctx.Err(); err != nil {
			return err
		}
		return errRemoved
	}
	if err := createFile(ctx, root, name); err != nil {
		return err
	}
	names := append(sidecars(name), name)
	// The same directory, opened again, so that the removal can outlive
	// BackupTracked's own handle.
	dir, err := root.OpenRoot(".")
	if err != nil {
		return errors.Join(err, removeFiles(context.WithoutCancel(ctx), root, names...))
	}
	t.dir, t.names = dir, names
	return nil
}

// remove removes the copy, if one was made and not removed already, and
// keeps one from being made after.
func (t *tracked) remove(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.removed = true
	if t.dir == nil {
		return t.left
	}
	if err := removeFiles(ctx, t.dir, t.names...); err != nil {
		return err
	}
	return t.close()
}

// failed removes the copy as BackupTracked fails, if one was made and not
// removed already, and returns every failure to remove a file, which
// remove returns from then on.
func (t *tracked) failed(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.dir == nil {
		return nil
	}
	t.left = removeFiles(ctx, t.dir, t.names...)
	return errors.Join(t.left, t.close())
}

// close closes the copy's directory. The caller holds mu.
func (t *tracked) close() error {
	name := t.dir.Name()
	err := t.dir.Close()
	t.dir = nil
	if err != nil {
		return explain.New(err, "Pisati cannot close the directory %s: %s", name, pathless(err))
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
