// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync/atomic"

	"github.com/divbast/pisati/internal/store"
)

// backupCmd is pisati backup.
type backupCmd struct {
	Dest string `arg:"" help:"The file to write the copy to. It must not exist yet."`
}

// Run writes a copy of the database to the file c.Dest names. It takes no
// lock, so serve can run beside it. With no database, it refuses, and
// writes nothing: the store would make an empty one to copy. A signal
// that stops it leaves nothing of an unfinished copy at c.Dest; a second
// one stops it at once, and Run removes what the copy has written so far.
func (c backupCmd) Run(ctx context.Context, h Host) error {
	dest, err := absolute(c.Dest)
	if err != nil {
		return err
	}
	// Ended by a halt too, so that the copy stops writing then.
	work, cancel := context.WithCancel(ctx)
	defer cancel()
	var remove atomic.Pointer[func() error]
	err = untilHalt(h, func() error {
		return c.backup(work, h, dest, func(r func() error) { remove.Store(&r) })
	})
	var why string
	switch {
	case errors.Is(err, errHalted):
		cancel()
		why = "the backup stopped at once, before it finished, because a second SIGTERM or SIGINT signal arrived"
	case stopped(ctx, err):
		why = "the backup stopped before it finished, because a SIGTERM or SIGINT signal arrived"
	default:
		return err
	}
	if r := remove.Load(); r != nil {
		if err := (*r)(); err != nil {
			return notRemoved(err, why, dest)
		}
	}
	return fmt.Errorf("%s. No backup was made, and nothing of it is left at %s", why, dest)
}

// notRemoved is the error of a backup to dest stopped, as why says, whose
// unfinished copy's files err says Pisati cannot remove: one problem for
// each file, naming it, with the operating system's reason.
func notRemoved(err error, why, dest string) error {
	var errs []error
	for _, p := range separate(err) {
		pe, ok := errors.AsType[*fs.PathError](p.err)
		if !ok {
			errs = append(errs, explain(p.err, "%s. No backup was made, but Pisati cannot remove what the unfinished "+
				"copy wrote at %s: %s. Remove it", why, dest, p.msg))
			continue
		}
		errs = append(errs, explain(p.err, "%s. No backup was made, but Pisati cannot remove the file %s, which the "+
			"unfinished copy wrote: %s. Remove it", why, pe.Path, pe.Err))
	}
	return errors.Join(errs...)
}

// backup is Run's work: it copies the database to dest, and calls made
// as the store's BackupTracked does.
func (c backupCmd) backup(ctx context.Context, h Host, dest string, made func(remove func() error)) (err error) {
	cfg, _, err := loadConfig(ctx, h)
	if err != nil {
		return err
	}
	path := store.DatabasePath(cfg.Storage.StateDir)
	switch _, err := os.Lstat(path); {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("there is no database to back up at %s. Nothing was written", path)
	case err != nil:
		return explain(err, "Pisati cannot read the database %s: %s", path, reason(err))
	}
	db, err := store.Open(ctx, path)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, db.Close())
	}()
	return db.BackupTracked(ctx, dest, made)
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
	if err := checkStateDir(dir); err != nil {
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
