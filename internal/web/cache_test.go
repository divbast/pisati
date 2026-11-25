// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package web_test

import (
	"net/http"
	"strings"
	"testing"
)

// TestCachePolicy holds each row of the caching table to a response of
// the site's: what a shared cache may keep, and for whom.
func TestCachePolicy(t *testing.T) {
	t.Parallel()

	p := newPisati(t, options{store: newStore(t)})
	live := p.sessions
	session, err := live.Renew(t.Context(), 3)
	if err != nil {
		t.Fatal(err)
	}
	signedIn := []*http.Cookie{requestCookie(session.Name, session.Value)}
	stale := []*http.Cookie{requestCookie(live.CookieName(), unknownValue(t))}
	staticFile := "/static/" + script

	tests := []struct {
		name   string
		req    request
		status int
		want   string
		// cleared is whether the response clears the stale session cookie.
		cleared bool
	}{
		// A signed-out visitor's pages, and every 404, may be kept by any
		// cache for 5 minutes, apart for each set of cookies.
		{name: "the home page", req: request{target: "/"}, status: http.StatusOK, want: publicPage},
		{name: "the sign-in page", req: request{target: "/login"}, status: http.StatusOK, want: publicPage},
		{name: "HEAD of a page", req: request{method: http.MethodHead, target: "/login"}, status: http.StatusOK, want: publicPage},
		{name: "a page not found", req: request{target: "/nothing-here"}, status: http.StatusNotFound, want: publicPage},
		{name: "a static file not found", req: request{target: "/static/assets/gone-0000.js"}, status: http.StatusNotFound, want: publicPage},

		// The build's files keep their own caching, for every visitor.
		{name: "a static file", req: request{target: staticFile}, status: http.StatusOK, want: immutable},
		{name: "a static file, signed in", req: request{target: staticFile, cookies: signedIn}, status: http.StatusOK, want: immutable},

		// A signed-in visitor's answers are theirs alone.
		{name: "a page, signed in", req: request{target: "/", cookies: signedIn}, status: http.StatusOK, want: noStore},
		{name: "a 404, signed in", req: request{target: "/nothing-here", cookies: signedIn}, status: http.StatusNotFound, want: noStore},

		// An answer that sets or clears a cookie is never kept, even one
		// that would otherwise be public or immutable.
		{name: "a page clearing a stale session", req: request{target: "/", cookies: stale}, status: http.StatusOK, want: noStore, cleared: true},
		{name: "a 404 clearing a stale session", req: request{target: "/nothing-here", cookies: stale}, status: http.StatusNotFound, want: noStore, cleared: true},
		{name: "a static file clearing a stale session", req: request{target: staticFile, cookies: stale}, status: http.StatusOK, want: noStore, cleared: true},
		{name: "the theme switch", req: request{method: http.MethodPost, target: "/_/theme", body: "theme=dark&next=%2F"}, status: http.StatusSeeOther, want: noStore},

		// Every other status.
		{name: "a 303 setting no cookie", req: request{method: http.MethodPost, target: "/_/theme", body: "theme=x&next=%2F"}, status: http.StatusSeeOther, want: noStore},
		{name: "the mux's 307", req: request{target: "//login"}, status: http.StatusTemporaryRedirect, want: noStore},
		{name: "a 204", req: chromeUpload(chromeReports), status: http.StatusNoContent, want: noStore},
		{name: "a 400", req: request{method: http.MethodPost, target: "/_/theme", body: "next=%zz"}, status: http.StatusBadRequest, want: noStore},
		{name: "the mux's 405", req: request{method: http.MethodPost, target: "/nothing-here"}, status: http.StatusMethodNotAllowed, want: noStore},
		{name: "a 413", req: request{target: "/", body: "x", length: 1}, status: http.StatusRequestEntityTooLarge, want: noStore},
		{
			name: "a 416", req: request{target: staticFile, header: map[string]string{"Range": "bytes=100000-"}},
			status: http.StatusRequestedRangeNotSatisfiable, want: noStore,
		},
		{name: "the 501 of signing in", req: request{method: http.MethodPost, target: "/login", body: "username=a&password=b"}, status: http.StatusNotImplemented, want: noStore},
	}
	for _, tt := range tests {
		w := p.do(tt.req)
		if w.Code != tt.status {
			t.Errorf("%s: %d, want %d", tt.name, w.Code, tt.status)
			continue
		}
		wantCache(t, w, tt.want, tt.name)
		cleared := false
		for _, c := range w.Header().Values("Set-Cookie") {
			cleared = cleared || strings.HasPrefix(c, live.CookieName()+"=;")
		}
		if cleared != tt.cleared {
			t.Errorf("%s: the stale session cookie cleared: %t, want %t", tt.name, cleared, tt.cleared)
		}
		if tt.want == noStore && strings.Contains(strings.Join(w.Header().Values("Vary"), ","), "Cookie") {
			t.Errorf("%s: Vary: Cookie on an answer no cache keeps", tt.name)
		}
	}
}

// TestCachePolicyKeepsOnlyTheStaticFilesOwn holds the policy to its table
// whatever Cache-Control a response already carries when its header is
// written: only the build's files keep their own. Every other route's
// handler is, to the policy, like this middleware, which sets one before
// the site runs.
func TestCachePolicyKeepsOnlyTheStaticFilesOwn(t *testing.T) {
	t.Parallel()

	p := newPisati(t, options{store: newStore(t)})
	session, err := p.sessions.Renew(t.Context(), 4)
	if err != nil {
		t.Fatal(err)
	}
	signedIn := []*http.Cookie{requestCookie(session.Name, session.Value)}
	site := p.h
	p.h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=86400")
		site.ServeHTTP(w, r)
	})

	for _, tt := range []struct {
		name string
		req  request
		want string
	}{
		{name: "a page, signed in", req: request{target: "/", cookies: signedIn}, want: noStore},
		{name: "a page, signed out", req: request{target: "/"}, want: publicPage},
		{name: "a 404, signed out", req: request{target: "/nothing-here"}, want: publicPage},
		{name: "the theme switch", req: request{method: http.MethodPost, target: "/_/theme", body: "theme=dark&next=%2F"}, want: noStore},
		{name: "a static file", req: request{target: "/static/" + script}, want: immutable},
		{name: "a static file, signed in", req: request{target: "/static/" + script, cookies: signedIn}, want: immutable},
		// The static handler sets no Cache-Control of its own on a miss, so
		// what was there before it ran is not its to keep.
		{name: "a static file not found", req: request{target: "/static/assets/gone-0000.js"}, want: publicPage},
		{name: "a static file not found, signed in", req: request{target: "/static/assets/gone-0000.js", cookies: signedIn}, want: noStore},
		{name: "a static file's range not satisfiable", req: request{target: "/static/" + script, header: map[string]string{"Range": "bytes=100000-"}}, want: noStore},
	} {
		wantCache(t, p.do(tt.req), tt.want, tt.name)
	}
}
