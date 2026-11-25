// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package main

import (
	"context"
	"errors"
	"fmt"
)

// migrateCmd is pisati migrate.
type migrateCmd struct{}

// Run migrates the database, under the state directory's lock, and prints
// the schema versions it found and left.
func (migrateCmd) Run(ctx context.Context, h Host) error {
	cfg, _, err := loadConfig(ctx, h)
	if err != nil {
		return err
	}
	st, from, to, err := openState(ctx, cfg.Storage.StateDir)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(h.Stdout, "schema %d -> %d\n", from, to)
	return errors.Join(err, st.close())
}
