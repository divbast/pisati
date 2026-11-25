// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package web_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/divbast/pisati/internal/app"
	"github.com/divbast/pisati/internal/config"
	"github.com/divbast/pisati/internal/core/auth"
	"github.com/divbast/pisati/internal/store"
	"github.com/divbast/pisati/internal/web"
)

// The expiring cookies the middleware clears an unknown one with.
const (
	clearHTTPS = "__Host-pisati=; Path=/; Max-Age=0; HttpOnly; Secure; SameSite=Lax"
	clearLocal = "pisati=; Path=/; Max-Age=0; HttpOnly; SameSite=Lax"
)

// newStore returns a store at Pisati's schema, in a new state directory of
// its own, and closes it when the test ends.
func newStore(t *testing.T) *store.Store {
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
	return s
}

// logBuffer holds what a JSON slog handler writes to it.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// site is SessionMiddleware on Sessions, both logging to one buffer.
type site struct {
	sessions *app.Sessions
	logs     *logBuffer
	log      *slog.Logger
}

// newSite returns a site on s, served from base, reading the clock with
// clock: time.Now is the bubble's fake clock inside one.
func newSite(t *testing.T, s *store.Store, base string, clock func() time.Time) *site {
	t.Helper()
	b, err := config.ParseBaseURL(base)
	if err != nil {
		t.Fatal(err)
	}
	logs := &logBuffer{}
	log := slog.New(slog.NewJSONHandler(logs, nil))
	sessions, err := app.NewSessions(s, clock, rand.Reader, b, log)
	if err != nil {
		t.Fatalf("NewSessions: %v", err)
	}
	return &site{sessions: sessions, logs: logs, log: log}
}

// renew starts a session for user, failing the test on an error.
func (st *site) renew(t *testing.T, user auth.UserID) http.Cookie {
	t.Helper()
	c, err := st.sessions.Renew(t.Context(), user)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// seen is what the handler behind the middleware saw of one request.
type seen struct {
	called   bool
	session  bool
	user     auth.UserID
	signedIn bool
}

// requestCookie returns the cookie name=value as a browser sends it.
// AddCookie sends the name and value alone, so the attributes, set as the
// server sets them, change nothing.
func requestCookie(name, value string) *http.Cookie {
	return &http.Cookie{Name: name, Value: value, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode}
}

// serve sends a GET with ctx and the cookies through SessionMiddleware, to
// a handler that records what SessionFrom gives it, and returns the
// response, what the handler saw, and what was logged while it was
// served.
func (st *site) serve(ctx context.Context, t *testing.T, cookies ...*http.Cookie) (*httptest.ResponseRecorder, seen, string) {
	t.Helper()
	before := len(st.logs.String())
	var got seen
	h := web.SessionMiddleware(st.sessions, st.log)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.called = true
		var sess app.Session
		sess, got.session = web.SessionFrom(r.Context())
		got.user, got.signedIn = sess.User()
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec, got, st.logs.String()[before:]
}

// wantCleared fails the test unless resp clears the session cookie with
// exactly want, and sets no other cookie.
func wantCleared(t *testing.T, resp *httptest.ResponseRecorder, want, when string) {
	t.Helper()
	got := resp.Header().Values("Set-Cookie")
	if len(got) != 1 || got[0] != want {
		t.Errorf("%s: Set-Cookie %q, want exactly %q", when, got, want)
	}
}

// wantNoCookie fails the test if resp sets a cookie.
func wantNoCookie(t *testing.T, resp *httptest.ResponseRecorder, when string) {
	t.Helper()
	if got := resp.Header().Values("Set-Cookie"); len(got) != 0 {
		t.Errorf("%s: Set-Cookie %q, want none", when, got)
	}
}

// wantSignedOut fails the test unless the handler ran, with no session.
func wantSignedOut(t *testing.T, resp *httptest.ResponseRecorder, got seen, when string) {
	t.Helper()
	if resp.Code != http.StatusNoContent || !got.called {
		t.Fatalf("%s: status %d, handler called %t; want the handler's 204", when, resp.Code, got.called)
	}
	if got.session || got.signedIn {
		t.Errorf("%s: the handler saw a session (user %d), want none", when, got.user)
	}
}

// wantSignedIn fails the test unless the handler ran with user's session.
func wantSignedIn(t *testing.T, resp *httptest.ResponseRecorder, got seen, user auth.UserID, when string) {
	t.Helper()
	if resp.Code != http.StatusNoContent || !got.called || !got.session || !got.signedIn || got.user != user {
		t.Errorf("%s: status %d, the handler saw %+v; want its 204 with user %d's session", when, resp.Code, got, user)
	}
}

// unknownValue is a well-formed session cookie value that names no
// session.
func unknownValue(t *testing.T) string {
	t.Helper()
	id := make([]byte, 32)
	if _, err := rand.Read(id); err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(id)
}

func TestUnknownCookieCleared(t *testing.T) {
	for _, tc := range []struct {
		base, name, other, clear string
	}{
		{base: "https://blog.example", name: "__Host-pisati", other: "pisati", clear: clearHTTPS},
		{base: "http://localhost:8080", name: "pisati", other: "__Host-pisati", clear: clearLocal},
	} {
		t.Run(tc.base, func(t *testing.T) {
			st := newSite(t, newStore(t), tc.base, time.Now)
			for name, value := range map[string]string{
				"unknown":   unknownValue(t),
				"malformed": "not-a-session",
				"empty":     "",
			} {
				resp, got, _ := st.serve(t.Context(), t, requestCookie(tc.name, value))
				wantSignedOut(t, resp, got, name)
				wantCleared(t, resp, tc.clear, name)
			}

			live := st.renew(t, 8)
			resp, got, _ := st.serve(t.Context(), t, &live)
			wantSignedIn(t, resp, got, 8, "live cookie")
			wantNoCookie(t, resp, "live cookie")

			tok, ok := app.ParseToken(live.Value)
			if !ok {
				t.Fatal("ParseToken refused Renew's cookie")
			}
			if _, err := st.sessions.Destroy(t.Context(), tok); err != nil {
				t.Fatal(err)
			}
			resp, got, _ = st.serve(t.Context(), t, &live)
			wantSignedOut(t, resp, got, "destroyed")
			wantCleared(t, resp, tc.clear, "destroyed")

			resp, got, _ = st.serve(t.Context(), t)
			wantSignedOut(t, resp, got, "no cookie")
			wantNoCookie(t, resp, "no cookie")

			// A cookie of the other name is not this server's to read or
			// clear, even when it holds a live session's value.
			again := st.renew(t, 8)
			resp, got, _ = st.serve(t.Context(), t, requestCookie(tc.other, again.Value))
			wantSignedOut(t, resp, got, "the other name")
			wantNoCookie(t, resp, "the other name")
		})
	}

	t.Run("expired", func(t *testing.T) {
		s := newStore(t)
		synctest.Test(t, func(t *testing.T) {
			st := newSite(t, s, "https://blog.example", time.Now)
			c := st.renew(t, 8)
			time.Sleep(auth.IdleLimit - time.Millisecond)
			resp, got, _ := st.serve(t.Context(), t, &c)
			wantSignedIn(t, resp, got, 8, "a millisecond short of the idle limit")
			wantNoCookie(t, resp, "a millisecond short of the idle limit")

			time.Sleep(auth.IdleLimit)
			resp, got, _ = st.serve(t.Context(), t, &c)
			wantSignedOut(t, resp, got, "idle for a week")
			wantCleared(t, resp, clearHTTPS, "idle for a week")
		})
	})
}

// TestSessionMiddlewareDatabaseError checks a request whose session cannot
// be looked up gets a 500, logged once without the cookie, and keeps its
// cookie, while a value that names no session never reaches the database.
func TestSessionMiddlewareDatabaseError(t *testing.T) {
	s := newStore(t)
	st := newSite(t, s, "https://blog.example", time.Now)
	c := st.renew(t, 8)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	resp, got, logged := st.serve(t.Context(), t, &c)
	if resp.Code != http.StatusInternalServerError || got.called {
		t.Errorf("status %d, handler called %t; want 500 without the handler", resp.Code, got.called)
	}
	if cc := resp.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control %q, want no-store", cc)
	}
	wantNoCookie(t, resp, "a database error")
	if n := strings.Count(logged, `"msg":"session not read from the database, so the request was answered with status 500"`); n != 1 || strings.Count(logged, "\n") != 1 {
		t.Errorf("logged %d session failures, want exactly 1 record:\n%s", n, logged)
	}
	if !strings.Contains(logged, `"level":"ERROR"`) {
		t.Errorf("the failure is not logged at error level:\n%s", logged)
	}
	if strings.Contains(logged, c.Value) {
		t.Errorf("the log holds the cookie value:\n%s", logged)
	}

	resp, got, logged = st.serve(t.Context(), t, requestCookie("__Host-pisati", "not-a-session"))
	wantSignedOut(t, resp, got, "a malformed value on a closed store")
	wantCleared(t, resp, clearHTTPS, "a malformed value on a closed store")
	if logged != "" {
		t.Errorf("logged for a malformed value:\n%s", logged)
	}
}

// TestSessionMiddlewareServesWhenTheTouchFails has the database refuse
// every touch: the request is served with its session, and the failed
// touch is logged once, as a warning, without the cookie.
func TestSessionMiddlewareServesWhenTheTouchFails(t *testing.T) {
	s := newStore(t)
	start := time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)
	now := start
	var mu sync.Mutex
	st := newSite(t, s, "https://blog.example", func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	})
	c := st.renew(t, 8)
	if _, err := s.DB().ExecContext(t.Context(),
		`CREATE TRIGGER refuse_touch BEFORE UPDATE ON sessions BEGIN SELECT RAISE(ABORT, 'touch refused'); END`); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	now = start.Add(time.Minute)
	mu.Unlock()

	resp, got, logged := st.serve(t.Context(), t, &c)
	wantSignedIn(t, resp, got, 8, "a touch due, which fails")
	wantNoCookie(t, resp, "a touch due, which fails")
	if strings.Count(logged, `"msg":"session's last-seen time not saved"`) != 1 || strings.Count(logged, "\n") != 1 ||
		!strings.Contains(logged, `"level":"WARN"`) {
		t.Errorf("want exactly one warning of the failed touch:\n%s", logged)
	}
	if strings.Contains(logged, c.Value) {
		t.Errorf("the log holds the cookie value:\n%s", logged)
	}
}

// TestSessionMiddlewareQuietWhenTheClientIsGone checks a request whose
// context has ended, its client gone, is answered with nothing and logged
// nowhere.
func TestSessionMiddlewareQuietWhenTheClientIsGone(t *testing.T) {
	st := newSite(t, newStore(t), "https://blog.example", time.Now)
	c := st.renew(t, 8)
	gone, cancel := context.WithCancel(t.Context())
	cancel()

	resp, got, logged := st.serve(gone, t, &c)
	if got.called {
		t.Error("the handler ran for a request whose client had gone")
	}
	if resp.Code != http.StatusOK || resp.Body.Len() != 0 || len(resp.Header()) != 0 {
		t.Errorf("wrote status %d, %d bytes and headers %v; want nothing written", resp.Code, resp.Body.Len(), resp.Header())
	}
	if logged != "" {
		t.Errorf("logged for a request whose client had gone:\n%s", logged)
	}
}

func TestSessionFromWithoutMiddleware(t *testing.T) {
	sess, ok := web.SessionFrom(t.Context())
	if ok {
		t.Error("SessionFrom found a session in a context without one")
	}
	if user, signedIn := sess.User(); signedIn {
		t.Errorf("the zero Session has the user %d", user)
	}
}

func TestSessionMiddlewarePanicsOnNil(t *testing.T) {
	st := newSite(t, newStore(t), "https://blog.example", time.Now)
	log := slog.New(slog.DiscardHandler)
	for name, f := range map[string]func(){
		"nil Sessions":  func() { web.SessionMiddleware(nil, log) },
		"zero Sessions": func() { web.SessionMiddleware(&app.Sessions{}, log) },
		"nil logger":    func() { web.SessionMiddleware(st.sessions, nil) },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Errorf("SessionMiddleware with a %s did not panic", name)
				}
			}()
			f()
		})
	}
}
