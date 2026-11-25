// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package web_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"golang.org/x/net/html"

	"github.com/divbast/pisati/internal/web/msg"
)

// wantNoInlineCode fails the test if doc has a style element or
// attribute, an event-handler attribute, or a script without a src: the
// policy refuses each, so none may be sent.
func wantNoInlineCode(t *testing.T, doc *html.Node, body string) {
	t.Helper()
	if strings.Contains(body, "style=") {
		t.Error("the page holds style=")
	}
	for n := range doc.Descendants() {
		if n.Type != html.ElementNode {
			continue
		}
		if n.Data == "style" {
			t.Error("the page has a style element")
		}
		for _, a := range n.Attr {
			if a.Key == "style" || strings.HasPrefix(a.Key, "on") {
				t.Errorf("<%s> has the attribute %s=%q", n.Data, a.Key, a.Val)
			}
		}
		if _, ok := attr(n, "src"); n.Data == "script" && !ok {
			t.Errorf("an inline script: %q", text(n))
		}
	}
}

func TestLoginPage(t *testing.T) {
	t.Parallel()

	p := newPisati(t, options{})
	w, doc := p.page("/login", http.StatusOK)

	if got := w.Header().Get("Cross-Origin-Embedder-Policy"); got != "require-corp" {
		t.Errorf("Cross-Origin-Embedder-Policy %q, want require-corp", got)
	}
	root := one(t, all(doc, "html"), "html elements")
	if lang, _ := attr(root, "lang"); lang != "en" {
		t.Errorf("<html lang=%q>, want en", lang)
	}
	if got, want := text(one(t, all(doc, "title"), "titles")), msg.PageTitle(msg.English, msg.SignInTitle(msg.English)); got != want {
		t.Errorf("title %q, want %q", got, want)
	}
	if got := text(one(t, all(doc, "h1"), "headings")); got != msg.SignInTitle(msg.English) {
		t.Errorf("heading %q, want %q", got, msg.SignInTitle(msg.English))
	}

	form := one(t, byAttr(doc, "form", "action", "/login"), "forms posting to /login")
	if method, _ := attr(form, "method"); method != "post" {
		t.Errorf("the sign-in form's method is %q, want post", method)
	}
	fields := map[string]struct{ kind, autocomplete, label string }{
		"username": {kind: "text", autocomplete: "username", label: msg.UsernameLabel(msg.English)},
		"password": {kind: "password", autocomplete: "current-password", label: msg.PasswordLabel(msg.English)},
	}
	for name, want := range fields {
		input := one(t, byAttr(form, "input", "name", name), "inputs named "+name)
		for key, val := range map[string]string{"type": want.kind, "autocomplete": want.autocomplete} {
			if got, _ := attr(input, key); got != val {
				t.Errorf("the %s field's %s is %q, want %q", name, key, got, val)
			}
		}
		if _, ok := attr(input, "required"); !ok {
			t.Errorf("the %s field is not required", name)
		}
		id, _ := attr(input, "id")
		label := one(t, byAttr(form, "label", "for", id), "labels for "+name)
		if got := text(label); got != want.label {
			t.Errorf("the %s field's label is %q, want %q", name, got, want.label)
		}
	}

	// Every script carries the response's nonce and the integrity of
	// what it loads, and the stylesheet its integrity.
	nonce := nonceOf(t, w)
	files := build(manifest)
	scripts := all(doc, "script")
	if len(scripts) == 0 {
		t.Fatal("the page loads no script")
	}
	for _, s := range scripts {
		src, _ := attr(s, "src")
		got, _ := attr(s, "nonce")
		integrity, _ := attr(s, "integrity")
		if got != nonce || src != "/static/"+script || integrity != sri(files[script].Data) {
			t.Errorf("<script src=%q nonce=%q integrity=%q>, want src /static/%s, the nonce %q and %q",
				src, got, integrity, script, nonce, sri(files[script].Data))
		}
	}
	link := one(t, byAttr(doc, "link", "rel", "stylesheet"), "stylesheets")
	if integrity, _ := attr(link, "integrity"); integrity != sri(files[stylesheet].Data) {
		t.Errorf("the stylesheet's integrity is %q, want %q", integrity, sri(files[stylesheet].Data))
	}
	wantNoInlineCode(t, doc, w.Body.String())
}

func TestLoginWorksWithoutJS(t *testing.T) {
	t.Parallel()

	p := newPisati(t, options{})
	_, doc := p.page("/login", http.StatusOK)

	// What a browser with no script posts: the form's own fields, to its
	// own action, by its own method, sent by its submit button.
	form := one(t, byAttr(doc, "form", "action", "/login"), "forms posting to /login")
	one(t, byAttr(form, "button", "type", "submit"), "submit buttons in the sign-in form")
	method, _ := attr(form, "method")
	action, _ := attr(form, "action")
	// What the visitor typed, which the answer must not repeat.
	const typed = "typed into the second field"
	values := url.Values{}
	for _, input := range all(form, "input") {
		name, _ := attr(input, "name")
		values.Set(name, map[string]string{"username": "someone", "password": typed}[name])
	}
	w := p.do(request{method: strings.ToUpper(method), target: action, body: values.Encode()})

	if w.Code != http.StatusNotImplemented {
		t.Fatalf("posting the sign-in form = %d, want 501", w.Code)
	}
	doc = parse(t, w)
	if got := text(one(t, all(doc, "h1"), "headings")); got != msg.NotAvailableTitle(msg.English) {
		t.Errorf("heading %q, want %q", got, msg.NotAvailableTitle(msg.English))
	}
	if !strings.Contains(text(doc), msg.NotAvailableBody(msg.English)) {
		t.Errorf("the page does not say %q", msg.NotAvailableBody(msg.English))
	}
	if strings.Contains(w.Body.String(), typed) || strings.Contains(w.Body.String(), "someone") {
		t.Error("the answer repeats what was posted")
	}
	wantCache(t, w, noStore, "POST /login")
}

func TestHomePage(t *testing.T) {
	t.Parallel()

	p := newPisati(t, options{})
	w, doc := p.page("/", http.StatusOK)

	if got := text(one(t, all(doc, "h1"), "headings")); got != msg.SiteName(msg.English) {
		t.Errorf("heading %q, want %q", got, msg.SiteName(msg.English))
	}
	if got := text(one(t, all(doc, "title"), "titles")); got != msg.SiteName(msg.English) {
		t.Errorf("title %q, want %q", got, msg.SiteName(msg.English))
	}
	link := one(t, byAttr(doc, "a", "href", "/login"), "links to /login")
	if got := text(link); got != msg.SignInLink(msg.English) {
		t.Errorf("the link to /login reads %q, want %q", got, msg.SignInLink(msg.English))
	}
	if got := w.Header().Get("Cross-Origin-Embedder-Policy"); got != "" {
		t.Errorf("Cross-Origin-Embedder-Policy %q on the home page, which will show posts", got)
	}
	wantNoInlineCode(t, doc, w.Body.String())
}

func TestLayout(t *testing.T) {
	t.Parallel()

	p := newPisati(t, options{})
	_, doc := p.page("/login", http.StatusOK)

	body := one(t, all(doc, "body"), "bodies")
	var children []*html.Node
	for c := range body.ChildNodes() {
		if c.Type == html.ElementNode {
			children = append(children, c)
		}
	}
	if len(children) != 3 || children[0].Data != "a" || children[1].Data != "header" || children[2].Data != "main" {
		t.Fatalf("the body holds %d elements, want the skip link, the header and main", len(children))
	}
	skip := children[0]
	if href, _ := attr(skip, "href"); href != "#main" || text(skip) != msg.SkipToContent(msg.English) {
		t.Errorf("the skip link goes to %q and reads %q, want #main and %q", href, text(skip), msg.SkipToContent(msg.English))
	}
	if id, _ := attr(children[2], "id"); id != "main" {
		t.Errorf("main's id is %q, want main", id)
	}
	home := one(t, byAttr(children[1], "a", "href", "/"), "links home in the header")
	if text(home) != msg.SiteName(msg.English) {
		t.Errorf("the header's link home reads %q, want %q", text(home), msg.SiteName(msg.English))
	}
	one(t, byAttr(children[1], "form", "action", "/_/theme"), "theme forms in the header")
}

func TestLayoutInDevMode(t *testing.T) {
	t.Parallel()

	p := newPisati(t, options{base: "http://localhost:8080", dev: devConfig(t, "http://localhost:5174")})
	w, doc := p.page("/login", http.StatusOK)
	nonce := nonceOf(t, w)

	meta := one(t, byAttr(doc, "meta", "property", "csp-nonce"), "csp-nonce meta elements")
	if got, _ := attr(meta, "nonce"); got != nonce {
		t.Errorf("the csp-nonce meta carries %q, want the response's nonce %q", got, nonce)
	}
	var srcs []string
	for _, s := range all(doc, "script") {
		src, _ := attr(s, "src")
		srcs = append(srcs, src)
		if got, _ := attr(s, "nonce"); got != nonce {
			t.Errorf("<script src=%q> carries the nonce %q, want %q", src, got, nonce)
		}
		if _, ok := attr(s, "crossorigin"); !ok {
			t.Errorf("<script src=%q> lacks crossorigin, which an isolated page needs for the dev server", src)
		}
		if _, ok := attr(s, "integrity"); ok {
			t.Errorf("<script src=%q> has an integrity value, which the dev server's changing files cannot keep", src)
		}
	}
	if got := strings.Join(srcs, " "); got != "http://localhost:5174/static/@vite/client http://localhost:5174/static/web/ts/main.ts" {
		t.Errorf("the scripts are %s, want Vite's client, then main.ts, from the dev server", got)
	}
	link := one(t, byAttr(doc, "link", "rel", "stylesheet"), "stylesheets")
	if href, _ := attr(link, "href"); href != "http://localhost:5174/static/web/css/layers.css" {
		t.Errorf("the stylesheet is %q, want layers.css from the dev server", href)
	}
	if _, ok := attr(link, "crossorigin"); !ok {
		t.Error("the stylesheet lacks crossorigin")
	}
	wantNoInlineCode(t, doc, w.Body.String())
}

func TestTemplatesEscape(t *testing.T) {
	t.Parallel()

	const markup = `"><script>alert(1)</script><img src=x onerror=alert(2)>`
	tests := []struct {
		name     string
		req      request
		wantNext string
	}{
		{
			name:     "an escaped query",
			req:      request{target: "/nothing-here?q=" + url.QueryEscape(markup)},
			wantNext: "/nothing-here?q=" + url.QueryEscape(markup),
		},
		{
			// As a client may send it, unescaped: Go's server takes such
			// a query as it comes.
			name:     "a raw query",
			req:      request{target: "/nothing-here", rawQuery: markup},
			wantNext: "/nothing-here?" + markup,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			p := newPisati(t, options{})
			tt.req.header = map[string]string{"Cookie": `pisati-theme="` + markup}
			w := p.do(tt.req)
			if w.Code != http.StatusNotFound {
				t.Fatalf("GET = %d, want 404", w.Code)
			}
			doc := parse(t, w)
			if n := len(all(doc, "script")); n != 1 {
				t.Errorf("the page has %d scripts, want only its own", n)
			}
			if n := len(all(doc, "img")); n != 0 {
				t.Errorf("the page has %d images, made from the request", n)
			}
			root := one(t, all(doc, "html"), "html elements")
			if theme, _ := attr(root, "data-theme"); theme != "auto" {
				t.Errorf("data-theme %q from a cookie holding markup, want auto", theme)
			}
			next := one(t, byAttr(doc, "input", "name", "next"), "next fields")
			if got, _ := attr(next, "value"); got != tt.wantNext {
				t.Errorf("the theme form's next is %q, want %q, the request's path and query as they came", got, tt.wantNext)
			}
			wantNoInlineCode(t, doc, w.Body.String())
		})
	}
}

func TestNotFoundPage(t *testing.T) {
	t.Parallel()

	p := newPisati(t, options{})
	for _, target := range []string{"/nothing-here", "/login/", "/_/theme", "/static"} {
		w := p.do(request{target: target})
		if target == "/static" {
			// The mux sends /static on to /static/ itself.
			if w.Code != http.StatusTemporaryRedirect {
				t.Errorf("GET /static = %d, want the mux's 307", w.Code)
			}
			continue
		}
		if w.Code != http.StatusNotFound {
			t.Fatalf("GET %s = %d, want 404", target, w.Code)
		}
		doc := parse(t, w)
		if got := text(one(t, all(doc, "h1"), "headings")); got != msg.NotFoundTitle(msg.English) {
			t.Errorf("GET %s: heading %q, want %q", target, got, msg.NotFoundTitle(msg.English))
		}
		if got, want := text(one(t, all(doc, "title"), "titles")), msg.PageTitle(msg.English, msg.NotFoundTitle(msg.English)); got != want {
			t.Errorf("GET %s: title %q, want %q", target, got, want)
		}
		if !strings.Contains(text(doc), msg.NotFoundBody(msg.English)) {
			t.Errorf("GET %s: the page does not say %q", target, msg.NotFoundBody(msg.English))
		}
		one(t, byAttr(one(t, all(doc, "main"), "mains"), "a", "href", "/"), "links home in main")
		wantCache(t, w, publicPage, "GET "+target)
		wantNoInlineCode(t, doc, w.Body.String())
	}

	w := p.do(request{method: http.MethodHead, target: "/nothing-here"})
	if w.Code != http.StatusNotFound {
		t.Errorf("HEAD /nothing-here = %d, want 404", w.Code)
	}
	wantCache(t, w, publicPage, "HEAD /nothing-here")
}
