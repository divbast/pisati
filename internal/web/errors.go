// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package web

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"github.com/divbast/pisati/internal/app"
	"github.com/divbast/pisati/internal/web/msg"
)

// WriteError answers r with the page err calls for, mapping each
// app.Error, found with errors.AsType, once:
//
//   - *app.NotFound: 404;
//   - *app.Forbidden: 403;
//   - *app.Unauthenticated: 303 See Other to /login, with the page asked
//     for as next when parseLocalPath takes it;
//   - *app.Invalid: 400, saying which rule its Reason names, and never
//     what was sent;
//   - *app.Conflict: 409;
//   - *app.RateLimited: 429, with Retry-After its whole seconds.
//
// Any other error, nil among them, is the server's fault: it is logged,
// once, at error level, and answered 500 with a page that says nothing of
// it. Each page is the layout's, with the catalogue's text. Whether a
// cache may keep the answer is the site's cache policy's to decide.
func (s *Site) WriteError(w http.ResponseWriter, r *http.Request, err error) {
	s.made()
	l := msg.English
	e, ok := errors.AsType[app.Error](err)
	if !ok {
		s.serverError(w, r, err)
		return
	}
	switch e := e.(type) {
	case *app.NotFound:
		s.problem(w, r, http.StatusNotFound, msg.NotFoundTitle(l), msg.NotFoundBody(l))
	case *app.Forbidden:
		s.problem(w, r, http.StatusForbidden, msg.ForbiddenTitle(l), msg.ForbiddenBody(l))
	case *app.Unauthenticated:
		redirectToSignIn(w, r)
	case *app.Invalid:
		s.problem(w, r, http.StatusBadRequest, msg.InvalidTitle(l), msg.InvalidReason(l, e.Reason))
	case *app.Conflict:
		s.problem(w, r, http.StatusConflict, msg.ConflictTitle(l), msg.ConflictBody(l))
	case *app.RateLimited:
		w.Header().Set("Retry-After", strconv.FormatInt(e.RetryAfterSeconds(), 10))
		s.problem(w, r, http.StatusTooManyRequests, msg.TooManyTitle(l), msg.TooManyBody(l))
	default:
		// Only a type outside app that embeds a variant reaches here: a
		// bug, which the server answers for.
		s.serverError(w, r, err)
	}
}

// notFound answers a request for a page that does not exist.
func (s *Site) notFound(w http.ResponseWriter, r *http.Request) {
	s.WriteError(w, r, &app.NotFound{})
}

// serverError logs err, once, and answers 500 with a page that says
// nothing of it.
func (s *Site) serverError(w http.ResponseWriter, r *http.Request, err error) {
	l := msg.English
	s.log.ErrorContext(r.Context(), "request failed", "error", err)
	s.problem(w, r, http.StatusInternalServerError, msg.ServerErrorTitle(l), msg.ServerErrorBody(l))
}

// problem answers with status and the page that says heading and body,
// with a link home.
func (s *Site) problem(w http.ResponseWriter, r *http.Request, status int, heading, body string) {
	l := msg.English
	s.render(w, r, status, msg.PageTitle(l, heading), problemContent(l, heading, body))
}

// redirectToSignIn sends r to sign in: 303 See Other to /login, with r's
// path and query as next when parseLocalPath takes them, and with none
// otherwise.
func redirectToSignIn(w http.ResponseWriter, r *http.Request) {
	target := "/login"
	if next, ok := parseLocalPath(r.URL.RequestURI()); ok {
		target += "?" + url.Values{string(nextField): {next.String()}}.Encode()
	}
	w.Header().Set("Location", target)
	w.WriteHeader(http.StatusSeeOther)
}
