// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package store_test

import (
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/divbast/pisati/internal/store"
)

// newestVersion is the version of Pisati's newest migration.
const newestVersion store.SchemaVersion = 1

// migration is a goose migration that runs up.
func migration(up string) *fstest.MapFile {
	return &fstest.MapFile{Data: []byte("-- +goose Up\n" + up)}
}

// newerMigrations is what a newer Pisati would be built with: every one
// of this Pisati's migrations, and one more.
func newerMigrations(t *testing.T) fstest.MapFS {
	t.Helper()
	ours := os.DirFS("migrations")
	names, err := fs.Glob(ours, "*.sql")
	if err != nil || len(names) == 0 {
		t.Fatalf("Pisati's migrations: %v, %v", names, err)
	}
	fsys := fstest.MapFS{}
	for _, name := range names {
		data, err := fs.ReadFile(ours, name)
		if err != nil {
			t.Fatal(err)
		}
		fsys[name] = &fstest.MapFile{Data: data}
	}
	fsys["99999_newer.sql"] = migration("CREATE TABLE newer (id INTEGER PRIMARY KEY) STRICT;\n")
	return fsys
}

// tables returns the name of each table and index s holds, with whether
// each table is STRICT, leaving out SQLite's own and goose's.
func tables(t *testing.T, s *store.Store) map[string]string {
	t.Helper()
	rows, err := s.DB().QueryContext(t.Context(), `
		SELECT s.type, s.name, coalesce(l.strict, 0)
		FROM sqlite_schema AS s
		LEFT JOIN pragma_table_list AS l ON l.schema = 'main' AND l.name = s.name
		WHERE s.name NOT LIKE 'sqlite_%' AND s.name <> 'goose_db_version'`)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for rows.Next() {
		var kind, name string
		var strict bool
		if err := rows.Scan(&kind, &name, &strict); err != nil {
			t.Fatal(err)
		}
		if kind == "table" && strict {
			kind = "strict table"
		}
		got[name] = kind
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	return got
}

// schemaSize counts every table and index in s, goose's own included.
func schemaSize(t *testing.T, s *store.Store) int {
	t.Helper()
	var n int
	if err := s.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM sqlite_schema").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestMigrateFreshThenNoop(t *testing.T) {
	path := dbPath(t)
	s := openStore(t, path)
	lock := lockOn(t, filepath.Dir(path))
	ctx := t.Context()

	from, to, err := s.Migrate(ctx, lock)
	if err != nil || from != 0 || to != newestVersion {
		t.Fatalf("first Migrate = %d, %d, %v; want 0, %d, nil", from, to, err, newestVersion)
	}
	want := map[string]string{
		"sessions":                  "strict table",
		"sessions_by_expiry":        "index",
		"sessions_by_user":          "index",
		"jobs":                      "strict table",
		"jobs_by_state_run_at":      "index",
		"jobs_by_state_finished_at": "index",
		"data_keys":                 "strict table",
		"data_keys_one_current":     "index",
	}
	if got := tables(t, s); !maps.Equal(got, want) {
		t.Errorf("schema = %v, want %v", got, want)
	}

	from, to, err = s.Migrate(ctx, lock)
	if err != nil || from != newestVersion || to != newestVersion {
		t.Fatalf("second Migrate = %d, %d, %v; want %d, %d, nil", from, to, err, newestVersion, newestVersion)
	}
}

func TestMigrateRefusesNewerSchema(t *testing.T) {
	path := dbPath(t)
	lock := lockOn(t, filepath.Dir(path))
	ctx := t.Context()

	newer, err := store.Open(ctx, path, store.WithMigrations(newerMigrations(t)))
	if err != nil {
		t.Fatal(err)
	}
	_, newerVersion, err := newer.Migrate(ctx, lock)
	if err := errors.Join(err, newer.Close()); err != nil {
		t.Fatal(err)
	}
	before := checksum(t, path)

	s, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	from, to, err := s.Migrate(ctx, lock)
	if cerr := s.Close(); cerr != nil {
		t.Fatal(cerr)
	}
	if !errors.Is(err, store.ErrSchemaNewer) {
		t.Fatalf("Migrate: %v, want ErrSchemaNewer", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("Migrate: %v; want the error to name %s", err, path)
	}
	if from != newerVersion || to != newerVersion {
		t.Errorf("Migrate = %d, %d; want %d, %d: the version found, left as it was", from, to, newerVersion, newerVersion)
	}
	if checksum(t, path) != before {
		t.Error("Migrate changed the database it refused")
	}
}

func TestMigrateRollsBackAFailedStep(t *testing.T) {
	path := dbPath(t)
	lock := lockOn(t, filepath.Dir(path))
	s := openStore(t, path, store.WithMigrations(fstest.MapFS{
		"00001_first.sql": migration("CREATE TABLE first (id INTEGER PRIMARY KEY) STRICT;\n"),
		// The table is created, then the INSERT fails: a STRICT table
		// refuses text in an INTEGER column.
		"00002_second.sql": migration("CREATE TABLE second (id INTEGER PRIMARY KEY) STRICT;\n" +
			"INSERT INTO second (id) VALUES ('not a number');\n"),
	}))
	ctx := t.Context()

	for _, wantFrom := range []store.SchemaVersion{0, 1} {
		from, to, err := s.Migrate(ctx, lock)
		if err == nil {
			t.Fatal("Migrate succeeded over a failing step")
		}
		if from != wantFrom || to != 1 {
			t.Errorf("Migrate = %d, %d, %v; want %d, 1: the steps before the failure applied", from, to, err, wantFrom)
		}
		got := slices.Sorted(maps.Keys(tables(t, s)))
		if !slices.Equal(got, []string{"first"}) {
			t.Errorf("tables after the failure = %v, want only [first]: the failed step rolled back", got)
		}
	}
}

func TestMigrateRefusesWithoutTheLock(t *testing.T) {
	state := t.TempDir()
	path := store.DatabasePath(stateDir(t, state))
	s := openStore(t, path)
	ctx := t.Context()

	released, err := store.AcquireLock(ctx, stateDir(t, state))
	if err != nil {
		t.Fatal(err)
	}
	if err := released.Release(); err != nil {
		t.Fatal(err)
	}
	for name, lock := range map[string]store.Lock{
		"the zero Lock":                 {},
		"a released lock":               released,
		"the lock on another directory": lockOn(t, t.TempDir()),
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := s.Migrate(t.Context(), lock); !errors.Is(err, store.ErrLockNotHeld) {
				t.Errorf("Migrate: %v, want ErrLockNotHeld", err)
			}
			if n := schemaSize(t, s); n != 0 {
				t.Errorf("Migrate without the lock wrote %d schema entries", n)
			}
		})
	}

	// The lock covers the state directory's database, not another file
	// that happens to be in the directory.
	other := openStore(t, filepath.Join(state, "other.db"))
	held := lockOn(t, state)
	if _, _, err := other.Migrate(ctx, held); !errors.Is(err, store.ErrLockNotHeld) {
		t.Errorf("Migrate of %s under the lock on %s: %v, want ErrLockNotHeld", "other.db", state, err)
	}
	if n := schemaSize(t, other); n != 0 {
		t.Errorf("Migrate under another file's lock wrote %d schema entries", n)
	}
}

func TestDatabasePath(t *testing.T) {
	if got, want := store.DatabasePath(stateDir(t, "/var/lib/pisati")), "/var/lib/pisati/pisati.db"; got != want {
		t.Errorf("DatabasePath = %s, want %s", got, want)
	}
}
