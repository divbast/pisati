// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package app_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/divbast/pisati/internal/app"
	"github.com/divbast/pisati/internal/config"
	"github.com/divbast/pisati/internal/core/auth"
	"github.com/divbast/pisati/internal/store"
	"github.com/divbast/pisati/internal/store/storedb"
)

// thirtyDays is a new session cookie's Max-Age: its absolute lifetime, in
// seconds.
const thirtyDays = 30 * 24 * 60 * 60

func TestNewSessionCookie(t *testing.T) {
	for _, tc := range []struct {
		base, name string
		secure     bool
	}{
		{base: httpsBase, name: "__Host-pisati", secure: true},
		{base: "https://localhost:8443", name: "__Host-pisati", secure: true},
		{base: localBase, name: "pisati", secure: false},
		{base: "http://127.0.0.1:8080", name: "pisati", secure: false},
		{base: "http://[::1]:8080", name: "pisati", secure: false},
	} {
		t.Run(tc.base, func(t *testing.T) {
			s, dir := newStore(t)
			sessions := newSessions(t, s, time.Now, tc.base)
			c := renew(t, sessions, 7)

			if c.Name != tc.name || c.Path != "/" || c.Domain != "" || c.Secure != tc.secure ||
				!c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.MaxAge != thirtyDays ||
				!c.Expires.IsZero() || c.Partitioned {
				t.Errorf("Renew's cookie = %+v, want %s, Path=/, no Domain, Secure %t, HttpOnly, SameSite=Lax, Max-Age %d and nothing else",
					c, tc.name, tc.secure, thirtyDays)
			}
			want := tc.name + "=" + c.Value + "; Path=/; Max-Age=2592000; HttpOnly; SameSite=Lax"
			if tc.secure {
				want = tc.name + "=" + c.Value + "; Path=/; Max-Age=2592000; HttpOnly; Secure; SameSite=Lax"
			}
			if got := c.String(); got != want {
				t.Errorf("Set-Cookie: %s, want %s", got, want)
			}
			if name := sessions.CookieName(); name != tc.name {
				t.Errorf("CookieName() = %q, want %q", name, tc.name)
			}
			wantUser(t, sessions, c.Value, 7, "just made")

			// Only the ID's SHA-256 reaches the database: not the ID, and
			// not the cookie value that spells it, in any file SQLite
			// writes.
			id := idOf(t, c.Value)
			if r := mustRow(t, s, c.Value, "just made"); !r.user.Valid || r.user.Int64 != 7 {
				t.Errorf("row's user_id = %+v, want 7", r.user)
			}
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := root.Close(); err != nil {
					t.Error(err)
				}
			})
			files, err := fs.Glob(root.FS(), "pisati.db*")
			if err != nil || len(files) == 0 {
				t.Fatalf("the database files: %v, %v", files, err)
			}
			for _, f := range files {
				content, err := root.ReadFile(f)
				if err != nil {
					t.Fatal(err)
				}
				if bytes.Contains(content, id) {
					t.Errorf("%s holds the raw session ID", f)
				}
				if bytes.Contains(content, []byte(c.Value)) {
					t.Errorf("%s holds the session cookie's value", f)
				}
			}
		})
	}
}

func TestNewSessionsRefuses(t *testing.T) {
	s, _ := newStore(t)
	base := baseURL(t, httpsBase)
	for _, tc := range []struct {
		name  string
		store *store.Store
		clock func() time.Time
		rnd   io.Reader
		base  config.BaseURL
	}{
		{name: "nil store", clock: time.Now, rnd: rand.Reader, base: base},
		{name: "zero store", store: &store.Store{}, clock: time.Now, rnd: rand.Reader, base: base},
		{name: "nil clock", store: s, rnd: rand.Reader, base: base},
		{name: "nil random source", store: s, clock: time.Now, base: base},
		{name: "zero base URL", store: s, clock: time.Now, rnd: rand.Reader},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := app.NewSessions(tc.store, tc.clock, tc.rnd, tc.base)
			if err == nil || got != nil {
				t.Errorf("NewSessions = %v, %v; want nil and an error", got, err)
			}
		})
	}

	t.Run("the zero Sessions", func(t *testing.T) {
		var zero app.Sessions
		ctx := t.Context()
		if _, _, err := zero.Load(ctx, strings.Repeat("A", 43)); err == nil {
			t.Error("Load on the zero Sessions: no error")
		}
		if _, err := zero.Renew(ctx, 1); err == nil {
			t.Error("Renew on the zero Sessions: no error")
		}
		if _, err := zero.Destroy(ctx, strings.Repeat("A", 43)); err == nil {
			t.Error("Destroy on the zero Sessions: no error")
		}
		if err := zero.DestroyAllForUser(ctx, 1); err == nil {
			t.Error("DestroyAllForUser on the zero Sessions: no error")
		}
		if _, err := zero.Sweep(ctx); err == nil {
			t.Error("Sweep on the zero Sessions: no error")
		}
	})
}

func TestRenewRefusesWithoutRandomness(t *testing.T) {
	s, _ := newStore(t)
	for name, rnd := range map[string]io.Reader{
		"failing": failingRand{},
		"short":   &shortRand{n: 31},
	} {
		t.Run(name, func(t *testing.T) {
			sessions, err := app.NewSessions(s, time.Now, rnd, baseURL(t, httpsBase))
			if err != nil {
				t.Fatal(err)
			}
			if c, err := sessions.Renew(t.Context(), 1); err == nil {
				t.Errorf("Renew = %v, nil; want an error", c)
			}
			if n := countSessions(t, s); n != 0 {
				t.Errorf("%d sessions stored, want 0", n)
			}
		})
	}
}

func TestLoadRefusesMalformedValues(t *testing.T) {
	s, _ := newStore(t)
	sessions := newSessions(t, s, time.Now, httpsBase)
	good := renew(t, sessions, 3).Value

	// One spelling for each ID: the last character's two unused bits are
	// zero. Setting one spells the same 32 bytes, which Load refuses.
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	unused := good[:42] + string(alphabet[strings.IndexByte(alphabet, good[42])|1])

	for name, value := range map[string]string{
		"empty":                "",
		"one short":            good[:42],
		"one long":             good + "A",
		"padded":               good + "=",
		"standard alphabet":    strings.NewReplacer("-", "+", "_", "/").Replace(strings.Repeat("-_", 21)) + "A",
		"space":                good[:42] + " ",
		"unused bits set":      unused,
		"64 bytes in base64":   base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 64)),
		"the raw ID's SHA-256": base64.RawURLEncoding.EncodeToString(hashOf(t, good)),
	} {
		t.Run(name, func(t *testing.T) {
			wantNoSession(t, sessions, value, name)
		})
	}
	wantUser(t, sessions, good, 3, "the good value")
}

func TestRenewRotates(t *testing.T) {
	s, _ := newStore(t)
	sessions := newSessions(t, s, time.Now, httpsBase)

	seen := map[string]bool{}
	var values []string
	for range 10 {
		c := renew(t, sessions, 5)
		if seen[c.Value] {
			t.Fatalf("Renew returned the same ID twice")
		}
		seen[c.Value] = true
		values = append(values, c.Value)
	}
	for _, v := range values {
		wantUser(t, sessions, v, 5, "each renewed")
	}

	gone, err := sessions.Destroy(t.Context(), values[0])
	if err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	want := "__Host-pisati=; Path=/; Max-Age=0; HttpOnly; Secure; SameSite=Lax"
	if got := gone.String(); got != want || gone.MaxAge >= 0 {
		t.Errorf("Destroy's cookie: %s (MaxAge %d), want %s", got, gone.MaxAge, want)
	}
	cleared := sessions.Clear()
	if got := cleared.String(); got != want {
		t.Errorf("Clear's cookie: %s, want %s", got, want)
	}
	wantNoSession(t, sessions, values[0], "destroyed")
	if _, ok := rowOf(t, s, values[0]); ok {
		t.Error("the destroyed session's row is still there")
	}
	for _, v := range values[1:] {
		wantUser(t, sessions, v, 5, "after another was destroyed")
	}

	// Destroying what is no session still expires the cookie.
	for _, v := range []string{values[0], "not a session", ""} {
		c, err := sessions.Destroy(t.Context(), v)
		if err != nil || c.String() != want {
			t.Errorf("Destroy(no session) = %s, %v; want %s, nil", c.String(), err, want)
		}
	}

	for _, user := range []auth.UserID{0, -1} {
		if c, err := sessions.Renew(t.Context(), user); err == nil {
			t.Errorf("Renew(%d) = %v, nil; want an error", user, c)
		}
	}
}

func TestDestroyAllForUser(t *testing.T) {
	s, _ := newStore(t)
	sessions := newSessions(t, s, time.Now, httpsBase)
	first, second := renew(t, sessions, 1), renew(t, sessions, 1)
	other := renew(t, sessions, 2)

	if err := sessions.DestroyAllForUser(t.Context(), 1); err != nil {
		t.Fatalf("DestroyAllForUser(1): %v", err)
	}
	wantNoSession(t, sessions, first.Value, "user 1's first")
	wantNoSession(t, sessions, second.Value, "user 1's second")
	wantUser(t, sessions, other.Value, 2, "user 2's")
	if n := countSessions(t, s); n != 1 {
		t.Errorf("%d sessions left, want 1", n)
	}

	// A user with no sessions has none to destroy.
	if err := sessions.DestroyAllForUser(t.Context(), 3); err != nil {
		t.Errorf("DestroyAllForUser(3): %v", err)
	}
	for _, user := range []auth.UserID{0, -1} {
		if err := sessions.DestroyAllForUser(t.Context(), user); err == nil {
			t.Errorf("DestroyAllForUser(%d): no error", user)
		}
	}
	wantUser(t, sessions, other.Value, 2, "user 2's, at the end")
}

func TestIdleAndAbsoluteExpiry(t *testing.T) {
	t.Run("idle", func(t *testing.T) {
		inBubble(t, func(t *testing.T, s *store.Store) {
			sessions := newSessions(t, s, time.Now, httpsBase)
			start := time.Now()
			idle, used := renew(t, sessions, 4), renew(t, sessions, 4)
			if r := mustRow(t, s, idle.Value, "new"); r.created != ms(start) || r.lastSeen != ms(start) ||
				r.expiresAt != ms(start.Add(auth.IdleLimit)) {
				t.Errorf("new row: %+v, want created and last seen at %d, expiring %d",
					r, ms(start), ms(start.Add(auth.IdleLimit)))
			}

			time.Sleep(auth.IdleLimit - time.Millisecond)
			wantUser(t, sessions, used.Value, 4, "a millisecond short of the idle limit")
			touched := time.Now()
			time.Sleep(time.Millisecond)
			wantNoSession(t, sessions, idle.Value, "at the idle limit")
			if _, ok := rowOf(t, s, idle.Value); ok {
				t.Error("Load left the idle session's row")
			}
			wantUser(t, sessions, used.Value, 4, "used a millisecond ago")

			time.Sleep(time.Until(touched.Add(auth.IdleLimit)) - time.Millisecond)
			wantUser(t, sessions, used.Value, 4, "a millisecond short of its next idle limit")
		})
	})

	t.Run("absolute", func(t *testing.T) {
		inBubble(t, func(t *testing.T, s *store.Store) {
			sessions := newSessions(t, s, time.Now, httpsBase)
			start := time.Now()
			c := renew(t, sessions, 9)
			for range 4 {
				time.Sleep(6 * 24 * time.Hour)
				wantUser(t, sessions, c.Value, 9, "used every six days")
				// The row expires when the first limit comes: a week after
				// it was last seen, but never past 30 days.
				now := time.Now()
				want := min(ms(start.Add(auth.AbsoluteLimit)), ms(now.Add(auth.IdleLimit)))
				if r := mustRow(t, s, c.Value, "touched"); r.lastSeen != ms(now) || r.expiresAt != want {
					t.Errorf("touched row: %+v, want last seen at %d, expiring %d", r, ms(now), want)
				}
			}
			time.Sleep(time.Until(start.Add(auth.AbsoluteLimit)) - time.Millisecond)
			wantUser(t, sessions, c.Value, 9, "a millisecond short of 30 days")
			time.Sleep(time.Millisecond)
			wantNoSession(t, sessions, c.Value, "30 days old, though used a day ago")
			if _, ok := rowOf(t, s, c.Value); ok {
				t.Error("Load left the expired session's row")
			}
		})
	})
}

func TestLoadTouchesOncePerMinute(t *testing.T) {
	inBubble(t, func(t *testing.T, s *store.Store) {
		sessions := newSessions(t, s, time.Now, httpsBase)
		start := time.Now()
		c := renew(t, sessions, 2)

		wantSeen := func(at time.Time, when string) {
			t.Helper()
			r := mustRow(t, s, c.Value, when)
			if r.lastSeen != ms(at) || r.expiresAt != ms(at.Add(auth.IdleLimit)) {
				t.Errorf("%s: row last seen at %+d ms, expiring at %+d ms; want %+d and %+d", when,
					r.lastSeen-ms(start), r.expiresAt-ms(start), ms(at)-ms(start), ms(at.Add(auth.IdleLimit))-ms(start))
			}
		}

		time.Sleep(time.Second)
		wantUser(t, sessions, c.Value, 2, "a second on")
		wantSeen(start, "a second on")
		time.Sleep(time.Minute - time.Second - time.Millisecond)
		wantUser(t, sessions, c.Value, 2, "a millisecond short of a minute")
		wantSeen(start, "a millisecond short of a minute")
		time.Sleep(time.Millisecond)
		wantUser(t, sessions, c.Value, 2, "a minute on")
		wantSeen(start.Add(time.Minute), "a minute on")
		for range 3 {
			time.Sleep(15 * time.Second)
			wantUser(t, sessions, c.Value, 2, "within the minute after the touch")
		}
		wantSeen(start.Add(time.Minute), "45 s after the touch")
		time.Sleep(15 * time.Second)
		wantUser(t, sessions, c.Value, 2, "a minute after the touch")
		wantSeen(start.Add(2*time.Minute), "a minute after the touch")
	})
}

func TestSweep(t *testing.T) {
	inBubble(t, func(t *testing.T, s *store.Store) {
		ctx := t.Context()
		sessions := newSessions(t, s, time.Now, httpsBase)
		start := time.Now()
		var old []string
		for range 3 {
			old = append(old, renew(t, sessions, 1).Value)
		}
		time.Sleep(24 * time.Hour)
		var young []string
		for range 2 {
			young = append(young, renew(t, sessions, 2).Value)
		}

		sweep := func(want int, when string) {
			t.Helper()
			n, err := sessions.Sweep(ctx)
			if err != nil {
				t.Fatalf("%s: Sweep: %v", when, err)
			}
			if n != want {
				t.Errorf("%s: Sweep removed %d sessions, want %d", when, n, want)
			}
		}

		time.Sleep(time.Until(start.Add(auth.IdleLimit)) - time.Millisecond)
		sweep(0, "a millisecond before the first expire")
		if n := countSessions(t, s); n != 5 {
			t.Errorf("%d sessions, want 5", n)
		}
		time.Sleep(time.Millisecond)
		sweep(3, "as the first expire")
		for _, v := range old {
			if _, ok := rowOf(t, s, v); ok {
				t.Error("an expired session's row is still there")
			}
		}
		for _, v := range young {
			mustRow(t, s, v, "a day younger")
		}

		// More than one batch's worth, each deleted in a transaction of
		// its own.
		const many = 1203
		if err := s.InTx(ctx, func(q *storedb.Queries) error {
			for i := range many {
				id := make([]byte, 32)
				if _, err := rand.Read(id); err != nil {
					return err
				}
				if err := q.InsertSession(ctx, storedb.InsertSessionParams{
					IDHash:     id,
					UserID:     sql.NullInt64{Int64: 3, Valid: true},
					CreatedAt:  ms(start),
					LastSeenAt: ms(start),
					ExpiresAt:  ms(time.Now()) - int64(i),
				}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		sweep(many, "past many")
		sweep(0, "again")
		if n := countSessions(t, s); n != 2 {
			t.Errorf("%d sessions, want the 2 younger", n)
		}
		for _, v := range young {
			wantUser(t, sessions, v, 2, "a day younger, after the sweeps")
		}
	})
}

// TestSessionsConcurrent has many requests load, renew and destroy
// sessions at once, each load touching its session, while sweeps run:
// every one of them writes, and none may fail with SQLITE_BUSY.
func TestSessionsConcurrent(t *testing.T) {
	s, _ := newStore(t)
	var clockMillis atomic.Int64
	clockMillis.Store(time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC).UnixMilli())
	// Each reading of the clock is 61 s after the last, so every load
	// finds a touch due. The 1,500 or so readings span about a day.
	clock := func() time.Time {
		return time.UnixMilli(clockMillis.Add(61_000))
	}
	sessions := newSessions(t, s, clock, httpsBase)
	ctx := t.Context()

	// The sweeps read a clock of their own, which stays at the start, so
	// that they neither move the requests' clock nor find a session
	// expired.
	start := time.UnixMilli(clockMillis.Load())
	sweeper := newSessions(t, s, func() time.Time { return start }, httpsBase)

	const goroutines, loads = 50, 20
	var requests, sweeping sync.WaitGroup
	done := make(chan struct{})
	sweeping.Go(func() {
		for {
			select {
			case <-done:
				return
			case <-time.After(time.Millisecond):
			}
			if _, err := sweeper.Sweep(ctx); err != nil {
				t.Errorf("Sweep: %v", err)
				return
			}
		}
	})
	for g := range goroutines {
		user := auth.UserID(g + 1)
		requests.Go(func() {
			c, err := sessions.Renew(ctx, user)
			if err != nil {
				t.Errorf("user %d: Renew: %v", user, err)
				return
			}
			for i := range loads {
				sess, ok, err := sessions.Load(ctx, c.Value)
				if err != nil {
					t.Errorf("user %d: Load %d: %v", user, i, err)
					return
				}
				if got, signedIn := sess.User(); !ok || !signedIn || got != user {
					t.Errorf("user %d: Load %d = user %d (%t, %t)", user, i, got, ok, signedIn)
					return
				}
				// Every fifth request signs in again: a new session, and
				// the old one destroyed. The last two loads touch the
				// last session.
				if i%5 == 2 {
					next, err := sessions.Renew(ctx, user)
					if err != nil {
						t.Errorf("user %d: Renew %d: %v", user, i, err)
						return
					}
					if _, err := sessions.Destroy(ctx, c.Value); err != nil {
						t.Errorf("user %d: Destroy %d: %v", user, i, err)
						return
					}
					c = next
				}
			}
		})
	}
	requests.Wait()
	close(done)
	sweeping.Wait()
	if n := countSessions(t, s); n != goroutines {
		t.Errorf("%d sessions left, want one for each of %d users", n, goroutines)
	}
	var untouched int
	if err := s.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM sessions WHERE last_seen_at <= created_at`).Scan(&untouched); err != nil {
		t.Fatal(err)
	}
	if untouched != 0 {
		t.Errorf("%d sessions never touched, want every one touched by its loads", untouched)
	}
}

// fakeClock is a clock a test sets, safe for concurrent use.
type fakeClock struct {
	millis atomic.Int64
}

func newFakeClock(at time.Time) *fakeClock {
	c := &fakeClock{}
	c.set(at)
	return c
}

func (c *fakeClock) now() time.Time   { return time.UnixMilli(c.millis.Load()) }
func (c *fakeClock) set(at time.Time) { c.millis.Store(at.UnixMilli()) }

// holdWriteLock begins a write transaction on s, which takes the
// database's write lock at once and keeps it until the test commits or
// rolls it back, or ends.
func holdWriteLock(t *testing.T, s *store.Store) *sql.Tx {
	t.Helper()
	tx, err := s.DB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			t.Error(err)
		}
	})
	return tx
}

// TestLoadWithoutTouchDueTakesNoWriteLock checks a load writes only when a
// touch is due: under WAL a reader never waits for a writer, so a load
// that does not write is served while another holds the write lock. That
// is what writing at most once a minute buys. These run on real
// goroutines, not in a synctest bubble: SQLite waits for its locks in
// system calls a bubble cannot see.
func TestLoadWithoutTouchDueTakesNoWriteLock(t *testing.T) {
	s, _ := newStore(t)
	start := time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)
	clock := newFakeClock(start)
	sessions := newSessions(t, s, clock.now, httpsBase)
	c := renew(t, sessions, 6)
	holdWriteLock(t, s)

	clock.set(start.Add(time.Minute - time.Millisecond))
	quick, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if sess, ok, err := sessions.Load(quick, c.Value); err != nil || !ok {
		t.Errorf("Load with no touch due, while the write lock is held = %v, %t, %v; want the session", sess, ok, err)
	}

	// The test can see a write: a touch due waits for the lock, until
	// its context ends.
	clock.set(start.Add(time.Minute))
	brief, cancelBrief := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancelBrief()
	if _, _, err := sessions.Load(brief, c.Value); err == nil {
		t.Error("Load with a touch due, while the write lock is held: no error, so the test cannot see a write")
	}
}

// TestTouchNeverMovesLastSeenBack checks a touch decided on a row that
// another touch has since moved on writes nothing: one load reads the
// row, then waits for the write lock while another, later, touch
// commits.
func TestTouchNeverMovesLastSeenBack(t *testing.T) {
	s, _ := newStore(t)
	ctx := t.Context()
	start := time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)
	clock := newFakeClock(start)
	sessions := newSessions(t, s, clock.now, httpsBase)
	c := renew(t, sessions, 6)

	// The later touch, at 90 s, holds the write lock until it commits.
	later := start.Add(90 * time.Second)
	tx := holdWriteLock(t, s)
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET last_seen_at = ?, expires_at = ? WHERE id_hash = ?`,
		ms(later), ms(later.Add(auth.IdleLimit)), hashOf(t, c.Value)); err != nil {
		t.Fatal(err)
	}

	// The earlier one, at 61 s, reads the last committed row, last seen
	// at the start, finds a touch due and waits for the lock.
	clock.set(start.Add(61 * time.Second))
	loaded := make(chan error, 1)
	go func() {
		_, ok, err := sessions.Load(ctx, c.Value)
		if err == nil && !ok {
			err = errors.New("no session")
		}
		loaded <- err
	}()
	// Long enough for it to reach the lock; were it slower, it would
	// read the later touch, and the test would pass without the race.
	time.Sleep(200 * time.Millisecond)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-loaded; err != nil {
		t.Fatalf("Load: %v", err)
	}
	if r := mustRow(t, s, c.Value, "after both touches"); r.lastSeen != ms(later) || r.expiresAt != ms(later.Add(auth.IdleLimit)) {
		t.Errorf("row last seen at %+d ms, expiring at %+d ms; want the later touch's %+d and %+d",
			r.lastSeen-ms(start), r.expiresAt-ms(start), ms(later)-ms(start), ms(later.Add(auth.IdleLimit))-ms(start))
	}
}

// TestLoadFailsOnAClosedStore checks a database error is returned as one,
// never taken for "no session".
func TestLoadFailsOnAClosedStore(t *testing.T) {
	s, _ := newStore(t)
	sessions := newSessions(t, s, time.Now, httpsBase)
	c := renew(t, sessions, 1)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := sessions.Load(t.Context(), c.Value); err == nil || ok {
		t.Errorf("Load on a closed store = %t, %v; want an error", ok, err)
	} else if strings.Contains(err.Error(), c.Value) {
		t.Errorf("Load's error holds the cookie value: %v", err)
	}
	// A value that names no session is refused before the database.
	if _, ok, err := sessions.Load(t.Context(), "nonsense"); err != nil || ok {
		t.Errorf("Load(malformed) on a closed store = %t, %v; want false, nil", ok, err)
	}
	if _, err := sessions.Renew(t.Context(), 1); err == nil {
		t.Error("Renew on a closed store: no error")
	}
	if _, err := sessions.Destroy(t.Context(), c.Value); err == nil {
		t.Error("Destroy on a closed store: no error")
	}
	if err := sessions.DestroyAllForUser(t.Context(), 1); err == nil {
		t.Error("DestroyAllForUser on a closed store: no error")
	}
	if _, err := sessions.Sweep(t.Context()); err == nil {
		t.Error("Sweep on a closed store: no error")
	}
}
