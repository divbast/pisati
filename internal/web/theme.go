// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package web

import (
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/divbast/pisati/internal/app"
	"github.com/divbast/pisati/internal/httpx"
)

const (
	// themeCookieName is the cookie that remembers a visitor's theme.
	themeCookieName = "pisati-theme"
	// themeMaxAge is how long the theme cookie lasts: a year, in seconds.
	themeMaxAge = 365 * 24 * 60 * 60
	// maxNext is the longest next the theme switch takes: 8 KiB.
	maxNext = 8 << 10
)

// The theme form's fields.
const (
	themeField app.Field = "theme"
	nextField  app.Field = "next"
)

// theme is the colour scheme a page asks for, as its <html data-theme>
// names it. The zero theme is themeAuto, which follows the visitor's
// system, and is a page's theme when its visitor chose none.
type theme uint8

const (
	themeAuto theme = iota
	themeLight
	themeDark
)

// String returns t as data-theme and the theme cookie spell it: auto,
// light or dark, and auto for a theme outside the set.
func (t theme) String() string {
	switch t {
	case themeLight:
		return "light"
	case themeDark:
		return "dark"
	case themeAuto:
	}
	return "auto"
}

// parseTheme returns the theme s spells, exactly, and whether it spells
// one.
func parseTheme(s string) (theme, bool) {
	for _, t := range [...]theme{themeAuto, themeLight, themeDark} {
		if s == t.String() {
			return t, true
		}
	}
	return themeAuto, false
}

// themeOf returns the theme r's theme cookie holds: themeAuto without
// one, or with one that spells no theme.
func themeOf(r *http.Request) theme {
	c, err := r.Cookie(themeCookieName)
	if err != nil {
		return themeAuto
	}
	t, _ := parseTheme(c.Value)
	return t
}

// themeCookie returns the cookie that remembers t for a year: HttpOnly,
// SameSite=Lax, Path=/, and Secure when the site is https.
func (s *Site) themeCookie(t theme) *http.Cookie {
	//nolint:gosec // G124: Secure is false only for a plain-http base URL, which config.ParseBaseURL allows on localhost alone, for development, where Safari refuses a Secure cookie.
	return &http.Cookie{
		Name:     themeCookieName,
		Value:    t.String(),
		Path:     "/",
		MaxAge:   themeMaxAge,
		Secure:   s.secure,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	}
}

// setTheme answers the theme form: it sets the theme cookie to the theme
// posted, light, dark or auto, and sends the browser back to next with
// 303 See Other. A theme it does not know, or none, changes nothing, and
// the browser goes back all the same. A next the rule safeNext follows
// refuses sends the browser to / instead. A form it cannot read is
// answered 400, and one over its limit 413.
func (s *Site) setTheme(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		if httpx.RefuseTooLarge(w, err) {
			return
		}
		s.WriteError(w, r, &app.Invalid{Field: themeField, Reason: app.Malformed})
		return
	}
	if t, ok := parseTheme(r.PostForm.Get(string(themeField))); ok {
		http.SetCookie(w, s.themeCookie(t))
	}
	next, ok := safeNext(r.PostForm.Get(string(nextField)))
	if !ok {
		next = "/"
	}
	w.Header().Set("Location", locationOf(next))
	w.WriteHeader(http.StatusSeeOther)
}

// safeNext returns s, and whether it is a path on this site a browser may
// be sent back to: a single / first, so it names no scheme or host, and
// neither // nor /\ first, which a browser would read as another host;
// valid UTF-8, with no control character anywhere, as browsers drop some
// of them before they read a URL; and at most 8 KiB.
func safeNext(s string) (string, bool) {
	if len(s) == 0 || len(s) > maxNext || s[0] != '/' {
		return "", false
	}
	if len(s) > 1 && (s[1] == '/' || s[1] == '\\') {
		return "", false
	}
	if !utf8.ValidString(s) || strings.ContainsFunc(s, unicode.IsControl) {
		return "", false
	}
	return s, true
}

// locationOf returns next, a path safeNext took, as a Location header
// carries it: each byte a URL may not hold as it is, a space or a byte of
// a character outside ASCII among them, percent-encoded, as is a % that
// does not begin an escape. A browser reads it as the same path.
func locationOf(next string) string {
	var b strings.Builder
	for i := 0; i < len(next); i++ {
		c := next[i]
		switch {
		case c == '%' && i+2 < len(next) && isHex(next[i+1]) && isHex(next[i+2]):
			b.WriteByte(c)
		case c != '%' && c < utf8.RuneSelf && strings.IndexByte(uriChars, c) >= 0:
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(upperHex[c>>4])
			b.WriteByte(upperHex[c&0xf])
		}
	}
	return b.String()
}

// uriChars are the characters a URL holds as they are, % aside: RFC
// 3986's unreserved and reserved characters.
const uriChars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~:/?#[]@!$&'()*+,;="

// upperHex are the digits of a percent-encoded byte.
const upperHex = "0123456789ABCDEF"

// isHex reports whether c is a hexadecimal digit.
func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}
