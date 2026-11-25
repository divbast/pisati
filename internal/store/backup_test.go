// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package store_test

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/divbast/pisati/internal/store"
	"github.com/divbast/pisati/internal/store/storedb"
)

func TestBackupRoundTrip(t *testing.T) {
	s := migratedStore(t, dbPath(t))
	ctx := t.Context()
	insertSession(t, s, "committed", 42)
	// Named as a state directory's database, so that it can be migrated
	// under that directory's lock.
	dest := store.DatabasePath(stateDir(t, t.TempDir()))

	// A transaction open while the backup runs: the backup neither waits
	// for it nor holds what it has not committed.
	inTx, backedUp := make(chan struct{}), make(chan struct{})
	txErr := make(chan error, 1)
	go func() {
		txErr <- s.InTx(ctx, func(q *storedb.Queries) error {
			if err := q.InsertSession(ctx, newSession("uncommitted", 1)); err != nil {
				close(inTx)
				return err
			}
			close(inTx)
			<-backedUp
			return nil
		})
	}()
	select {
	case <-inTx:
	case err := <-txErr:
		t.Fatalf("InTx ended before the backup: %v", err)
	}
	err := s.Backup(ctx, dest)
	close(backedUp)
	if err := errors.Join(err, <-txErr); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("backup mode = %v, want -rw-------", mode)
	}
	if got := listDir(t, filepath.Dir(dest)); !slices.Equal(got, []string{"pisati.db"}) {
		t.Errorf("backup directory = %v, want the one file", got)
	}
	b := openStore(t, dest)
	if from, to, err := migrate(t, b, dest); err != nil || from != newestVersion || to != newestVersion {
		t.Errorf("Migrate on the backup = %d, %d, %v; want the schema at %d", from, to, err, newestVersion)
	}
	got, err := b.Queries().GetSession(ctx, sessionID("committed"))
	if err != nil {
		t.Fatalf("the backup lacks the committed session: %v", err)
	}
	if got.LastSeenAt != 42 {
		t.Errorf("backed-up session last_seen_at = %d, want 42", got.LastSeenAt)
	}
	if hasSession(t, b, "uncommitted") {
		t.Error("the backup holds a session committed after it began")
	}
}

func TestBackupRefusesExisting(t *testing.T) {
	s := migratedStore(t, dbPath(t))
	// A write-ahead log left without its database counts as well: SQLite
	// would read it as the new copy's own.
	for _, existing := range []string{"backup.db", "backup.db-wal"} {
		t.Run(existing, func(t *testing.T) {
			dir := t.TempDir()
			dest := filepath.Join(dir, "backup.db")
			keep := []byte("an earlier backup")
			if err := os.WriteFile(filepath.Join(dir, existing), keep, 0o600); err != nil {
				t.Fatal(err)
			}

			err := s.Backup(t.Context(), dest)
			if !errors.Is(err, fs.ErrExist) {
				t.Fatalf("Backup beside an existing %s: %v, want fs.ErrExist", existing, err)
			}
			if got := listDir(t, dir); !slices.Equal(got, []string{existing}) {
				t.Errorf("directory after the refused backup = %v, want only %s", got, existing)
			}
			got, err := fs.ReadFile(os.DirFS(dir), existing)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, keep) {
				t.Error("Backup changed the file it refused")
			}
		})
	}
}

// TestBackupRefusesUncheckableSidecar: a file beside the copy whose
// presence Pisati cannot check is refused as one that exists, and said to
// be unknown. A name of 250 bytes is one the system takes, while its
// rollback journal's, 258 bytes, is too long to look up.
func TestBackupRefusesUncheckableSidecar(t *testing.T) {
	s := migratedStore(t, dbPath(t))
	dir := t.TempDir()
	name := strings.Repeat("b", 250)
	err := s.Backup(t.Context(), filepath.Join(dir, name))
	if !errors.Is(err, fs.ErrExist) {
		t.Fatalf("Backup to a name whose journal's is too long: %v, want fs.ErrExist", err)
	}
	journal := filepath.Join(dir, name+"-journal")
	if want := "Pisati cannot tell whether the file " + journal + " is there"; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not say %q", err, want)
	}
	if got := listDir(t, dir); len(got) != 0 {
		t.Errorf("directory after the refused backup = %v, want it empty", got)
	}
}

// backup writes a copy of s to name in a new directory, and returns its
// path.
func backup(t *testing.T, s *store.Store, name string) string {
	t.Helper()
	dest := filepath.Join(t.TempDir(), name)
	if err := s.Backup(t.Context(), dest); err != nil {
		t.Fatalf("Backup(%s): %v", dest, err)
	}
	return dest
}

func TestBackupTracked(t *testing.T) {
	s := migratedStore(t, dbPath(t))
	t.Run("made first, and the finished copy removed", func(t *testing.T) {
		dir := t.TempDir()
		dest := filepath.Join(dir, "backup.db")
		var remove func() error
		calls := 0
		if err := s.BackupTracked(t.Context(), dest, func(r func() error) {
			calls++
			if _, err := os.Lstat(dest); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("made was called once the copy existed (%v), want it called before anything", err)
			}
			remove = r
		}); err != nil {
			t.Fatal(err)
		}
		if calls != 1 || remove == nil {
			t.Fatalf("made was called %d times, want once with a remover", calls)
		}
		// A journal SQLite left beside the copy is the copy's own too.
		if err := os.WriteFile(dest+"-journal", []byte("a journal"), 0o600); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if err := remove(); err != nil {
				t.Errorf("remove: %v, want nil, the second time too", err)
			}
		}
		if got := listDir(t, dir); len(got) != 0 {
			t.Errorf("directory after remove = %v, want nothing", got)
		}
	})
	t.Run("removed from the directory it was made in", func(t *testing.T) {
		parent := t.TempDir()
		dir := filepath.Join(parent, "backups")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		dest := filepath.Join(dir, "backup.db")
		var remove func() error
		if err := s.BackupTracked(t.Context(), dest, func(r func() error) { remove = r }); err != nil {
			t.Fatal(err)
		}
		// The directory moved, and another put in its place, holding a file
		// of the copy's name that is not the copy.
		moved := filepath.Join(parent, "moved")
		if err := os.Rename(dir, moved); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dest, []byte("another file"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := remove(); err != nil {
			t.Fatalf("remove: %v", err)
		}
		if got := listDir(t, moved); len(got) != 0 {
			t.Errorf("the copy's own directory after remove = %v, want nothing", got)
		}
		if got := string(readFile(t, dest)); got != "another file" {
			t.Errorf("remove changed the file now at the copy's path: it holds %q", got)
		}
	})
	t.Run("removed before it is made: nothing is made", func(t *testing.T) {
		dir := t.TempDir()
		dest := filepath.Join(dir, "backup.db")
		var removed error
		err := s.BackupTracked(t.Context(), dest, func(r func() error) { removed = r() })
		if err == nil || removed != nil {
			t.Errorf("BackupTracked after remove = %v, and remove = %v; want an error, and remove nil", err, removed)
		}
		if got := listDir(t, dir); len(got) != 0 {
			t.Errorf("directory after a backup removed before it began = %v, want nothing", got)
		}
	})
	t.Run("refused: an earlier file at dest stays", func(t *testing.T) {
		dir := t.TempDir()
		dest := filepath.Join(dir, "backup.db")
		if err := os.WriteFile(dest, []byte("an earlier backup"), 0o600); err != nil {
			t.Fatal(err)
		}
		var remove func() error
		err := s.BackupTracked(t.Context(), dest, func(r func() error) { remove = r })
		if !errors.Is(err, fs.ErrExist) {
			t.Errorf("BackupTracked over an existing file: %v, want fs.ErrExist", err)
		}
		if err := remove(); err != nil {
			t.Errorf("remove of a copy never made: %v, want nil", err)
		}
		if got := string(readFile(t, dest)); got != "an earlier backup" {
			t.Errorf("the earlier file at dest holds %q after remove, want it as it was", got)
		}
	})
	t.Run("stopped: the copy goes", func(t *testing.T) {
		dir := t.TempDir()
		dest := filepath.Join(dir, "backup.db")
		ctx, cancel := context.WithCancel(t.Context())
		var remove func() error
		err := s.BackupTracked(ctx, dest, func(r func() error) {
			remove = r
			// Ended before the copy is made: BackupTracked makes it all
			// the same, then stops, as its context says.
			cancel()
		})
		if !errors.Is(err, context.Canceled) {
			t.Errorf("BackupTracked with its context ended: %v, want context.Canceled", err)
		}
		if err := remove(); err != nil {
			t.Errorf("remove once BackupTracked removed the copy itself: %v, want nil", err)
		}
		if got := listDir(t, dir); len(got) != 0 {
			t.Errorf("directory after a stopped backup = %v, want nothing", got)
		}
	})
}
