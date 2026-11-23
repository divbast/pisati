// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package web_test

import (
	"bytes"
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

// newSessions returns Sessions on s, served from base, reading the clock
// with time.Now (the bubble's fake clock inside one).
func newSessions(t *testing.T, s *store.Store, base string) *app.Sessions {
	t.Helper()
	b, err := config.ParseBaseURL(base)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := app.NewSessions(s, time.Now, rand.Reader, b)
	if err != nil {
		t.Fatalf("NewSessions: %v", err)
	}
	return sessions
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

// serve sends a GET with the cookies through SessionMiddleware on
// sessions, to a handler that records what SessionFrom gives it, and
// returns the response, what the handler saw, and what was logged.
func serve(t *testing.T, sessions *app.Sessions, cookies ...*http.Cookie) (*httptest.ResponseRecorder, seen, string) {
	t.Helper()
	var logs logBuffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))
	var got seen
	h := web.SessionMiddleware(sessions, log)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.called = true
		var sess app.Session
		sess, got.session = web.SessionFrom(r.Context())
		got.user, got.signedIn = sess.User()
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec, got, logs.String()
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
			s := newStore(t)
			sessions := newSessions(t, s, tc.base)
			for name, value := range map[string]string{
				"unknown":   unknownValue(t),
				"malformed": "not-a-session",
				"empty":     "",
			} {
				resp, got, _ := serve(t, sessions, requestCookie(tc.name, value))
				wantSignedOut(t, resp, got, name)
				wantCleared(t, resp, tc.clear, name)
			}

			live, err := sessions.Renew(t.Context(), 8)
			if err != nil {
				t.Fatal(err)
			}
			resp, got, _ := serve(t, sessions, &live)
			if !got.called || !got.session || !got.signedIn || got.user != 8 {
				t.Errorf("live cookie: the handler saw %+v, want user 8's session", got)
			}
			wantNoCookie(t, resp, "live cookie")

			if _, err := sessions.Destroy(t.Context(), live.Value); err != nil {
				t.Fatal(err)
			}
			resp, got, _ = serve(t, sessions, &live)
			wantSignedOut(t, resp, got, "destroyed")
			wantCleared(t, resp, tc.clear, "destroyed")

			resp, got, _ = serve(t, sessions)
			wantSignedOut(t, resp, got, "no cookie")
			wantNoCookie(t, resp, "no cookie")

			// A cookie of the other name is not this server's to read or
			// clear, even when it holds a live session's value.
			again, err := sessions.Renew(t.Context(), 8)
			if err != nil {
				t.Fatal(err)
			}
			resp, got, _ = serve(t, sessions, requestCookie(tc.other, again.Value))
			wantSignedOut(t, resp, got, "the other name")
			wantNoCookie(t, resp, "the other name")
		})
	}

	t.Run("expired", func(t *testing.T) {
		s := newStore(t)
		synctest.Test(t, func(t *testing.T) {
			sessions := newSessions(t, s, "https://blog.example")
			c, err := sessions.Renew(t.Context(), 8)
			if err != nil {
				t.Fatal(err)
			}
			time.Sleep(auth.IdleLimit - time.Millisecond)
			resp, got, _ := serve(t, sessions, &c)
			if !got.session || got.user != 8 {
				t.Errorf("a millisecond short of the idle limit: the handler saw %+v, want user 8's session", got)
			}
			wantNoCookie(t, resp, "a millisecond short of the idle limit")

			time.Sleep(auth.IdleLimit)
			resp, got, _ = serve(t, sessions, &c)
			wantSignedOut(t, resp, got, "idle for a week")
			wantCleared(t, resp, clearHTTPS, "idle for a week")
		})
	})
}

// TestSessionMiddlewareDatabaseError checks a request whose session cannot
// be read gets a 500, logged once without the cookie, and keeps its
// cookie, while a value that names no session never reaches the database.
func TestSessionMiddlewareDatabaseError(t *testing.T) {
	s := newStore(t)
	sessions := newSessions(t, s, "https://blog.example")
	c, err := sessions.Renew(t.Context(), 8)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	resp, got, logged := serve(t, sessions, &c)
	if resp.Code != http.StatusInternalServerError || got.called {
		t.Errorf("status %d, handler called %t; want 500 without the handler", resp.Code, got.called)
	}
	if cc := resp.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control %q, want no-store", cc)
	}
	wantNoCookie(t, resp, "a database error")
	if n := strings.Count(logged, `"msg":"session load failed"`); n != 1 {
		t.Errorf("logged %d session failures, want 1:\n%s", n, logged)
	}
	if !strings.Contains(logged, `"level":"ERROR"`) {
		t.Errorf("the failure is not logged at error level:\n%s", logged)
	}
	if strings.Contains(logged, c.Value) {
		t.Errorf("the log holds the cookie value:\n%s", logged)
	}

	resp, got, logged = serve(t, sessions, requestCookie("__Host-pisati", "not-a-session"))
	wantSignedOut(t, resp, got, "a malformed value on a closed store")
	wantCleared(t, resp, clearHTTPS, "a malformed value on a closed store")
	if logged != "" {
		t.Errorf("logged for a malformed value:\n%s", logged)
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
	s := newStore(t)
	sessions := newSessions(t, s, "https://blog.example")
	log := slog.New(slog.DiscardHandler)
	for name, f := range map[string]func(){
		"nil Sessions": func() { web.SessionMiddleware(nil, log) },
		"nil logger":   func() { web.SessionMiddleware(sessions, nil) },
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
