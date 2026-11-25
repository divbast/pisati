// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/divbast/pisati/internal/config"
	"github.com/divbast/pisati/internal/crypto"
	"github.com/divbast/pisati/internal/telemetry"
)

// keysCmd is pisati keys.
type keysCmd struct {
	Rewrap rewrapCmd `cmd:"" help:"Encrypt the data keys with a new main key, in place of the one in use. Stop pisati serve first."`
	Rotate rotateCmd `cmd:"" help:"Make a new data key, and use it from now on. Stop pisati serve first."`
}

// rewrapCmd is pisati keys rewrap.
type rewrapCmd struct {
	NewMainKeyFile string `help:"The file that holds the new main key: 32 bytes in base64, as the command 'openssl rand -base64 32' writes them." placeholder:"PATH" required:""`
}

// Run wraps the data keys under the main key in c.NewMainKeyFile, in
// place of the configured one, under the state directory's lock, which
// serve holds while it runs, and once the database is migrated.
func (c rewrapCmd) Run(ctx context.Context, h Host) error {
	return untilHalt(h, func() error { return c.rewrap(ctx, h) })
}

// rewrap is Run's work.
func (c rewrapCmd) rewrap(ctx context.Context, h Host) (err error) {
	cfg, creds, err := loadConfig(ctx, h)
	if err != nil {
		return err
	}
	path, err := absolute(c.NewMainKeyFile)
	if err != nil {
		return err
	}
	log := telemetry.NewLogger(h.Stderr, cfg.Log)
	st, _, _, err := openState(ctx, cfg.Storage.StateDir)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, st.close())
	}()
	// The new key first: a file that holds none fails before anything is
	// generated for the old one.
	to, _, err := loadMainKey(ctx, config.FromFile{Path: path}, nil, st.dir, log)
	if err != nil {
		return err
	}
	from, origin, err := loadMainKey(ctx, cfg.Keys.MainKey, creds, st.dir, log)
	if err != nil {
		return err
	}
	switch err := crypto.Rewrap(ctx, st.db, from, to); {
	case errors.Is(err, crypto.ErrMainKeyMismatch):
		return mainKeyMismatch(err, cfg.Keys.MainKey, st.dir, origin)
	case err != nil:
		return err
	}
	_, err = fmt.Fprintln(h.Stdout, "The data keys are now encrypted with the new main key. Before you start pisati serve again, "+
		"set keys.main_key_file or PISATI_KEYS_MAIN_KEY_FILE to the new key file, "+
		"or put the new key in the credential pisati-main-key.")
	return err
}

// rotateCmd is pisati keys rotate.
type rotateCmd struct{}

// Run makes a new data key current and queues the re-encryption under it,
// under the state directory's lock, which serve holds while it runs, and
// once the database is migrated. serve runs the re-encryption when it
// next starts.
func (c rotateCmd) Run(ctx context.Context, h Host) error {
	return untilHalt(h, func() error { return c.rotate(ctx, h) })
}

// rotate is Run's work.
func (rotateCmd) rotate(ctx context.Context, h Host) (err error) {
	cfg, creds, err := loadConfig(ctx, h)
	if err != nil {
		return err
	}
	log := telemetry.NewLogger(h.Stderr, cfg.Log)
	st, _, _, err := openState(ctx, cfg.Storage.StateDir)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, st.close())
	}()
	key, origin, err := loadMainKey(ctx, cfg.Keys.MainKey, creds, st.dir, log)
	if err != nil {
		return err
	}
	keyring, err := openKeyring(ctx, h, st, cfg.Keys.MainKey, key, origin)
	if err != nil {
		return err
	}
	if err := crypto.Rotate(ctx, st.db, keyring); err != nil {
		return err
	}
	_, err = fmt.Fprintln(h.Stdout, "A new data key is now in use.")
	return err
}
