// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"

	"github.com/pressly/goose/v3"
)

// ErrSchemaNewer is a database whose schema a newer Pisati migrated. This
// Pisati cannot know what that schema means, so it changes nothing in it.
var ErrSchemaNewer = errors.New("store: database schema is newer than this Pisati")

// migrations are the goose migrations Pisati is built with.
//
//go:embed migrations/*.sql
var migrations embed.FS

// pisatiMigrations returns the migrations Pisati is built with, at the root
// of the FS, where goose looks for them.
func pisatiMigrations() (fs.FS, error) {
	sub, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return nil, fmt.Errorf("store: the migrations built in: %w", err)
	}
	return sub, nil
}

// Migrate applies, in version order, each migration the database has not
// had, each in a transaction of its own, and returns the schema version it
// found, from, and the one it left, to. A version is the number a
// migration's file name starts with; a database never migrated is at 0.
//
// A step that fails is rolled back, and Migrate returns its error, with to
// the version of the last step that succeeded. A database at a version
// newer than any migration the Store has (Pisati's own, or those
// WithMigrations gave) is refused with an error wrapping
// ErrSchemaNewer before anything is written to it, and from and to are
// both its version.
func (s *Store) Migrate(ctx context.Context) (from, to int64, err error) {
	if err := s.open(); err != nil {
		return 0, 0, err
	}
	p, err := newProvider(s.db, s.migrations)
	if err != nil {
		return 0, 0, fmt.Errorf("store: migrate %s: %w", s.path, err)
	}
	// goose creates its table of versions if there is none, as in a
	// database never migrated. A newer database has the table, so reading
	// its version writes nothing to it.
	from, newest, err := p.GetVersions(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("store: migrate %s: %w", s.path, err)
	}
	if from > newest {
		return from, from, fmt.Errorf("%w: %s is at version %d, and the newest this Pisati knows is %d", ErrSchemaNewer, s.path, from, newest)
	}

	// goose applies the steps in order, and stops at the first that fails,
	// having applied those before it.
	applied, err := p.Up(ctx)
	if partial, ok := errors.AsType[*goose.PartialError](err); ok {
		applied = partial.Applied
	}
	to = from
	if len(applied) > 0 {
		to = applied[len(applied)-1].Source.Version
	}
	if err != nil {
		return from, to, fmt.Errorf("store: migrate %s: stopped at version %d: %w", s.path, to, err)
	}
	return from, to, nil
}

// newProvider returns a goose Provider that migrates db with the
// migrations in fsys and nothing else.
func newProvider(db *sql.DB, fsys fs.FS) (*goose.Provider, error) {
	return goose.NewProvider(goose.DialectSQLite3, db, fsys,
		// goose's global registry holds migrations written in Go that any
		// package may add to: Pisati has none, and takes none from there.
		goose.WithDisableGlobalRegistry(true),
		// goose would print each step to stdout through the log package;
		// Migrate's caller reports what it returns instead.
		goose.WithLogger(goose.NopLogger()),
	)
}
