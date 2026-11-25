// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package web

import "net/http"

// The Cache-Control values the cache policy sends.
const (
	// publicPage lets any cache keep a page for 5 minutes. A page's CSP
	// nonce is then shared by everyone the cache serves it to in that
	// time.
	publicPage = "public, max-age=300"
	noStore    = "no-store"
)

// response is what the cache policy decides a response's caching by.
type response struct {
	status int
	// page is whether the request is a GET or a HEAD.
	page bool
	// signedIn is whether the request came with a live session.
	signedIn bool
	// setsCookie is whether the response sets or clears a cookie.
	setsCookie bool
	// own is the Cache-Control the handler set itself, if any.
	own string
}

// cacheControl returns the Cache-Control resp is sent with, and whether
// it varies by the request's cookies. The first rule that applies wins:
//
//  1. a response that sets or clears a cookie is stored by no cache, even
//     one that would otherwise be kept;
//  2. a response whose handler chose its own Cache-Control keeps it: only
//     /static/'s files, which are immutable;
//  3. a response to a signed-in visitor is stored by no cache;
//  4. a GET or HEAD answered 200 or 404 may be kept by any cache for 5
//     minutes, apart for each set of cookies: Vary: Cookie;
//  5. every other response is stored by no cache.
func (resp response) cacheControl() (value string, varyCookie bool) {
	switch {
	case resp.setsCookie:
		return noStore, false
	case resp.own != "":
		return resp.own, false
	case resp.signedIn:
		return noStore, false
	case resp.page && (resp.status == http.StatusOK || resp.status == http.StatusNotFound):
		return publicPage, true
	}
	return noStore, false
}

// cachePolicy sets the Cache-Control of every response next sends, and
// Vary: Cookie where it may be kept, as response.cacheControl decides,
// just before its header is written. A cookie set before next runs, as
// SessionMiddleware clears a stale one, counts as the response's.
func cachePolicy(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, signedIn := SessionFrom(r.Context())
		page := r.Method == http.MethodGet || r.Method == http.MethodHead
		cw := &cacheWriter{ResponseWriter: w}
		cw.decide = func(status int) {
			h := w.Header()
			value, vary := response{
				status: status, page: page, signedIn: signedIn,
				setsCookie: len(h.Values("Set-Cookie")) > 0, own: h.Get("Cache-Control"),
			}.cacheControl()
			h.Set("Cache-Control", value)
			if vary {
				h.Add("Vary", "Cookie")
			}
		}
		next.ServeHTTP(cw, r)
		if !cw.decided {
			// The handler wrote nothing, so net/http sends 200 once it
			// returns.
			cw.decided = true
			cw.decide(http.StatusOK)
		}
	})
}

// cacheWriter runs decide once, just before the response's header is
// written, with the status it is written with.
type cacheWriter struct {
	http.ResponseWriter

	decide  func(status int)
	decided bool
}

func (w *cacheWriter) WriteHeader(code int) {
	// A 1xx other than 101 leaves the final response still to come.
	if !w.decided && (code < 100 || code >= 200 || code == http.StatusSwitchingProtocols) {
		w.decided = true
		w.decide(code)
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *cacheWriter) Write(p []byte) (int, error) {
	if !w.decided {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(p)
}

// FlushError writes the header, if the handler has not, then flushes what
// is buffered to the client.
func (w *cacheWriter) FlushError() error {
	if !w.decided {
		w.WriteHeader(http.StatusOK)
	}
	return http.NewResponseController(w.ResponseWriter).Flush()
}

// Unwrap returns the ResponseWriter w wraps, for http.ResponseController.
func (w *cacheWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
