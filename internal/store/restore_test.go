// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package store_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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
	path := store.DatabasePath(state)
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
	return lockOn(t, tg.state)
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

		if err := store.Restore(t.Context(), src, lock); err != nil {
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

			err := store.Restore(t.Context(), src, lock)
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
	if _, _, err := migrate(t, s, path); err != nil {
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
	if err := store.Restore(ctx, src, store.Lock{}); !errors.Is(err, store.ErrLockNotHeld) {
		t.Errorf("Restore without the lock: %v, want ErrLockNotHeld", err)
	}
	tg.assertUntouched(t, before)

	if err := serving.Release(); err != nil {
		t.Fatal(err)
	}
	if err := store.Restore(ctx, src, serving); !errors.Is(err, store.ErrLockNotHeld) {
		t.Errorf("Restore with a released lock: %v, want ErrLockNotHeld", err)
	}
	tg.assertUntouched(t, before)
}

// TestRestoreRefusesItsOwnFiles restores from each file Restore removes
// or replaces in the state directory. Taken as the source, Restore would
// delete it before it had copied it.
func TestRestoreRefusesItsOwnFiles(t *testing.T) {
	backupData := readFile(t, sourceWith(t, "restored"))
	const reason = "which Restore removes or replaces"
	for _, name := range []string{
		"pisati.db", "pisati.db-wal", "pisati.db-shm", "pisati.db-journal",
		"pisati.db.restore", "pisati.db.restore-wal", "pisati.db.restore-shm", "pisati.db.restore-journal",
	} {
		t.Run(name, func(t *testing.T) {
			tg := newTarget(t)
			lock := tg.lock(t)
			inside := filepath.Join(tg.state, name)
			if name != "pisati.db" {
				if err := os.WriteFile(inside, backupData, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before, srcBefore := checksum(t, tg.path), checksum(t, inside)
			// Spelt uncleaned: the path, not its spelling, is what counts.
			src := tg.state + string(filepath.Separator) + "." + string(filepath.Separator) + name

			err := store.Restore(t.Context(), src, lock)
			if err == nil || !strings.Contains(err.Error(), reason) || !strings.Contains(err.Error(), src) {
				t.Fatalf("Restore from %s: %v; want a refusal naming it and saying %q", src, err, reason)
			}
			if checksum(t, inside) != srcBefore || checksum(t, tg.path) != before {
				t.Error("the refused restore changed its source or the database")
			}
			want := []string{"pisati.db", "pisati.lock"}
			if name != "pisati.db" {
				want = append(want, name)
				slices.Sort(want)
			}
			if got := tg.files(t); !slices.Equal(got, want) {
				t.Errorf("state directory after the refused restore = %v, want %v", got, want)
			}
		})
	}

	// By its path alone: with no file there to compare, the refusal still
	// says why, rather than that the source does not exist.
	t.Run("one of their names with no file", func(t *testing.T) {
		tg := newTarget(t)
		lock := tg.lock(t)
		src := filepath.Join(tg.state, "pisati.db.restore-shm")

		err := store.Restore(t.Context(), src, lock)
		if err == nil || !strings.Contains(err.Error(), reason) {
			t.Fatalf("Restore from %s: %v; want a refusal saying %q", src, err, reason)
		}
		tg.assertUntouched(t, checksum(t, tg.path))
	})

	t.Run("a symbolic link to one of them", func(t *testing.T) {
		tg := newTarget(t)
		lock := tg.lock(t)
		inside := filepath.Join(tg.state, "pisati.db.restore")
		if err := os.WriteFile(inside, backupData, 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(t.TempDir(), "backup.db")
		if err := os.Symlink(inside, link); err != nil {
			t.Fatal(err)
		}
		srcBefore := checksum(t, inside)

		err := store.Restore(t.Context(), link, lock)
		if err == nil || !strings.Contains(err.Error(), reason) {
			t.Fatalf("Restore from a link to %s: %v; want a refusal saying %q", inside, err, reason)
		}
		if checksum(t, inside) != srcBefore {
			t.Error("the refused restore changed the file its source links to")
		}
	})
}

// TestRestoreSharedLockRunsOneAtATime runs Restores at once under copies
// of one Lock. Each builds its copy under the same name, so each must
// wait for the one before it to finish.
func TestRestoreSharedLockRunsOneAtATime(t *testing.T) {
	tg := newTarget(t)
	lock := tg.lock(t)
	ctx := t.Context()

	const restores = 8
	sources := make([]string, restores)
	for i := range sources {
		sources[i] = sourceWith(t, fmt.Sprintf("restored-%d", i))
	}
	errs := make(chan error, restores)
	var wg sync.WaitGroup
	for _, src := range sources {
		held := lock
		wg.Go(func() {
			errs <- store.Restore(ctx, src, held)
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}

	if got := tg.files(t); !slices.Equal(got, []string{"pisati.db", "pisati.lock"}) {
		t.Errorf("state directory after the restores = %v, want only the database and the lock", got)
	}
	s := openStore(t, tg.path)
	found := 0
	for i := range restores {
		if hasSession(t, s, fmt.Sprintf("restored-%d", i)) {
			found++
		}
	}
	if found != 1 || hasSession(t, s, "live") {
		t.Errorf("the database holds %d of the restored sessions, want exactly one, the last restore's", found)
	}
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
