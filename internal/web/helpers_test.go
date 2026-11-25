// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package web_test

import (
	"bufio"
	"crypto/rand"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"

	"github.com/divbast/pisati/internal/app"
	"github.com/divbast/pisati/internal/config"
	"github.com/divbast/pisati/internal/httpx"
	"github.com/divbast/pisati/internal/store"
	"github.com/divbast/pisati/internal/web"
)

// The site's address in every test but those that say otherwise.
const (
	origin = "https://blog.example.org"
	host   = "blog.example.org"
)

// The answers the cache policy gives.
const (
	publicPage = "public, max-age=300"
	noStore    = "no-store"
)

// pisati is the public site as a browser reaches it: SecurityHeaders,
// CrossOrigin, SessionMiddleware when it has sessions, then the site's
// own handler.
type pisati struct {
	t        *testing.T
	site     *web.Site
	h        http.Handler
	logs     *logBuffer
	sessions *app.Sessions
}

// options change what newPisati makes. The zero options serve origin,
// with the build's assets, the CSP-report limiter Pisati uses on the
// wall clock, and no sessions.
type options struct {
	base    string
	dev     config.Dev
	reports *httpx.LimiterOptions
	store   *store.Store
}

// newPisati returns the site o describes.
func newPisati(t *testing.T, o options) *pisati {
	t.Helper()
	if o.base == "" {
		o.base = origin
	}
	base := baseURL(t, o.base)
	assets, err := web.NewAssets(t.Context(), build(manifest), base, o.dev)
	if err != nil {
		t.Fatalf("NewAssets: %v", err)
	}
	limits := httpx.CSPReportLimiterOptions()
	if o.reports != nil {
		limits = *o.reports
	}
	reports, err := httpx.NewLimiter(limits, time.Now)
	if err != nil {
		t.Fatalf("NewLimiter: %v", err)
	}
	logs := &logBuffer{}
	log := slog.New(slog.NewJSONHandler(logs, nil))
	site, err := web.NewSite(assets, base, reports, httpx.NewClientIP(nil), log)
	if err != nil {
		t.Fatalf("NewSite: %v", err)
	}
	p := &pisati{t: t, site: site, logs: logs}
	h := site.Handler()
	if o.store != nil {
		p.sessions, err = app.NewSessions(o.store, time.Now, rand.Reader, base, log)
		if err != nil {
			t.Fatalf("NewSessions: %v", err)
		}
		h = web.SessionMiddleware(p.sessions, log)(h)
	}
	p.h = httpx.Chain(h, httpx.SecurityHeaders(base, config.HSTSHostOnly, o.dev), httpx.CrossOrigin(base))
	return p
}

// request is one request a test sends: a browser's, unless it says
// otherwise.
type request struct {
	method, target, body string
	// header holds the request's headers, beyond those a browser sends
	// with a form: Sec-Fetch-Site same-origin and the site's Origin, for
	// any method but GET and HEAD. A header set to "" is left out.
	header map[string]string
	// length, when not 0, is the Content-Length the request declares;
	// -1 sends the body chunked, with no length.
	length  int64
	cookies []*http.Cookie
	remote  string
	// rawQuery, when not empty, is the query exactly as sent, unescaped.
	rawQuery string
	// host, when not empty, is the Host the request names, which is
	// otherwise the site's.
	host string
}

// do sends req through the site, and returns the answer.
func (p *pisati) do(req request) *httptest.ResponseRecorder {
	p.t.Helper()
	if req.method == "" {
		req.method = http.MethodGet
	}
	r := httptest.NewRequestWithContext(p.t.Context(), req.method, req.target, strings.NewReader(req.body))
	r.Host = host
	if req.host != "" {
		r.Host = req.host
	}
	if req.remote != "" {
		r.RemoteAddr = req.remote
	}
	if req.method != http.MethodGet && req.method != http.MethodHead {
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		r.Header.Set("Origin", origin)
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range req.header {
		if v == "" {
			r.Header.Del(k)
			continue
		}
		r.Header.Set(k, v)
	}
	switch {
	case req.length < 0:
		r.ContentLength = -1
		r.Body = io.NopCloser(strings.NewReader(req.body))
	case req.length > 0:
		r.ContentLength = req.length
	}
	for _, c := range req.cookies {
		r.AddCookie(c)
	}
	if req.rawQuery != "" {
		r.URL.RawQuery = req.rawQuery
		r.RequestURI = r.URL.Path + "?" + req.rawQuery
	}
	w := httptest.NewRecorder()
	p.h.ServeHTTP(w, r)
	return w
}

// page sends a GET for target, and fails the test unless it is answered
// with status and an HTML page, which it returns parsed.
func (p *pisati) page(target string, status int, cookies ...*http.Cookie) (*httptest.ResponseRecorder, *html.Node) {
	p.t.Helper()
	w := p.do(request{target: target, cookies: cookies})
	if w.Code != status {
		p.t.Fatalf("GET %s = %d, want %d; body:\n%s", target, w.Code, status, w.Body.String())
	}
	return w, parse(p.t, w)
}

// records returns each JSON record logged since the site was made.
func (p *pisati) records() []map[string]any {
	p.t.Helper()
	var out []map[string]any
	sc := bufio.NewScanner(strings.NewReader(p.logs.String()))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		var rec map[string]any
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			p.t.Fatalf("a log line is not JSON: %v: %s", err, sc.Text())
		}
		out = append(out, rec)
	}
	return out
}

// parse returns w's body parsed as HTML, failing the test unless it is
// sent as HTML.
func parse(t *testing.T, w *httptest.ResponseRecorder) *html.Node {
	t.Helper()
	if ct := w.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Fatalf("Content-Type %q, want text/html; charset=utf-8", ct)
	}
	doc, err := html.Parse(strings.NewReader(w.Body.String()))
	if err != nil {
		t.Fatalf("the page is not HTML: %v", err)
	}
	return doc
}

// all returns every element under n named tag, in document order.
func all(n *html.Node, tag string) []*html.Node {
	var out []*html.Node
	for d := range n.Descendants() {
		if d.Type == html.ElementNode && d.Data == tag {
			out = append(out, d)
		}
	}
	return out
}

// attr returns n's attribute key, and whether n has it.
func attr(n *html.Node, key string) (string, bool) {
	for _, a := range n.Attr {
		if a.Namespace == "" && a.Key == key {
			return a.Val, true
		}
	}
	return "", false
}

// text returns the text n holds, with runs of space made one.
func text(n *html.Node) string {
	var b strings.Builder
	for d := range n.Descendants() {
		if d.Type == html.TextNode {
			b.WriteString(d.Data)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// byAttr returns the elements named tag under n whose attribute key is
// val.
func byAttr(n *html.Node, tag, key, val string) []*html.Node {
	var out []*html.Node
	for _, e := range all(n, tag) {
		if v, ok := attr(e, key); ok && v == val {
			out = append(out, e)
		}
	}
	return out
}

// one returns the one element in list, failing the test unless there is
// exactly one.
func one(t *testing.T, list []*html.Node, what string) *html.Node {
	t.Helper()
	if len(list) != 1 {
		t.Fatalf("%d of %s, want exactly one", len(list), what)
	}
	return list[0]
}

// nonceRE finds the nonce in a Content-Security-Policy.
var nonceRE = regexp.MustCompile(`'nonce-([^']+)'`)

// nonceOf returns the nonce w's policy allows scripts by.
func nonceOf(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	m := nonceRE.FindStringSubmatch(w.Header().Get("Content-Security-Policy"))
	if m == nil {
		t.Fatalf("the policy %q has no nonce", w.Header().Get("Content-Security-Policy"))
	}
	return m[1]
}

// wantCache fails the test unless w has Cache-Control cc, and, for the
// public answer, Vary: Cookie.
func wantCache(t *testing.T, w *httptest.ResponseRecorder, cc, when string) {
	t.Helper()
	if got := w.Header().Get("Cache-Control"); got != cc {
		t.Errorf("%s: Cache-Control %q, want %q", when, got, cc)
	}
	vary := strings.Join(w.Header().Values("Vary"), ",")
	if cc == publicPage && !strings.Contains(vary, "Cookie") {
		t.Errorf("%s: Vary %q, want Cookie in it", when, vary)
	}
}
