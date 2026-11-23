// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

// Package web serves Pisati's website. SessionMiddleware loads each
// request's session from its cookie, and SessionFrom hands it to the
// handlers. Assets gives pages the front end's stylesheets and scripts,
// each checked by its integrity value, and serves them under /static/.
package web

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/divbast/pisati/internal/app"
	"github.com/divbast/pisati/internal/httpx"
)

// sessionKey keys the request's session in its context.
type sessionKey struct{}

// SessionMiddleware loads the session that each request's session cookie
// names, with sessions.Load, and hands it on in the request's context,
// for SessionFrom. It reads only the cookie of sessions.CookieName: over
// https, a pisati cookie, which another host could have set, is not
// read. A request without the cookie goes on with no session.
//
// It parses the cookie's value with app.ParseToken, once, here at the
// edge. A cookie that names no live session, being malformed, unknown,
// destroyed or expired, is cleared with sessions.Clear's expiring cookie,
// and the request goes on with no session. A malformed one never reaches
// the database.
//
// When the session cannot be looked up, the database failing, the
// failure is logged to log, once and without the cookie, and the request
// is answered 500 with Cache-Control: no-store, its handler never called.
// It is not served as signed out: a signed-in user would then be sent to
// sign in again for the server's fault, which signing in would not mend,
// and the cookie is left as it is, since it may well name a live
// session. A failed touch fails nothing: sessions.Load serves the session
// and logs it. A request whose context ends first, its client gone, is
// answered with nothing and logged nowhere.
//
// A nil sessions or log, or the zero Sessions, is a bug, and
// SessionMiddleware panics on it.
func SessionMiddleware(sessions *app.Sessions, log *slog.Logger) httpx.Middleware {
	name := sessions.CookieName()
	if name == "" {
		panic("web: SessionMiddleware needs Sessions made by app.NewSessions")
	}
	if log == nil {
		panic("web: SessionMiddleware needs a logger")
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// r.Cookie fails only with http.ErrNoCookie.
			c, err := r.Cookie(name)
			if err != nil {
				next.ServeHTTP(w, r)
				return
			}
			signedOut := func() {
				expired := sessions.Clear()
				http.SetCookie(w, &expired)
				next.ServeHTTP(w, r)
			}
			t, ok := app.ParseToken(c.Value)
			if !ok {
				signedOut()
				return
			}
			sess, ok, err := sessions.Load(r.Context(), t)
			if err != nil {
				// The client has gone: there is no one to answer, and
				// nothing went wrong to log.
				if errors.Is(err, context.Canceled) {
					return
				}
				log.ErrorContext(r.Context(), "session load failed", "error", err)
				w.Header().Set("Cache-Control", "no-store")
				http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
				return
			}
			if !ok {
				signedOut()
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), sessionKey{}, sess)))
		})
	}
}

// SessionFrom returns the session SessionMiddleware loaded for the request
// whose context is ctx, or false when it has none: it had no session
// cookie, or one that named no live session.
func SessionFrom(ctx context.Context) (app.Session, bool) {
	sess, ok := ctx.Value(sessionKey{}).(app.Session)
	return sess, ok
}
