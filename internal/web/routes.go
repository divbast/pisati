// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package web

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/a-h/templ"

	"github.com/divbast/pisati/internal/config"
	"github.com/divbast/pisati/internal/httpx"
	"github.com/divbast/pisati/internal/telemetry"
	"github.com/divbast/pisati/internal/web/msg"
)

// Access is who may use a route: *Public, anyone, or *SignedIn, a
// visitor with a live session. Each is a pointer type, so a value of the
// struct alone is no Access.
//
//sumtype:decl
type Access interface {
	access()
}

// Public is a route anyone may use.
type Public struct{}

// SignedIn is a route only a visitor signed in as a user may use. Anyone
// else is sent to sign in, with 303 See Other to /login, which carries
// the page they asked for as next when the rule the theme switch's next
// follows takes it.
type SignedIn struct{}

func (*Public) access()   {}
func (*SignedIn) access() {}

// Each variant is an Access as a pointer, and only as one.
var (
	_ Access = (*Public)(nil)
	_ Access = (*SignedIn)(nil)
)

// noBody is the body limit of a route that takes none, as a GET: a
// request that declares a body is answered 413.
const noBody httpx.ByteLimit = 0

// Route registers h on mux for pattern, and is the only way a route is
// registered: it declares who may use the route, a, whether its
// responses are cross-origin isolated, iso, and how much body a request
// to it may carry, limit, in one place. A request it matches first tells
// telemetry its route (telemetry.RecordRoute), then gets iso's headers,
// then a's check, and then reaches h with its body held to limit
// (httpx.BodyLimit), or is answered 413 when it declares more.
//
// A nil mux, Access or h, or a pattern mux refuses, is a bug, and Route
// panics on it, as the site's routes are registered once, at start.
func Route(mux *http.ServeMux, pattern string, a Access, iso httpx.Isolation, limit httpx.ByteLimit, h http.Handler) {
	if mux == nil || h == nil {
		panic("web: Route " + pattern + " needs a mux and a handler")
	}
	inner := httpx.BodyLimit(limit)(h)
	switch a.(type) {
	case *Public:
	case *SignedIn:
		inner = signedIn(inner)
	default:
		panic("web: Route " + pattern + " declares no Access")
	}
	isolated := iso.Middleware()(inner)
	mux.Handle(pattern, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		telemetry.RecordRoute(r)
		isolated.ServeHTTP(w, r)
	}))
}

// signedIn serves next a request whose session has a user, and sends any
// other to sign in.
func signedIn(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sess, ok := SessionFrom(r.Context()); ok {
			if _, ok := sess.User(); ok {
				next.ServeHTTP(w, r)
				return
			}
		}
		redirectToSignIn(w, r)
	})
}

// Site is Pisati's public website: every route it serves, behind one
// cache policy, and the pages and error pages they answer with. Make one
// with NewSite, once, in main. The zero Site serves nothing: its methods
// are a bug, and panic.
type Site struct {
	assets *Assets
	secure bool
	log    *slog.Logger
	// reports is the limiter of CSP reports, as middleware.
	reports httpx.Middleware
	handler http.Handler
}

// NewSite returns the site that shows assets' stylesheet and script on
// its pages, sets its cookies Secure when base is https, logs to log, and
// limits CSP reports with reports, finding each client with ip. reports
// is a Limiter of its own, made with httpx.CSPReportLimiterOptions, which
// limits /_/csp-report alone, beside the page limiter every request
// passes. It returns an error naming each argument that is nil, or the
// zero base. A Limiter not made by httpx.NewLimiter is a bug, and
// NewSite panics on it.
func NewSite(assets *Assets, base config.BaseURL, reports *httpx.Limiter, ip httpx.ClientIP, log *slog.Logger) (*Site, error) {
	var errs []error
	if assets == nil {
		errs = append(errs, errors.New("web: the site needs the front end's assets"))
	}
	if base == (config.BaseURL{}) {
		errs = append(errs, errors.New("web: the site needs the base URL, and the zero BaseURL is none"))
	}
	if reports == nil {
		errs = append(errs, errors.New("web: the site needs a limiter for CSP reports"))
	}
	if ip == nil {
		errs = append(errs, errors.New("web: the site needs a ClientIP"))
	}
	if log == nil {
		errs = append(errs, errors.New("web: the site needs a logger"))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	s := &Site{assets: assets, secure: base.IsHTTPS(), log: log, reports: reports.Middleware(ip)}
	s.handler = cachePolicy(s.routes())
	return s, nil
}

// Handler returns the site's handler: a mux holding every route, each
// registered with Route, inside the cache policy, which decides what a
// cache may keep of every response it sends. It is the last of the
// public listener's middleware, after SessionMiddleware, whose session it
// reads. Every call returns the same handler.
func (s *Site) Handler() http.Handler {
	s.made()
	return s.handler
}

// routes returns the mux with every route of the site.
func (s *Site) routes() *http.ServeMux {
	mux := http.NewServeMux()
	Route(mux, "GET /{$}", &Public{}, httpx.NotIsolated, noBody, http.HandlerFunc(s.home))
	Route(mux, "GET /login", &Public{}, httpx.Isolated, noBody, http.HandlerFunc(s.signIn))
	Route(mux, "POST /login", &Public{}, httpx.Isolated, httpx.FormLimit, http.HandlerFunc(s.signInPost))
	Route(mux, "POST /_/theme", &Public{}, httpx.NotIsolated, httpx.FormLimit, http.HandlerFunc(s.setTheme))
	Route(mux, "POST /_/csp-report", &Public{}, httpx.NotIsolated, httpx.CSPReportLimit, s.reports(http.HandlerFunc(s.cspReport)))
	Route(mux, "GET /static/", &Public{}, httpx.NotIsolated, noBody, keepsOwnCaching(s.assets.Handler()))
	// Every other GET finds no page. Other methods to a path no route
	// takes are the mux's own 405.
	Route(mux, "GET /", &Public{}, httpx.NotIsolated, noBody, http.HandlerFunc(s.notFound))
	return mux
}

// made panics unless NewSite made s.
func (s *Site) made() {
	if s == nil || s.handler == nil {
		panic("web: the Site was not made by NewSite")
	}
}

// home serves the home page, which shows nothing yet.
func (s *Site) home(w http.ResponseWriter, r *http.Request) {
	l := msg.English
	s.render(w, r, http.StatusOK, msg.SiteName(l), home(l))
}

// signIn serves the sign-in page.
func (s *Site) signIn(w http.ResponseWriter, r *http.Request) {
	l := msg.English
	s.render(w, r, http.StatusOK, msg.PageTitle(l, msg.SignInTitle(l)), signInForm(l))
}

// signInPost answers the sign-in form with 501: signing in arrives with
// users. It reads nothing the form sent.
func (s *Site) signInPost(w http.ResponseWriter, r *http.Request) {
	l := msg.English
	s.render(w, r, http.StatusNotImplemented, msg.PageTitle(l, msg.NotAvailableTitle(l)),
		problemContent(l, msg.NotAvailableTitle(l), msg.NotAvailableBody(l)))
}

// render answers with status and the page content shows, inside the
// layout, titled title. It renders the page whole before it writes
// anything, so that a failure to render is answered 500 rather than sent
// as half a page. A failure is logged, but for a request whose client
// has gone.
func (s *Site) render(w http.ResponseWriter, r *http.Request, status int, title string, content templ.Component) {
	ctx := r.Context()
	if nonce, ok := httpx.Nonce(ctx); ok {
		ctx = templ.WithNonce(ctx, nonce)
	}
	var b bytes.Buffer
	//nolint:contextcheck // templ's generated components take their context from Render, which this call passes; contextcheck reads the closures they are built from as functions that drop it
	if err := layout(s.page(r, title)).Render(templ.WithChildren(ctx, content), &b); err != nil {
		if !errors.Is(err, context.Canceled) {
			s.log.ErrorContext(ctx, "page render failed", "error", err)
		}
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	//nolint:errcheck // a failed write means the client has gone, and nothing is left to answer it
	w.Write(b.Bytes())
}

// page is what the layout shows around a page's content.
type page struct {
	lang  msg.Lang
	title string
	theme theme
	// next is the page's own path and query, which the theme form comes
	// back to, or / when parseLocalPath refuses them.
	next                     localPath
	style, script, devClient asset
}

// asset is one tag the layout writes for the front end. Its url is empty
// when there is none.
type asset struct {
	url, integrity string
	crossOrigin    bool
}

// assetOf returns the tag tagOf gives as the layout writes it, or the
// zero asset when there is none.
func assetOf(tagOf func() (AssetTag, bool)) asset {
	tag, ok := tagOf()
	if !ok {
		return asset{}
	}
	integrity, _ := tag.Integrity()
	return asset{url: tag.URL(), integrity: integrity, crossOrigin: tag.CrossOrigin()}
}

// page returns what the layout shows around the answer to r, titled
// title.
func (s *Site) page(r *http.Request, title string) page {
	// A path refused is the zero localPath: the home page.
	next, _ := parseLocalPath(r.URL.RequestURI())
	return page{
		lang:      msg.English,
		title:     title,
		theme:     themeOf(r),
		next:      next,
		style:     assetOf(s.assets.StyleTag),
		script:    assetOf(s.assets.ScriptTag),
		devClient: assetOf(s.assets.DevClient),
	}
}
