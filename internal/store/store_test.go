// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package store_test

import (
	"crypto/sha256"
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/divbast/pisati/internal/store"
	"github.com/divbast/pisati/internal/store/storedb"
)

// dbPath returns where a test's database lives: pisati.db in a new
// state directory of its own.
func dbPath(t *testing.T) string {
	t.Helper()
	return store.DatabasePath(t.TempDir())
}

// lockOn takes the lock on the state directory dir until the test ends.
func lockOn(t *testing.T, dir string) store.Lock {
	t.Helper()
	l, err := store.AcquireLock(t.Context(), dir)
	if err != nil {
		t.Fatalf("AcquireLock(%s): %v", dir, err)
	}
	t.Cleanup(func() {
		if err := l.Release(); err != nil {
			t.Error(err)
		}
	})
	return l
}

// migrate migrates s, the database at path, under the lock on path's
// directory, which it takes for the call alone.
func migrate(t *testing.T, s *store.Store, path string) (from, to store.SchemaVersion, err error) {
	t.Helper()
	l, err := store.AcquireLock(t.Context(), filepath.Dir(path))
	if err != nil {
		t.Fatalf("AcquireLock(%s): %v", filepath.Dir(path), err)
	}
	from, to, err = s.Migrate(t.Context(), l)
	if rerr := l.Release(); rerr != nil {
		t.Fatal(rerr)
	}
	return from, to, err
}

// openStore opens the database at path, failing the test if it cannot,
// and closes it when the test ends.
func openStore(t *testing.T, path string, opts ...store.Option) *store.Store {
	t.Helper()
	s, err := store.Open(t.Context(), path, opts...)
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close(%s): %v", path, err)
		}
	})
	return s
}

// migratedStore opens the database at path and brings it to Pisati's
// schema.
func migratedStore(t *testing.T, path string) *store.Store {
	t.Helper()
	s := openStore(t, path)
	if _, _, err := migrate(t, s, path); err != nil {
		t.Fatalf("Migrate(%s): %v", path, err)
	}
	return s
}

// sessionID is a test's stand-in for the SHA-256 of a session ID.
func sessionID(name string) []byte {
	sum := sha256.Sum256([]byte(name))
	return sum[:]
}

// newSession is a session row for the ID name, last seen at lastSeen.
func newSession(name string, lastSeen int64) storedb.InsertSessionParams {
	return storedb.InsertSessionParams{
		IDHash:     sessionID(name),
		UserID:     sql.NullInt64{Int64: 7, Valid: true},
		CreatedAt:  1_700_000_000_000,
		LastSeenAt: lastSeen,
		ExpiresAt:  1_800_000_000_000,
	}
}

// insertSession commits the session name, last seen at lastSeen.
func insertSession(t *testing.T, s *store.Store, name string, lastSeen int64) {
	t.Helper()
	if err := s.Queries().InsertSession(t.Context(), newSession(name, lastSeen)); err != nil {
		t.Fatalf("InsertSession(%s): %v", name, err)
	}
}

// hasSession reports whether s holds the session name.
func hasSession(t *testing.T, s *store.Store, name string) bool {
	t.Helper()
	_, err := s.Queries().GetSession(t.Context(), sessionID(name))
	switch {
	case err == nil:
		return true
	case errors.Is(err, sql.ErrNoRows):
		return false
	}
	t.Fatalf("GetSession(%s): %v", name, err)
	return false
}

// listDir returns the names in the directory dir, sorted.
func listDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// readFile returns what the file at path holds, read through a directory
// FS as gosec prefers.
func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := fs.ReadFile(os.DirFS(filepath.Dir(path)), filepath.Base(path))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

// checksum returns the SHA-256 of the file at path.
func checksum(t *testing.T, path string) [sha256.Size]byte {
	t.Helper()
	return sha256.Sum256(readFile(t, path))
}

func TestOpenSetsPragmas(t *testing.T) {
	s := openStore(t, dbPath(t))
	ctx := t.Context()

	// Held at once, so that the pool opens a connection for each: every
	// one of them must have the settings, not only the first.
	const conns = 3
	for i := range conns {
		conn, err := s.DB().Conn(ctx)
		if err != nil {
			t.Fatalf("connection %d: %v", i, err)
		}
		t.Cleanup(func() {
			if err := conn.Close(); err != nil {
				t.Errorf("close connection %d: %v", i, err)
			}
		})
		for _, c := range []struct{ query, want string }{
			{"PRAGMA journal_mode", "wal"},
			{"PRAGMA foreign_keys", "1"},
			{"PRAGMA busy_timeout", "5000"},
			{"PRAGMA synchronous", "1"}, // NORMAL
		} {
			var got string
			if err := conn.QueryRowContext(ctx, c.query).Scan(&got); err != nil {
				t.Fatalf("connection %d: %s: %v", i, c.query, err)
			}
			if got != c.want {
				t.Errorf("connection %d: %s = %s, want %s", i, c.query, got, c.want)
			}
		}
	}
}

func TestOpenNamesUnopenablePath(t *testing.T) {
	dir := t.TempDir()
	for name, path := range map[string]string{
		"a directory that does not exist": filepath.Join(dir, "missing", "pisati.db"),
		"a directory, not a file":         dir,
		"a relative path":                 "pisati.db",
	} {
		t.Run(name, func(t *testing.T) {
			s, err := store.Open(t.Context(), path)
			if err == nil {
				t.Cleanup(func() {
					if err := s.Close(); err != nil {
						t.Error(err)
					}
				})
				t.Fatalf("Open(%s) succeeded, want an error", path)
			}
			if !strings.Contains(err.Error(), path) {
				t.Errorf("Open(%s): %v; want the error to name the path", path, err)
			}
		})
	}
}

func TestInTxRollsBackOnError(t *testing.T) {
	s := migratedStore(t, dbPath(t))

	t.Run("an error", func(t *testing.T) {
		ctx := t.Context()
		errStop := errors.New("stop")
		err := s.InTx(ctx, func(q *storedb.Queries) error {
			if err := q.InsertSession(ctx, newSession("refused", 1)); err != nil {
				return err
			}
			return errStop
		})
		if !errors.Is(err, errStop) {
			t.Fatalf("InTx: %v, want the error fn returned", err)
		}
		if hasSession(t, s, "refused") {
			t.Error("the session fn inserted before its error was committed")
		}
	})

	t.Run("a panic", func(t *testing.T) {
		ctx := t.Context()
		func() {
			defer func() {
				if recover() == nil {
					t.Error("InTx did not let fn's panic through")
				}
			}()
			err := s.InTx(ctx, func(q *storedb.Queries) error {
				if err := q.InsertSession(ctx, newSession("panicked", 1)); err != nil {
					return err
				}
				panic("fn panics")
			})
			t.Errorf("InTx returned %v; want fn's panic to go through", err)
		}()
		if hasSession(t, s, "panicked") {
			t.Error("the session fn inserted before it panicked was committed")
		}
		// Were the write lock still held, this would wait out the busy
		// timeout and fail.
		if err := s.InTx(ctx, func(q *storedb.Queries) error {
			return q.InsertSession(ctx, newSession("after", 1))
		}); err != nil {
			t.Fatalf("InTx after a panic: %v", err)
		}
	})

	t.Run("no error", func(t *testing.T) {
		ctx := t.Context()
		if err := s.InTx(ctx, func(q *storedb.Queries) error {
			return q.InsertSession(ctx, newSession("kept", 1))
		}); err != nil {
			t.Fatalf("InTx: %v", err)
		}
		if !hasSession(t, s, "kept") {
			t.Error("the session fn inserted was not committed")
		}
	})
}

// TestInTxConcurrentReadThenWrite runs transactions that each read a row
// and then write it, from many goroutines at once. They run on real
// goroutines, not in a synctest bubble: what is under test is SQLite's
// file locking, which waits in system calls a bubble cannot see.
func TestInTxConcurrentReadThenWrite(t *testing.T) {
	s := migratedStore(t, dbPath(t))
	ctx := t.Context()
	insertSession(t, s, "shared", 0)
	id := sessionID("shared")

	const goroutines, rounds = 16, 10
	errs := make(chan error, goroutines*rounds)
	var wg sync.WaitGroup
	for range goroutines {
		wg.Go(func() {
			for range rounds {
				errs <- s.InTx(ctx, func(q *storedb.Queries) error {
					got, err := q.GetSession(ctx, id)
					if err != nil {
						return err
					}
					n, err := q.TouchSession(ctx, storedb.TouchSessionParams{
						LastSeenAt:  got.LastSeenAt + 1,
						ExpiresAt:   got.ExpiresAt,
						IDHash:      id,
						TouchBefore: got.LastSeenAt,
					})
					if err == nil && n != 1 {
						err = errors.New("the touch's condition on what the transaction read did not hold")
					}
					return err
				})
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}

	got, err := s.Queries().GetSession(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	// Each transaction read the value the one before it wrote: none
	// overwrote another's update.
	if want := int64(goroutines * rounds); got.LastSeenAt != want {
		t.Errorf("after %d increments, last_seen_at = %d", want, got.LastSeenAt)
	}
}
