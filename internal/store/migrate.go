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
	"path/filepath"

	"github.com/pressly/goose/v3"

	"github.com/divbast/pisati/internal/explain"
)

// SchemaVersion is the version of a database's schema: the number that
// the file name of the newest migration applied to it starts with. A
// database never migrated is at 0.
type SchemaVersion int64

// ErrSchemaNewer is a database whose schema a newer Pisati migrated. This
// Pisati cannot know what that schema means, so it changes nothing in it.
var ErrSchemaNewer = errors.New("a newer version of Pisati has updated its schema, the layout of its tables")

// migrations are the goose migrations Pisati is built with.
//
//go:embed migrations/*.sql
var migrations embed.FS

// pisatiMigrations returns the migrations Pisati is built with, at the root
// of the FS, where goose looks for them.
func pisatiMigrations() (fs.FS, error) {
	sub, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return nil, fmt.Errorf("the schema updates (the changes to the database's layout) built into Pisati cannot "+
			"be read, which is a bug in Pisati: %w", err)
	}
	return sub, nil
}

// Migrate applies, in version order, each migration the database has not
// had, each in a transaction of its own, and returns the schema version it
// found, from, and the one it left, to.
//
// lock must hold the lock on the state directory whose database the Store
// has open, DatabasePath of it, so that no other Pisati migrates the same
// file meanwhile; otherwise Migrate returns ErrLockNotHeld and writes
// nothing. Migrate and Restore run one at a time under one Lock: waiting
// for another, Migrate returns ctx's error if ctx ends first, and Release
// waits for a Migrate that runs.
//
// A step that fails is rolled back, and Migrate returns its error, with to
// the version of the last step that succeeded. A database at a version
// newer than any migration the Store has (Pisati's own, or those
// WithMigrations gave) is refused with an error wrapping
// ErrSchemaNewer before anything is written to it, and from and to are
// both its version.
func (s *Store) Migrate(ctx context.Context, lock Lock) (from, to SchemaVersion, err error) {
	if err := s.open(); err != nil {
		return 0, 0, err
	}
	dir, unhold, err := lock.hold(ctx)
	if err != nil {
		return 0, 0, explain.Wrap(err, "Pisati cannot update the schema of the database %s, the layout of its tables",
			s.path)
	}
	defer unhold()
	if db := DatabasePath(dir); filepath.Clean(s.path) != db {
		return 0, 0, explain.New(ErrLockNotHeld, "Pisati cannot update the schema of the database %s, because the lock "+
			"it was given is on the state directory %s, whose database is %s. This is a bug in Pisati", s.path, dir, db)
	}
	p, err := newProvider(s.db, s.migrations)
	if err != nil {
		return 0, 0, explain.Wrap(err, "Pisati cannot update the schema of the database %s, the layout of its tables",
			s.path)
	}
	// goose creates its table of versions if there is none, as in a
	// database never migrated. A newer database has the table, so reading
	// its version writes nothing to it.
	current, latest, err := p.GetVersions(ctx)
	if err != nil {
		return 0, 0, explain.Wrap(err, "Pisati cannot read the schema version of the database %s, the version of "+
			"the layout of its tables", s.path)
	}
	from, newest := SchemaVersion(current), SchemaVersion(latest)
	if from > newest {
		return from, from, explain.New(ErrSchemaNewer, "the database %s has schema version %d, the version of the "+
			"layout of its tables, but this Pisati knows versions up to %d only: a newer Pisati has updated it. Pisati "+
			"changes nothing in it. Run a version of Pisati that knows schema version %d", s.path, from, newest, from)
	}

	// goose applies the steps in order, and stops at the first that fails,
	// having applied those before it.
	applied, err := p.Up(ctx)
	if partial, ok := errors.AsType[*goose.PartialError](err); ok {
		applied = partial.Applied
	}
	to = from
	if len(applied) > 0 {
		to = SchemaVersion(applied[len(applied)-1].Source.Version)
	}
	if err != nil {
		return from, to, explain.Wrap(err, "Pisati cannot update the schema of the database %s, the layout of its "+
			"tables, past version %d", s.path, to)
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
