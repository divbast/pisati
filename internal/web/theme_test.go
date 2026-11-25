// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package web_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/divbast/pisati/internal/httpx"
	"github.com/divbast/pisati/internal/web/msg"
)

// switchTheme posts the theme form as a browser without script does.
func switchTheme(p *pisati, theme, next string) *httptest.ResponseRecorder {
	p.t.Helper()
	form := url.Values{"next": {next}}
	if theme != "" {
		form.Set("theme", theme)
	}
	return p.do(request{method: http.MethodPost, target: "/_/theme", body: form.Encode()})
}

func TestThemeToggleNoJS(t *testing.T) {
	t.Parallel()

	t.Run("each theme sets the cookie and goes back", func(t *testing.T) {
		t.Parallel()

		p := newPisati(t, options{})
		for _, theme := range []string{"light", "dark", "auto"} {
			w := switchTheme(p, theme, "/login?a=1")
			if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login?a=1" {
				t.Errorf("theme %s: %d to %q, want 303 to /login?a=1", theme, w.Code, w.Header().Get("Location"))
			}
			want := "pisati-theme=" + theme + "; Path=/; Max-Age=31536000; HttpOnly; Secure; SameSite=Lax"
			if got := w.Header().Values("Set-Cookie"); len(got) != 1 || got[0] != want {
				t.Errorf("theme %s: Set-Cookie %q, want exactly %q", theme, got, want)
			}
			wantCache(t, w, noStore, "the theme switch")
		}
	})

	t.Run("over plain http on localhost, the cookie is not Secure", func(t *testing.T) {
		t.Parallel()

		p := newPisati(t, options{base: "http://localhost:8080"})
		w := p.do(request{
			method: http.MethodPost, target: "/_/theme", body: "theme=dark&next=%2F",
			header: map[string]string{"Origin": "http://localhost:8080"},
		})
		want := "pisati-theme=dark; Path=/; Max-Age=31536000; HttpOnly; SameSite=Lax"
		if got := w.Header().Values("Set-Cookie"); len(got) != 1 || got[0] != want {
			t.Errorf("Set-Cookie %q, want exactly %q", got, want)
		}
	})

	t.Run("pages show the theme the cookie holds", func(t *testing.T) {
		t.Parallel()

		p := newPisati(t, options{})
		for cookie, want := range map[string]string{"light": "light", "dark": "dark", "auto": "auto", "purple": "auto", "": "auto"} {
			var cookies []*http.Cookie
			if cookie != "" {
				cookies = append(cookies, requestCookie("pisati-theme", cookie))
			}
			_, doc := p.page("/login", http.StatusOK, cookies...)
			root := one(t, all(doc, "html"), "html elements")
			if got, _ := attr(root, "data-theme"); got != want {
				t.Errorf("with the cookie %q: data-theme %q, want %q", cookie, got, want)
			}
			form := one(t, byAttr(doc, "form", "action", "/_/theme"), "theme forms")
			for _, b := range all(form, "button") {
				value, _ := attr(b, "value")
				if pressed, _ := attr(b, "aria-pressed"); pressed != map[bool]string{true: "true", false: "false"}[value == want] {
					t.Errorf("with the cookie %q: the %s button has aria-pressed=%q", cookie, value, pressed)
				}
			}
		}
	})

	t.Run("the form works on its own", func(t *testing.T) {
		t.Parallel()

		p := newPisati(t, options{})
		_, doc := p.page("/login?from=here", http.StatusOK)
		form := one(t, byAttr(doc, "form", "action", "/_/theme"), "theme forms")
		if method, _ := attr(form, "method"); method != "post" {
			t.Errorf("the theme form's method is %q, want post", method)
		}
		if _, ok := attr(form, "data-theme-form"); !ok {
			t.Error("the theme form lacks data-theme-form, which theme.ts enhances")
		}
		if label, _ := attr(form, "aria-label"); label != msg.ThemeLabel(msg.English) {
			t.Errorf("the theme form is named %q, want %q", label, msg.ThemeLabel(msg.English))
		}
		next := one(t, byAttr(form, "input", "name", "next"), "next fields")
		if kind, _ := attr(next, "type"); kind != "hidden" {
			t.Errorf("the next field is %q, want hidden", kind)
		}
		if value, _ := attr(next, "value"); value != "/login?from=here" {
			t.Errorf("the next field holds %q, want the page's own path and query", value)
		}
		buttons := all(form, "button")
		labels := map[string]string{
			"light": msg.ThemeLight(msg.English), "dark": msg.ThemeDark(msg.English), "auto": msg.ThemeAuto(msg.English),
		}
		var values []string
		for _, b := range buttons {
			value, _ := attr(b, "value")
			values = append(values, value)
			if name, _ := attr(b, "name"); name != "theme" {
				t.Errorf("the %s button's name is %q, want theme", value, name)
			}
			if kind, _ := attr(b, "type"); kind != "submit" {
				t.Errorf("the %s button's type is %q, want submit", value, kind)
			}
			if text(b) != labels[value] {
				t.Errorf("the %s button reads %q, want %q", value, text(b), labels[value])
			}
			icon := one(t, all(b, "svg"), "icons in the "+value+" button")
			if hidden, _ := attr(icon, "aria-hidden"); hidden != "true" {
				t.Errorf("the %s button's icon is not aria-hidden", value)
			}
		}
		if got := strings.Join(values, " "); got != "light dark auto" {
			t.Errorf("the buttons are %s, want light dark auto", got)
		}
	})

	t.Run("an open redirect is refused", func(t *testing.T) {
		t.Parallel()

		p := newPisati(t, options{})
		for _, next := range []string{
			"", "//evil.example/", "/\\evil.example/", "https://evil.example/", "javascript:alert(1)",
			"evil.example", "\\\\evil.example", "/\t/evil.example", "/\n/evil.example", "/x\x00", "/x\x7f",
			"/x\u0085", "/\xff", " /x", "/" + strings.Repeat("a", 8<<10),
		} {
			w := switchTheme(p, "dark", next)
			if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
				t.Errorf("next %q: %d to %q, want 303 to /", next, w.Code, w.Header().Get("Location"))
			}
		}
		// 8 KiB is the longest next taken.
		longest := "/" + strings.Repeat("a", 8<<10-1)
		if w := switchTheme(p, "dark", longest); w.Header().Get("Location") != longest {
			t.Errorf("a next of 8 KiB went to %.20q…, want itself", w.Header().Get("Location"))
		}
	})

	t.Run("a next is sent back as a URL a header can hold", func(t *testing.T) {
		t.Parallel()

		p := newPisati(t, options{})
		w := switchTheme(p, "dark", "/posts/naïve café?x=a b")
		if got, want := w.Header().Get("Location"), "/posts/na%C3%AFve%20caf%C3%A9?x=a%20b"; got != want {
			t.Errorf("Location %q, want %q", got, want)
		}
	})

	t.Run("an unknown or missing theme changes nothing", func(t *testing.T) {
		t.Parallel()

		p := newPisati(t, options{})
		for _, theme := range []string{"purple", "", "Dark", "dark "} {
			w := switchTheme(p, theme, "/login")
			if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" {
				t.Errorf("theme %q: %d to %q, want 303 to /login", theme, w.Code, w.Header().Get("Location"))
			}
			if got := w.Header().Values("Set-Cookie"); len(got) != 0 {
				t.Errorf("theme %q: Set-Cookie %q, want none", theme, got)
			}
			wantCache(t, w, noStore, "an unknown theme")
		}
	})

	t.Run("a form that is no form is refused", func(t *testing.T) {
		t.Parallel()

		p := newPisati(t, options{})
		w := p.do(request{method: http.MethodPost, target: "/_/theme", body: "theme=dark&next=%zz"})
		if w.Code != http.StatusBadRequest {
			t.Errorf("a malformed form = %d, want 400", w.Code)
		}
		if got := w.Header().Values("Set-Cookie"); len(got) != 0 {
			t.Errorf("a malformed form set %q", got)
		}
		wantCache(t, w, noStore, "a malformed form")

		big := "theme=dark&next=%2F&pad=" + strings.Repeat("a", int(httpx.FormLimit))
		if w := p.do(request{method: http.MethodPost, target: "/_/theme", body: big, length: -1}); w.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("a form over 64 KiB = %d, want 413", w.Code)
		}
	})
}

// resolve returns where a browser on origin goes for location: it reads
// a backslash as a slash and drops tabs and line breaks, as the URL
// standard says, then resolves what is left against the page.
func resolve(t *testing.T, location string) *url.URL {
	t.Helper()
	cleaned := strings.NewReplacer("\\", "/", "\t", "", "\n", "", "\r", "").Replace(location)
	cleaned = strings.Trim(cleaned, "\x00\x01\x02\x03\x04\x05\x06\x07\x08\x0b\x0c\x0e\x0f\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1b\x1c\x1d\x1e\x1f ")
	ref, err := url.Parse(cleaned)
	if err != nil {
		t.Fatalf("Location %q is no URL: %v", location, err)
	}
	base, err := url.Parse(origin + "/login")
	if err != nil {
		t.Fatal(err)
	}
	return base.ResolveReference(ref)
}

func FuzzThemeNext(f *testing.F) {
	for _, seed := range []string{
		"/", "/login?a=1#top", "//evil.example", "/\\evil.example", "https://evil.example", "/%2F%2Fevil.example",
		"/..//evil.example", "/./\\evil", "\\/evil", "/\t/evil", "/a b", "/naïve", "/ x", "http:evil.example",
		"/%5C%5Cevil", "/?next=//evil", "#frag", "?q", "/" + strings.Repeat("x", 8<<10),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, next string) {
		p := newPisati(t, options{})
		w := switchTheme(p, "dark", next)
		if w.Code != http.StatusSeeOther {
			t.Fatalf("next %q: %d, want 303", next, w.Code)
		}
		loc := w.Header().Get("Location")
		for _, r := range loc {
			if r < 0x21 || r > 0x7e {
				t.Fatalf("next %q: Location %q holds %U, which a header should not carry raw", next, loc, r)
			}
		}
		if !utf8.ValidString(loc) || !strings.HasPrefix(loc, "/") {
			t.Fatalf("next %q: Location %q is not a path", next, loc)
		}
		got := resolve(t, loc)
		if got.Scheme != "https" || got.Host != host || got.User != nil {
			t.Fatalf("next %q: Location %q takes a browser to %s, off the site", next, loc, got)
		}
	})
}
