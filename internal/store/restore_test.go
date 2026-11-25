// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package store_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/divbast/pisati/internal/config"
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
	path := store.DatabasePath(stateDir(t, state))
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
	serving, err := store.AcquireLock(ctx, stateDir(t, tg.state))
	if err != nil {
		t.Fatal(err)
	}
	// Releasing twice does nothing, so these hold however the test ends.
	t.Cleanup(func() {
		if err := serving.Release(); err != nil {
			t.Error(err)
		}
	})
	if _, err := store.AcquireLock(ctx, stateDir(t, tg.state)); !errors.Is(err, store.ErrLocked) {
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
	const reason = "which a restore removes or replaces"
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

// TestQueuedRestoreStopsWithItsContext queues a Restore behind a Migrate
// that holds the lock, then ends the Restore's context. The waits here are
// bounded in real time rather than run in a synctest bubble: what holds
// Migrate up is SQLite's lock on the file, which a bubble cannot see.
func TestQueuedRestoreStopsWithItsContext(t *testing.T) {
	state := t.TempDir()
	path := store.DatabasePath(stateDir(t, state))
	s := openStore(t, path)
	lock := lockOn(t, state)
	ctx := t.Context()

	// SQLite's write lock, held on a connection of its own until letGo.
	conn, err := s.DB().Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	letGo := sync.OnceFunc(func() {
		if _, err := conn.ExecContext(ctx, "ROLLBACK"); err != nil {
			t.Error(err)
		}
		if err := conn.Close(); err != nil {
			t.Error(err)
		}
	})

	// The holder: Migrate must write to the new database, so it holds the
	// store's lock while it waits, up to the busy timeout, for SQLite's.
	// migrateErr is read only once migrated is closed.
	var migrateErr error
	migrated := make(chan struct{})
	go func() {
		defer close(migrated)
		_, _, migrateErr = s.Migrate(ctx, lock)
	}()
	// The database itself is the source of every Restore here: one that
	// gets the lock refuses it at once, and changes nothing.
	// Each result is sent, then its channel closed, so that the deferred
	// drain below returns whether or not the test has read it.
	var unfinished []<-chan error
	restore := func(rctx context.Context) <-chan error {
		done := make(chan error, 1)
		go func() {
			defer close(done)
			done <- store.Restore(rctx, path, lock)
		}()
		unfinished = append(unfinished, done)
		return done
	}
	defer func() {
		letGo()
		<-migrated
		for _, done := range unfinished {
			for range done {
			}
		}
	}()

	// Migrate takes the lock before anything else, but may not have yet,
	// and until it has, a Restore takes the lock first. One still waiting
	// after 50 ms is queued behind Migrate.
	var queued <-chan error
	var cancel context.CancelFunc
	for attempt := 0; queued == nil; attempt++ {
		if attempt == 100 {
			t.Fatal("Migrate never held the lock")
		}
		rctx, stop := context.WithCancel(ctx)
		done := restore(rctx)
		select {
		case err := <-done:
			stop()
			if err == nil || !strings.Contains(err.Error(), "removes or replaces") {
				t.Fatalf("Restore with the lock free: %v, want its source refused", err)
			}
			time.Sleep(10 * time.Millisecond)
		case <-time.After(50 * time.Millisecond):
			queued, cancel = done, stop
		}
	}
	cancel()
	select {
	case err := <-queued:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("queued Restore whose context ended: %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Error("a queued Restore did not return within 1 s of its context ending")
	}

	// Release waits for Migrate, which still holds the lock, and a Restore
	// that asks for the lock once Release has begun gets none.
	released := make(chan error, 1)
	go func() { released <- lock.Release() }()
	select {
	case err := <-released:
		t.Errorf("Release returned %v while Migrate held the lock", err)
	case <-time.After(100 * time.Millisecond):
	}
	select {
	case err := <-restore(ctx):
		if !errors.Is(err, store.ErrLockNotHeld) {
			t.Errorf("Restore once Release has begun: %v, want ErrLockNotHeld", err)
		}
	case <-time.After(time.Second):
		t.Error("a Restore asked for the lock once Release had begun, and waited for it")
	}
	select {
	case <-migrated:
		t.Fatalf("Migrate returned %v while SQLite's write lock was held", migrateErr)
	default:
	}

	letGo()
	<-migrated
	if migrateErr != nil {
		t.Errorf("Migrate: %v", migrateErr)
	}
	select {
	case err := <-released:
		if err != nil {
			t.Errorf("Release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("Release did not return once Migrate had")
	}
}

func TestLockIsExclusive(t *testing.T) {
	state := stateDir(t, t.TempDir())
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

	if _, err := store.AcquireLock(ctx, config.StateDir{}); err == nil || !strings.Contains(err.Error(), "which is a bug in Pisati") {
		t.Errorf("AcquireLock of the zero StateDir: %v; want it refused as a bug", err)
	}
}

// endsAfter is a context that ends, as its Err reports it, from the
// (n+1)th time it is asked on: a stop that comes just after a call's nth
// check of its context. Its Done never closes, so only those checks see it
// end, each at its own place in the call.
type endsAfter struct {
	context.Context //nolint:containedctx // it is a context: it wraps its parent, as the context package's own do
	left            atomic.Int64
}

func newEndsAfter(parent context.Context, n int64) *endsAfter {
	c := &endsAfter{Context: parent}
	c.left.Store(n)
	return c
}

func (c *endsAfter) Err() error {
	if c.left.Add(-1) < 0 {
		return context.Canceled
	}
	return c.Context.Err()
}

// targetWithLog returns a state directory whose database holds the
// session "live" in its write-ahead log alone, as a Pisati that stopped
// without closing it leaves it: the database is whole only with its log.
func targetWithLog(t *testing.T) target {
	t.Helper()
	origin := t.TempDir()
	path := store.DatabasePath(stateDir(t, origin))
	s := migratedStore(t, path)
	insertSession(t, s, "live", 1)
	tg := target{state: t.TempDir()}
	tg.path = store.DatabasePath(stateDir(t, tg.state))
	// Copied while s still has them open, so the log is not yet folded
	// into the file.
	for _, name := range []string{"pisati.db", "pisati.db-wal"} {
		if err := os.WriteFile(filepath.Join(tg.state, name), readFile(t, filepath.Join(origin, name)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return tg
}

// holds reports whether s holds the session name: false too when it holds
// no table of sessions at all, as a database whose log is lost may not.
func holds(t *testing.T, s *store.Store, name string) bool {
	t.Helper()
	_, err := s.Queries().GetSession(t.Context(), sessionID(name))
	switch {
	case err == nil:
		return true
	case errors.Is(err, sql.ErrNoRows), strings.Contains(err.Error(), "no such table: sessions"):
		return false
	}
	t.Fatalf("GetSession(%s): %v", name, err)
	return false
}

func TestRestoreLeavesOneWholeDatabase(t *testing.T) {
	// A stop after each check Restore makes of its context in turn, until
	// one comes after the last: each time the state directory holds the
	// old database, its log and all, or the whole of the new one, and
	// Restore says which.
	src := sourceWith(t, "restored")
	for n := int64(0); ; n++ {
		if n > 100 {
			t.Fatal("Restore still stopped after 100 checks of its context")
		}
		tg := targetWithLog(t)
		lock, err := store.AcquireLock(t.Context(), stateDir(t, tg.state))
		if err != nil {
			t.Fatal(err)
		}
		err = store.Restore(newEndsAfter(t.Context(), n), src, lock)
		if rerr := lock.Release(); rerr != nil {
			t.Fatal(rerr)
		}
		for _, name := range tg.files(t) {
			if strings.HasPrefix(name, "pisati.db.restore") {
				t.Errorf("stopped after %d checks: Restore left %s in the state directory", n, name)
			}
		}
		s := openStore(t, tg.path)
		live, restored := holds(t, s, "live"), holds(t, s, "restored")
		switch {
		case err == nil && (!restored || live):
			t.Errorf("stopped after %d checks: Restore succeeded, but the database holds the old session (%v) and "+
				"the new one (%v); want the new one alone", n, live, restored)
		case err != nil && (!errors.Is(err, context.Canceled) || !live || restored):
			t.Errorf("stopped after %d checks: Restore = %v, and the database holds the old session (%v) and the new "+
				"one (%v); want the stop, and the old database whole", n, err, live, restored)
		}
		if err == nil {
			return
		}
	}
}

func TestBackupStoppedLeavesNothing(t *testing.T) {
	// A stop after each check BackupTracked makes of its context in turn,
	// until one comes after the last: a stopped backup leaves nothing, and
	// one that returns nil leaves its whole copy.
	s := migratedStore(t, dbPath(t))
	insertSession(t, s, "kept", 1)
	for n := int64(0); ; n++ {
		if n > 100 {
			t.Fatal("the backup still stopped after 100 checks of its context")
		}
		dir := t.TempDir()
		dest := filepath.Join(dir, "backup.db")
		err := s.Backup(newEndsAfter(t.Context(), n), dest)
		got := listDir(t, dir)
		switch {
		case err != nil && (!errors.Is(err, context.Canceled) || len(got) != 0):
			t.Errorf("stopped after %d checks: Backup = %v, and left %v; want the stop, and nothing", n, err, got)
		case err == nil && !hasSession(t, openStore(t, dest), "kept"):
			t.Errorf("stopped after %d checks: Backup succeeded, but its copy lacks the session", n)
		}
		if err == nil {
			return
		}
	}
}
