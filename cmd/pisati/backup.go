// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/divbast/pisati/internal/store"
)

// backupCmd is pisati backup.
type backupCmd struct {
	Dest string `arg:"" help:"The file to write the copy to. It must not exist yet."`
}

// Run writes a copy of the database to the file c.Dest names. It takes no
// lock, so serve can run beside it. With no database, it refuses, and
// writes nothing: the store would make an empty one to copy.
func (c backupCmd) Run(ctx context.Context, h Host) (err error) {
	cfg, _, err := loadConfig(ctx, h)
	if err != nil {
		return err
	}
	dest, err := absolute(c.Dest)
	if err != nil {
		return err
	}
	path := store.DatabasePath(cfg.Storage.StateDir)
	switch _, err := os.Lstat(path); {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("there is no database to back up at %s. Nothing was written", path)
	case err != nil:
		return fmt.Errorf("cannot read the database %s: %s", path, reason(err))
	}
	db, err := store.Open(ctx, path)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, db.Close())
	}()
	return db.Backup(ctx, dest)
}

// restoreCmd is pisati restore.
type restoreCmd struct {
	Src string `arg:"" help:"The copy to restore."`
}

// Run replaces the database with the copy c.Src names, under the state
// directory's lock, which serve holds while it runs.
func (c restoreCmd) Run(ctx context.Context, h Host) (err error) {
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
