// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package store_test

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
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
	dest := store.DatabasePath(t.TempDir())

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
