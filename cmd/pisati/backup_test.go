// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/divbast/pisati/internal/store"
	"github.com/divbast/pisati/internal/store/storedb"
)

// liveSession stores a session in the database at state that expires in
// an hour, and returns its ID's hash.
func liveSession(t *testing.T, state string) []byte {
	t.Helper()
	id := make([]byte, 32)
	if _, err := rand.Read(id); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	withStore(t, state, func(db *store.Store) {
		if err := db.Queries().InsertSession(t.Context(), storedb.InsertSessionParams{
			IDHash:     id,
			UserID:     sql.NullInt64{Int64: 1, Valid: true},
			CreatedAt:  now.UnixMilli(),
			LastSeenAt: now.UnixMilli(),
			ExpiresAt:  now.Add(time.Hour).UnixMilli(),
		}); err != nil {
			t.Fatal(err)
		}
	})
	return id
}

// dataKeys returns how many data keys the database at state holds.
func dataKeys(t *testing.T, state string) int {
	t.Helper()
	n := 0
	withStore(t, state, func(db *store.Store) {
		n = count(t, db, `SELECT count(*) FROM data_keys`)
	})
	return n
}

func TestBackupRestoreCommands(t *testing.T) {
	t.Run("no database to back up", func(t *testing.T) {
		s := newSite(t)
		dest := filepath.Join(t.TempDir(), "copy.db")
		r := s.run(t, "backup", dest)
		want := "pisati: there is no database to back up at " + filepath.Join(s.state, "pisati.db") + ". Nothing was written."
		if r.code != 1 || r.line(t) != want || r.stdout != "" {
			t.Errorf("pisati backup = %d, %q, %q; want 1, %q", r.code, r.stdout, r.stderr, want)
		}
		if entries, err := os.ReadDir(s.state); err != nil || len(entries) != 0 {
			t.Errorf("the state directory holds %v (%v), want nothing", entries, err)
		}
		if _, err := os.Lstat(dest); !os.IsNotExist(err) {
			t.Errorf("backup wrote %s: %v", dest, err)
		}
	})

	t.Run("back up while serving, restore once stopped", func(t *testing.T) {
		s := newSite(t)
		v := s.serve(t)

		// Relative paths are resolved against the working directory.
		copies := t.TempDir()
		t.Chdir(copies)
		if r := s.mustRun(t, "backup", "copy.db"); r.stdout != "" || r.stderr != "" {
			t.Errorf("pisati backup wrote %q and %q, want nothing", r.stdout, r.stderr)
		}
		info, err := os.Stat(filepath.Join(copies, "copy.db"))
		if err != nil {
			t.Fatalf("the backup is not where its relative path names: %v", err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("the backup has mode %04o, want 0600", perm)
		}

		// A session made after the backup would be gone, had the restore
		// gone ahead.
		later := liveSession(t, s.state)
		r := s.run(t, "restore", "copy.db")
		if want := lockedLine(s.state); r.code != 1 || r.line(t) != want {
			t.Errorf("pisati restore while serving = %d, %q; want 1, %q", r.code, r.stderr, want)
		}
		withStore(t, s.state, func(db *store.Store) {
			if n := count(t, db, `SELECT count(*) FROM sessions WHERE id_hash = ?`, later); n != 1 {
				t.Error("the refused restore changed the database: the session made after the backup is gone")
			}
		})
		if code := v.stop(); code != 0 {
			t.Fatalf("serve exited %d", code)
		}

		// A second data key, made after the backup, is gone once it is
		// restored.
		s.mustRun(t, "keys", "rotate")
		if n := dataKeys(t, s.state); n != 2 {
			t.Fatalf("%d data keys after the rotation, want 2", n)
		}
		if r := s.mustRun(t, "restore", "copy.db"); r.stdout != "" || r.stderr != "" {
			t.Errorf("pisati restore wrote %q and %q, want nothing", r.stdout, r.stderr)
		}
		if n := dataKeys(t, s.state); n != 1 {
			t.Errorf("%d data keys after the restore, want the backup's 1", n)
		}
	})
}

func TestBackupStopped(t *testing.T) {
	// No connection can open the database while another holds this lock,
	// so the backup waits, as a slow one would, when the signal comes.
	for _, tc := range []struct {
		name string
		stop func(cancel func(), halt func())
		want string
	}{
		{
			"by a first signal", func(cancel, _ func()) { cancel() },
			"pisati: the backup stopped before it finished, because a SIGTERM or SIGINT signal arrived. " +
				"No backup was made, and nothing of it is left at %s.",
		},
		{
			"by a second signal", func(_, halt func()) { halt() },
			"pisati: the backup stopped at once, before it finished, because a second SIGTERM or SIGINT signal " +
				"arrived. No backup was made, and nothing of it is left at %s.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSite(t)
			s.mustRun(t, "migrate")
			release := holdReadLock(t, s.state)
			dir := t.TempDir()
			dest := filepath.Join(dir, "copy.db")
			h, stdout, stderr := s.host("backup", dest)
			halted := make(chan struct{})
			h.Halt = halted
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan int, 1)
			go func() { done <- run(ctx, h) }()
			time.Sleep(500 * time.Millisecond)
			stopping := time.Now()
			tc.stop(cancel, func() { close(halted) })
			var code int
			select {
			case code = <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("pisati backup did not stop within 10 s")
			}
			took := time.Since(stopping)
			r := result{code: code, stdout: stdout.String(), stderr: stderr.String()}
			if want := fmt.Sprintf(tc.want, dest); r.code != 1 || r.line(t) != want || r.stdout != "" || took >= time.Second {
				t.Errorf("pisati backup stopped after %v = %d, %q, %q; want 1, %q, at once", took, r.code, r.stdout, r.stderr, want)
			}
			// Once the database is free again, the backup does not go on.
			release()
			settle(t)
			if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
				t.Errorf("the backup's directory holds %v (%v), want nothing", entries, err)
			}
		})
	}
}
