// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package store_test

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/divbast/pisati/internal/store"
)

// target is a state directory to restore into: it holds a database with
// the session "live", closed, as restore finds it once serve has stopped.
type target struct {
	state, path string
}

func newTarget(t *testing.T) target {
	t.Helper()
	state := t.TempDir()
	path := filepath.Join(state, "pisati.db")
	s := migratedStore(t, path)
	insertSession(t, s, "live", 1)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return target{state: state, path: path}
}

// lock takes the lock on the target's state directory until the test ends.
func (tg target) lock(t *testing.T) store.Lock {
	t.Helper()
	l, err := store.AcquireLock(t.Context(), tg.state)
	if err != nil {
		t.Fatalf("AcquireLock(%s): %v", tg.state, err)
	}
	t.Cleanup(func() {
		if err := l.Release(); err != nil {
			t.Error(err)
		}
	})
	return l
}

// files lists the state directory.
func (tg target) files(t *testing.T) []string {
	t.Helper()
	return listDir(t, tg.state)
}

// assertUntouched fails the test unless the database is as it was, with
// nothing beside it but the lock.
func (tg target) assertUntouched(t *testing.T, before [32]byte) {
	t.Helper()
	if checksum(t, tg.path) != before {
		t.Error("the refused restore changed the database")
	}
	if got := tg.files(t); !slices.Equal(got, []string{"pisati.db", "pisati.lock"}) {
		t.Errorf("state directory after the refused restore = %v, want only the database and the lock", got)
	}
}

// sourceWith returns a backup holding the session name.
func sourceWith(t *testing.T, name string) string {
	t.Helper()
	s := migratedStore(t, dbPath(t))
	insertSession(t, s, name, 1)
	return backup(t, s, "source.db")
}

func TestRestoreChecksIntegrityAndVersion(t *testing.T) {
	t.Run("a sound backup replaces the database", func(t *testing.T) {
		tg := newTarget(t)
		lock := tg.lock(t)
		src := sourceWith(t, "restored")
		// What a database that was not closed cleanly leaves: were these
		// kept, SQLite would read them as the restored database's own.
		for _, name := range []string{"pisati.db-wal", "pisati.db-shm"} {
			if err := os.WriteFile(filepath.Join(tg.state, name), []byte("stale"), 0o600); err != nil {
				t.Fatal(err)
			}
		}

		if err := store.Restore(t.Context(), src, tg.path, lock); err != nil {
			t.Fatalf("Restore: %v", err)
		}
		if got := listDir(t, filepath.Dir(src)); !slices.Equal(got, []string{"source.db"}) {
			t.Errorf("beside the source after Restore = %v, want the source alone: reading it writes nothing", got)
		}
		if got := tg.files(t); !slices.Equal(got, []string{"pisati.db", "pisati.lock"}) {
			t.Errorf("state directory after Restore = %v, want only the database and the lock", got)
		}
		info, err := os.Stat(tg.path)
		if err != nil {
			t.Fatal(err)
		}
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Errorf("restored database mode = %v, want -rw-------", mode)
		}
		s := openStore(t, tg.path)
		if !hasSession(t, s, "restored") || hasSession(t, s, "live") {
			t.Error("the database is not the backup")
		}
	})

	refused := []struct {
		name   string
		source func(t *testing.T) string
		want   error // nil: any error
	}{
		{"a corrupt database", corruptSource, store.ErrCorrupt},
		{"a database from a newer Pisati", newerSource, store.ErrSchemaNewer},
		{"a database without Pisati's schema", emptySource, nil},
		{"a file that is not a database", notADatabase, nil},
		{"a file that does not exist", func(t *testing.T) string {
			return filepath.Join(t.TempDir(), "missing.db")
		}, nil},
	}
	for _, c := range refused {
		t.Run(c.name+" is refused", func(t *testing.T) {
			tg := newTarget(t)
			lock := tg.lock(t)
			src := c.source(t)
			before := checksum(t, tg.path)

			err := store.Restore(t.Context(), src, tg.path, lock)
			switch {
			case err == nil:
				t.Fatal("Restore succeeded")
			case c.want != nil && !errors.Is(err, c.want):
				t.Errorf("Restore: %v, want %v", err, c.want)
			}
			tg.assertUntouched(t, before)
		})
	}
}

// corruptSource returns a database that PRAGMA integrity_check faults:
// an index's page is still in the file, but the schema no longer names
// it, so nothing uses the page.
func corruptSource(t *testing.T) string {
	t.Helper()
	path := dbPath(t)
	s := migratedStore(t, path)
	insertSession(t, s, "source", 1)
	if _, err := s.DB().ExecContext(t.Context(), `
		PRAGMA writable_schema = ON;
		DELETE FROM sqlite_schema WHERE name = 'sessions_by_user';
		PRAGMA writable_schema = OFF;`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// newerSource returns a database a newer Pisati migrated.
func newerSource(t *testing.T) string {
	t.Helper()
	path := dbPath(t)
	s := openStore(t, path, store.WithMigrations(newerMigrations(t)))
	if _, _, err := s.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return backup(t, s, "newer.db")
}

// emptySource returns an SQLite database that was never migrated.
func emptySource(t *testing.T) string {
	t.Helper()
	path := dbPath(t)
	if err := openStore(t, path).Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// notADatabase returns a file that is not an SQLite database.
func notADatabase(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(path, []byte("not a database, though long enough to have a header's worth of bytes in it"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRestoreRefusesWhileLocked(t *testing.T) {
	tg := newTarget(t)
	src := sourceWith(t, "restored")
	before := checksum(t, tg.path)
	ctx := t.Context()

	// serve holds the lock for as long as it runs.
	serving, err := store.AcquireLock(ctx, tg.state)
	if err != nil {
		t.Fatal(err)
	}
	// Releasing twice does nothing, so these hold however the test ends.
	t.Cleanup(func() {
		if err := serving.Release(); err != nil {
			t.Error(err)
		}
	})
	if _, err := store.AcquireLock(ctx, tg.state); !errors.Is(err, store.ErrLocked) {
		t.Fatalf("AcquireLock while serve holds the lock: %v, want ErrLocked", err)
	}
	if err := store.Restore(ctx, src, tg.path, store.Lock{}); !errors.Is(err, store.ErrLockNotHeld) {
		t.Errorf("Restore without the lock: %v, want ErrLockNotHeld", err)
	}
	tg.assertUntouched(t, before)

	other, err := store.AcquireLock(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := other.Release(); err != nil {
			t.Error(err)
		}
	})
	if err := store.Restore(ctx, src, tg.path, other); !errors.Is(err, store.ErrLockNotHeld) {
		t.Errorf("Restore with another directory's lock: %v, want ErrLockNotHeld", err)
	}
	tg.assertUntouched(t, before)

	if err := errors.Join(serving.Release(), other.Release()); err != nil {
		t.Fatal(err)
	}
	if err := store.Restore(ctx, src, tg.path, serving); !errors.Is(err, store.ErrLockNotHeld) {
		t.Errorf("Restore with a released lock: %v, want ErrLockNotHeld", err)
	}
	tg.assertUntouched(t, before)
}

func TestLockIsExclusive(t *testing.T) {
	state := t.TempDir()
	ctx := t.Context()

	first, err := store.AcquireLock(ctx, state)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AcquireLock(ctx, state); !errors.Is(err, store.ErrLocked) {
		t.Fatalf("second AcquireLock: %v, want ErrLocked", err)
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	if err := first.Release(); err != nil {
		t.Errorf("releasing a released lock: %v, want nil", err)
	}
	if err := (store.Lock{}).Release(); err != nil {
		t.Errorf("releasing the zero Lock: %v, want nil", err)
	}

	again, err := store.AcquireLock(ctx, state)
	if err != nil {
		t.Fatalf("AcquireLock after Release: %v", err)
	}
	if err := again.Release(); err != nil {
		t.Fatal(err)
	}

	if _, err := store.AcquireLock(ctx, "state"); err == nil {
		t.Error("AcquireLock took a relative path")
	}
}
