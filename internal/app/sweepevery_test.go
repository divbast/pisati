// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package app_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"testing"
	"testing/synctest"
	"time"

	"github.com/divbast/pisati/internal/app"
	"github.com/divbast/pisati/internal/store"
	"github.com/divbast/pisati/internal/store/storedb"
)

// sweepFailed is the message SweepEvery logs a failed sweep with.
const sweepFailed = "expired sessions not deleted"

// insertExpiring stores a session of user 1 that expires at expires, and
// returns its ID's hash.
func insertExpiring(t *testing.T, s *store.Store, expires time.Time) []byte {
	t.Helper()
	id := make([]byte, 32)
	if _, err := rand.Read(id); err != nil {
		t.Fatal(err)
	}
	now := ms(time.Now())
	if err := s.Queries().InsertSession(t.Context(), storedb.InsertSessionParams{
		IDHash:     id,
		UserID:     sql.NullInt64{Int64: 1, Valid: true},
		CreatedAt:  now,
		LastSeenAt: now,
		ExpiresAt:  ms(expires),
	}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}
	return id
}

// kept reports whether the session whose ID hashes to id is still stored.
func kept(t *testing.T, s *store.Store, id []byte) bool {
	t.Helper()
	var n int
	if err := s.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM sessions WHERE id_hash = ?`, id).Scan(&n); err != nil {
		t.Fatalf("look for the session: %v", err)
	}
	return n == 1
}

// sweepEvery runs SweepEvery until the stop function it returns is called,
// which cancels its context and returns its error.
func sweepEvery(t *testing.T, sessions *app.Sessions, every time.Duration) (stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- sessions.SweepEvery(ctx, every)
	}()
	return func() error {
		cancel()
		return <-done
	}
}

func TestSweepEvery(t *testing.T) {
	t.Run("at once, then every interval", func(t *testing.T) {
		inBubble(t, func(t *testing.T, s *store.Store) {
			sessions, logs := newLoggedSessions(t, s, time.Now, httpsBase)
			start := time.Now()
			expired := insertExpiring(t, s, start.Add(-time.Millisecond))
			second := insertExpiring(t, s, start.Add(12*time.Minute))
			third := insertExpiring(t, s, start.Add(25*time.Minute))

			stop := sweepEvery(t, sessions, 10*time.Minute)
			synctest.Wait()
			if kept(t, s, expired) {
				t.Error("the session that had expired before the start is still stored")
			}
			for _, step := range []struct {
				at       time.Duration
				id       []byte
				name     string
				goneHere bool
			}{
				{19*time.Minute + 59*time.Second, second, "the session expiring at 12 min", false},
				{20 * time.Minute, second, "the session expiring at 12 min", true},
				{29*time.Minute + 59*time.Second, third, "the session expiring at 25 min", false},
				{30 * time.Minute, third, "the session expiring at 25 min", true},
			} {
				time.Sleep(time.Until(start.Add(step.at)))
				synctest.Wait()
				if gone := !kept(t, s, step.id); gone != step.goneHere {
					t.Errorf("at %v: %s deleted: %t, want %t", step.at, step.name, gone, step.goneHere)
				}
			}
			if err := stop(); err != nil {
				t.Errorf("SweepEvery returned %v once its context ended, want nil", err)
			}
			wantLogged(t, logs, sweepFailed, "WARN", 0, "")
		})
	})

	t.Run("a failed sweep is logged, and the next runs on time", func(t *testing.T) {
		inBubble(t, func(t *testing.T, s *store.Store) {
			sessions, logs := newLoggedSessions(t, s, time.Now, httpsBase)
			exec(t, s, `DROP TABLE sessions`)
			start := time.Now()
			stop := sweepEvery(t, sessions, 10*time.Minute)
			synctest.Wait()
			wantLogged(t, logs, sweepFailed, "WARN", 1, "")
			time.Sleep(time.Until(start.Add(10*time.Minute - time.Second)))
			synctest.Wait()
			wantLogged(t, logs, sweepFailed, "WARN", 1, "")
			time.Sleep(time.Second)
			synctest.Wait()
			wantLogged(t, logs, sweepFailed, "WARN", 2, "")
			if err := stop(); err != nil {
				t.Errorf("SweepEvery returned %v once its context ended, want nil", err)
			}
		})
	})

	t.Run("a sweep its context ends is not logged", func(t *testing.T) {
		inBubble(t, func(t *testing.T, s *store.Store) {
			sessions, logs := newLoggedSessions(t, s, time.Now, httpsBase)
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if err := sessions.SweepEvery(ctx, 10*time.Minute); err != nil {
				t.Errorf("SweepEvery with an ended context returned %v, want nil", err)
			}
			wantLogged(t, logs, sweepFailed, "WARN", 0, "")
		})
	})

	t.Run("an interval that is not positive is refused", func(t *testing.T) {
		inBubble(t, func(t *testing.T, s *store.Store) {
			sessions := newSessions(t, s, time.Now, httpsBase)
			expired := insertExpiring(t, s, time.Now().Add(-time.Millisecond))
			for _, every := range []time.Duration{0, -time.Minute} {
				if err := sessions.SweepEvery(t.Context(), every); err == nil {
					t.Errorf("SweepEvery(%v) returned no error", every)
				}
			}
			if !kept(t, s, expired) {
				t.Error("a refused SweepEvery deleted a session")
			}
		})
	})

	t.Run("Sessions NewSessions did not make", func(t *testing.T) {
		for name, sessions := range map[string]*app.Sessions{"nil": nil, "zero": {}} {
			if err := sessions.SweepEvery(t.Context(), time.Minute); err == nil {
				t.Errorf("%s Sessions: SweepEvery returned no error", name)
			}
		}
	})
}
