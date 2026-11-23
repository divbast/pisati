// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package app_test

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/divbast/pisati/internal/app"
	"github.com/divbast/pisati/internal/config"
	"github.com/divbast/pisati/internal/core/auth"
	"github.com/divbast/pisati/internal/store"
)

// The bases the tests serve from: https, and plain http on localhost.
const (
	httpsBase = "https://blog.example"
	localBase = "http://localhost:8080"
)

// urlAlphabet is base64url's alphabet, each character at its value.
const urlAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"

// newStore returns a store at Pisati's schema, in a new state directory of
// its own, and that directory. The store is closed when the test ends.
func newStore(t testing.TB) (*store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	lock, err := store.AcquireLock(t.Context(), dir)
	if err != nil {
		t.Fatalf("AcquireLock(%s): %v", dir, err)
	}
	s, err := store.Open(t.Context(), store.DatabasePath(dir))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	_, _, err = s.Migrate(t.Context(), lock)
	if rerr := lock.Release(); rerr != nil {
		t.Fatal(rerr)
	}
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return s, dir
}

// inBubble runs f in a synctest bubble, with a store at Pisati's schema.
// The store is opened outside the bubble, so that the goroutine
// database/sql starts for it is the test's, not the bubble's.
func inBubble(t *testing.T, f func(t *testing.T, s *store.Store)) {
	t.Helper()
	s, _ := newStore(t)
	synctest.Test(t, func(t *testing.T) {
		f(t, s)
	})
}

// baseURL returns s as a config.BaseURL, failing the test if it is not one.
func baseURL(t testing.TB, s string) config.BaseURL {
	t.Helper()
	b, err := config.ParseBaseURL(s)
	if err != nil {
		t.Fatalf("ParseBaseURL(%q): %v", s, err)
	}
	return b
}

// newSessions returns Sessions on s that read the clock with clock and
// draw IDs from crypto/rand, served from base, logging nowhere.
func newSessions(t testing.TB, s *store.Store, clock func() time.Time, base string) *app.Sessions {
	t.Helper()
	sessions, err := app.NewSessions(s, clock, rand.Reader, baseURL(t, base), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewSessions: %v", err)
	}
	return sessions
}

// newLoggedSessions returns Sessions as newSessions does, logging to the
// buffer it returns.
func newLoggedSessions(t testing.TB, s *store.Store, clock func() time.Time, base string) (*app.Sessions, *logBuffer) {
	t.Helper()
	logs := &logBuffer{}
	sessions, err := app.NewSessions(s, clock, rand.Reader, baseURL(t, base), slog.New(slog.NewJSONHandler(logs, nil)))
	if err != nil {
		t.Fatalf("NewSessions: %v", err)
	}
	return sessions, logs
}

// logBuffer holds what a JSON slog handler writes to it, and is safe for
// concurrent use.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// String returns every line logged so far.
func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// wantLogged fails the test unless the log holds exactly n records with
// the message msg, each at level, and nothing that names value.
func wantLogged(t *testing.T, logs *logBuffer, msg, level string, n int, value string) {
	t.Helper()
	got := logs.String()
	if c := strings.Count(got, `"msg":"`+msg+`"`); c != n {
		t.Errorf("logged %q %d times, want %d:\n%s", msg, c, n, got)
	}
	if lines := strings.Count(got, "\n"); lines != n {
		t.Errorf("logged %d records, want %d:\n%s", lines, n, got)
	}
	if n > 0 && strings.Count(got, `"level":"`+level+`"`) != n {
		t.Errorf("want each record at %s:\n%s", level, got)
	}
	if value != "" && strings.Contains(got, value) {
		t.Errorf("the log holds the cookie value:\n%s", got)
	}
}

// renew starts a session for user, failing the test on an error, and
// returns its cookie.
func renew(t testing.TB, sessions *app.Sessions, user auth.UserID) http.Cookie {
	t.Helper()
	c, err := sessions.Renew(t.Context(), user)
	if err != nil {
		t.Fatalf("Renew(%d): %v", user, err)
	}
	return c
}

// token returns the Token a cookie value spells, failing the test if it
// spells none.
func token(t testing.TB, value string) app.Token {
	t.Helper()
	tok, ok := app.ParseToken(value)
	if !ok {
		t.Fatalf("ParseToken refused a cookie value of %d characters", len(value))
	}
	return tok
}

// load loads the session of the cookie value, failing the test on an
// error, and returns its user, or false when there is no live session.
func load(t *testing.T, sessions *app.Sessions, value string) (auth.UserID, bool) {
	t.Helper()
	sess, ok, err := sessions.Load(t.Context(), token(t, value))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !ok {
		if user, signedIn := sess.User(); signedIn {
			t.Errorf("Load found no session, but its Session has the user %d", user)
		}
		return 0, false
	}
	user, signedIn := sess.User()
	if !signedIn {
		t.Errorf("Load found a session without a user")
	}
	return user, true
}

// wantUser fails the test unless the cookie value loads a live session
// of user.
func wantUser(t *testing.T, sessions *app.Sessions, value string, user auth.UserID, when string) {
	t.Helper()
	got, ok := load(t, sessions, value)
	if !ok {
		t.Errorf("%s: no live session, want one of user %d", when, user)
		return
	}
	if got != user {
		t.Errorf("%s: session of user %d, want %d", when, got, user)
	}
}

// wantNoSession fails the test if the cookie value loads a session.
func wantNoSession(t *testing.T, sessions *app.Sessions, value, when string) {
	t.Helper()
	if user, ok := load(t, sessions, value); ok {
		t.Errorf("%s: a live session of user %d, want none", when, user)
	}
}

// idOf returns the 32-byte ID a session cookie value spells, failing the
// test unless it is 43 characters of unpadded base64url.
func idOf(t testing.TB, value string) []byte {
	t.Helper()
	if len(value) != 43 {
		t.Fatalf("cookie value of %d characters, want 43", len(value))
	}
	id, err := base64.RawURLEncoding.Strict().DecodeString(value)
	if err != nil || len(id) != 32 {
		t.Fatalf("cookie value is not 32 bytes of unpadded base64url (%d bytes, %v)", len(id), err)
	}
	return id
}

// hashOf returns the SHA-256 of the ID the cookie value spells: what the
// sessions table keys it by.
func hashOf(t testing.TB, value string) []byte {
	t.Helper()
	sum := sha256.Sum256(idOf(t, value))
	return sum[:]
}

// sessionRow is one row of the sessions table.
type sessionRow struct {
	user                         sql.NullInt64
	created, lastSeen, expiresAt int64
}

// rowOf returns the row of the session the cookie value names, or false
// when there is none.
func rowOf(t *testing.T, s *store.Store, value string) (sessionRow, bool) {
	t.Helper()
	var r sessionRow
	err := s.DB().QueryRowContext(t.Context(),
		`SELECT user_id, created_at, last_seen_at, expires_at FROM sessions WHERE id_hash = ?`,
		hashOf(t, value)).Scan(&r.user, &r.created, &r.lastSeen, &r.expiresAt)
	switch {
	case err == nil:
		return r, true
	case errors.Is(err, sql.ErrNoRows):
		return sessionRow{}, false
	}
	t.Fatalf("read the session row: %v", err)
	return sessionRow{}, false
}

// mustRow returns the row of the session the cookie value names, failing
// the test when there is none.
func mustRow(t *testing.T, s *store.Store, value, when string) sessionRow {
	t.Helper()
	r, ok := rowOf(t, s, value)
	if !ok {
		t.Fatalf("%s: no row for the session", when)
	}
	return r
}

// countSessions returns how many rows the sessions table holds.
func countSessions(t *testing.T, s *store.Store) int {
	t.Helper()
	var n int
	if err := s.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM sessions`).Scan(&n); err != nil {
		t.Fatalf("count the sessions: %v", err)
	}
	return n
}

// exec runs a statement on s, failing the test on an error.
func exec(t *testing.T, s *store.Store, query string, args ...any) {
	t.Helper()
	if _, err := s.DB().ExecContext(t.Context(), query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

// ms returns t in Unix milliseconds, as the sessions table holds times.
func ms(t time.Time) int64 {
	return t.UnixMilli()
}

// fakeClock is a clock a test sets, safe for concurrent use. When onRead
// is set, each reading of the clock calls it first.
type fakeClock struct {
	millis atomic.Int64
	onRead atomic.Pointer[func()]
}

func newFakeClock(at time.Time) *fakeClock {
	c := &fakeClock{}
	c.set(at)
	return c
}

func (c *fakeClock) now() time.Time {
	if f := c.onRead.Load(); f != nil {
		(*f)()
	}
	return time.UnixMilli(c.millis.Load())
}

func (c *fakeClock) set(at time.Time) { c.millis.Store(at.UnixMilli()) }

// failingRand is a random source that cannot be read.
type failingRand struct{}

func (failingRand) Read([]byte) (int, error) {
	return 0, errors.New("no randomness to be had")
}

// shortRand is a random source that runs dry after n bytes.
type shortRand struct {
	n int
}

func (r *shortRand) Read(p []byte) (int, error) {
	if r.n == 0 {
		return 0, io.EOF
	}
	k := min(len(p), r.n)
	for i := range k {
		p[i] = 0x5a
	}
	r.n -= k
	return k, nil
}
