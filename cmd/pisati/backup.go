// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"
	"time"

	"github.com/divbast/pisati/internal/explain"
	"github.com/divbast/pisati/internal/store"
)

// backupCmd is pisati backup.
type backupCmd struct {
	Dest string `arg:"" help:"The file to write the copy to. It must not exist yet."`
}

// haltWait bounds how long a backup that a second SIGTERM or SIGINT
// stopped waits for its copy to stop, before it removes what the copy
// wrote. The copy stops as soon as it sees its context end, which it
// checks in SQLite's every step, so the wait is short when nothing is
// stuck; and short when something is, since the stop is to be at once.
const haltWait = 500 * time.Millisecond

// Run writes a copy of the database to the file c.Dest names. It takes no
// lock, so serve can run beside it. With no database, it refuses, and
// writes nothing: the store would make an empty one to copy. A signal
// that stops it leaves nothing of an unfinished copy at c.Dest; a second
// one stops it at once: Run ends the copy, waits up to haltWait for it to
// stop, and then removes what it wrote, finished or not.
func (c backupCmd) Run(ctx context.Context, h Host) error {
	// Ended by a halt too, so that the copy stops writing then.
	work, cancel := context.WithCancel(ctx)
	defer cancel()
	var made backupCopy
	done := make(chan error, 1)
	go func() { done <- c.backup(work, h, &made) }()
	var why string
	select {
	case err := <-done:
		if !stopped(ctx, err) {
			return err
		}
		why = "the backup stopped before it finished, because a SIGTERM or SIGINT signal arrived"
	case <-h.Halt:
		cancel()
		wait := time.NewTimer(haltWait)
		select {
		case <-done: //nolint:errcheck // the halt ends the command whatever the copy returned, and remove, below, reports what it left
		case <-wait.C:
		}
		wait.Stop()
		why = "the backup stopped at once, before it finished, because a second SIGTERM or SIGINT signal arrived"
	}
	dest, remove := made.giveUp(c.Dest)
	if remove != nil {
		if err := remove(); err != nil {
			return notRemoved(err, why, dest)
		}
	}
	return fmt.Errorf("%s. No backup was made, and nothing of it is left at %s", why, dest)
}

// backupCopy is what the command knows of the copy the store makes: its
// path, once the command has resolved it, and the store's function that
// removes it, once the store has handed it over. Its methods are safe for
// concurrent use.
type backupCopy struct {
	mu     sync.Mutex
	dest   string
	remove func() error
	// gaveUp is set once Run has stopped waiting for the copy: from then
	// on, no copy may be made.
	gaveUp bool
}

// resolved records dest, the copy's path.
func (b *backupCopy) resolved(dest string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.dest = dest
}

// made takes the store's function that removes the copy, as
// Store.BackupTracked hands it over before it makes anything. Once Run
// has given up, it calls it at once, so that the store makes no copy.
func (b *backupCopy) made(remove func() error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.remove = remove
	if b.gaveUp {
		//nolint:errcheck // called before the store has made anything, it has nothing to remove, and fails at nothing: it only keeps the copy from being made
		remove()
	}
}

// giveUp stops the copy from being made from now on, and returns its
// path, or given, the path as the command line gave it, before the
// command had resolved it, and the store's function that removes it, nil
// before the store has handed it over.
func (b *backupCopy) giveUp(given string) (dest string, remove func() error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.gaveUp = true
	return cmp.Or(b.dest, given), b.remove
}

// notRemoved is the error of a backup to dest stopped, as why says, whose
// unfinished copy's files err says Pisati cannot remove: one problem for
// each file, naming it, with the operating system's reason.
func notRemoved(err error, why, dest string) error {
	var errs []error
	for _, p := range separate(err) {
		pe, ok := errors.AsType[*fs.PathError](p.err)
		if !ok {
			errs = append(errs, explain.New(p.err, "%s. No backup was made, but Pisati cannot remove what the unfinished "+
				"copy wrote at %s: %s. Remove it", why, dest, p.msg))
			continue
		}
		errs = append(errs, explain.New(p.err, "%s. No backup was made, but Pisati cannot remove the file %s, which the "+
			"unfinished copy wrote: %s. Remove it", why, pe.Path, pe.Err))
	}
	return errors.Join(errs...)
}

// backup is Run's work: it reads the configuration, then resolves c.Dest,
// as restore and keys rewrap do theirs, and copies the database there,
// recording the copy in made.
func (c backupCmd) backup(ctx context.Context, h Host, made *backupCopy) (err error) {
	cfg, _, err := loadConfig(ctx, h)
	if err != nil {
		return err
	}
	dest, err := absolute(c.Dest)
	if err != nil {
		return err
	}
	made.resolved(dest)
	path := store.DatabasePath(cfg.Storage.StateDir)
	switch _, err := os.Lstat(path); {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("there is no database to back up at %s. Nothing was written", path)
	case err != nil:
		return explain.New(err, "Pisati cannot read the database %s: %s", path, reason(err))
	}
	db, err := store.Open(ctx, path)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, db.Close())
	}()
	return db.BackupTracked(ctx, dest, made.made)
}

// restoreCmd is pisati restore.
type restoreCmd struct {
	Src string `arg:"" help:"The copy that pisati backup made, to restore."`
}

// Run replaces the database with the copy c.Src names, under the state
// directory's lock, which serve holds while it runs.
func (c restoreCmd) Run(ctx context.Context, h Host) error {
	return untilHalt(h, func() error { return c.restore(ctx, h) })
}

// restore is Run's work.
func (c restoreCmd) restore(ctx context.Context, h Host) (err error) {
	cfg, _, err := loadConfig(ctx, h)
	if err != nil {
		return err
	}
	src, err := absolute(c.Src)
	if err != nil {
		return err
	}
	dir := cfg.Storage.StateDir
	if err := checkStateDir(ctx, dir); err != nil {
		return err
	}
	lock, err := lockState(ctx, dir)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, lock.Release())
	}()
	return store.Restore(ctx, src, lock)
}
