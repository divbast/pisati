// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package auth_test

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/divbast/pisati/internal/core/auth"
	"github.com/divbast/pisati/internal/explain"
)

func TestParsePasswordBounds(t *testing.T) {
	for _, c := range []struct {
		name string
		in   string
		want error
	}{
		{"empty", "", auth.ErrPasswordTooShort},
		{"14 runes", strings.Repeat("a", 14), auth.ErrPasswordTooShort},
		{"15 runes", strings.Repeat("a", 15), nil},
		{"14 runes and a trailing space, which counts", strings.Repeat("a", 14) + " ", nil},
		{"a leading space and 14 runes, which counts", " " + strings.Repeat("a", 14), nil},
		{"15 spaces", strings.Repeat(" ", 15), nil},
		{"a tab and a NUL among 15 runes", strings.Repeat("a", 13) + "\t\x00", nil},
		{"14 emoji, 56 bytes", strings.Repeat("🔑", 14), auth.ErrPasswordTooShort},
		{"15 emoji, 60 bytes", strings.Repeat("🔑", 15), nil},
		{"14 \u00e9 as one rune each (NFC)", strings.Repeat("\u00e9", 14), auth.ErrPasswordTooShort},
		{"14 \u00e9 as two runes each (NFD), not normalised", strings.Repeat("e\u0301", 14), nil},
		{"1024 bytes", strings.Repeat("a", 1024), nil},
		{"1025 bytes", strings.Repeat("a", 1025), auth.ErrPasswordTooLong},
		{"256 emoji, 1024 bytes", strings.Repeat("🔑", 256), nil},
		{"257 emoji, 1028 bytes", strings.Repeat("🔑", 257), auth.ErrPasswordTooLong},
		{"1 MiB", strings.Repeat("a", 1<<20), auth.ErrPasswordTooLong},
		{"1025 bytes, not UTF-8: the length is checked first", strings.Repeat("\xff", 1025), auth.ErrPasswordTooLong},
		{"a byte that starts no rune", strings.Repeat("a", 15) + "\xff", auth.ErrPasswordNotUTF8},
		{"a truncated rune", strings.Repeat("a", 15) + "\xf0\x9f\x94", auth.ErrPasswordNotUTF8},
		{"a surrogate half", strings.Repeat("a", 15) + "\xed\xa0\x80", auth.ErrPasswordNotUTF8},
		{"an over-long encoding of /", strings.Repeat("a", 15) + "\xc0\xaf", auth.ErrPasswordNotUTF8},
		{"5 bytes, not UTF-8: refused as not UTF-8 before its length", "\xff\xfe\xfd\xfc\xfb", auth.ErrPasswordNotUTF8},
	} {
		_, err := auth.ParsePassword(c.in)
		switch {
		case c.want == nil && err != nil:
			t.Errorf("%s: ParsePassword = %v, want it accepted", c.name, err)
		case c.want != nil && !errors.Is(err, c.want):
			t.Errorf("%s: ParsePassword = %v, want %v", c.name, err, c.want)
		}
	}
}

func TestPasswordBoundsAreTheApprovedValues(t *testing.T) {
	if auth.MinPasswordRunes != 15 || auth.MaxPasswordBytes != 1024 {
		t.Errorf("bounds %d runes to %d bytes, want 15 runes to 1024 bytes", auth.MinPasswordRunes, auth.MaxPasswordBytes)
	}
}

func TestPasswordRedacts(t *testing.T) {
	const secret = "correct horse battery staple"
	p, err := auth.ParsePassword(secret)
	if err != nil {
		t.Fatalf("ParsePassword: %v", err)
	}
	type unexported struct{ pw auth.Password }
	type exported struct{ PW auth.Password }
	var shown []string
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%T"} {
		shown = append(shown,
			fmt.Sprintf(verb, p), fmt.Sprintf(verb, &p), fmt.Sprintf(verb, []auth.Password{p}),
			fmt.Sprintf(verb, unexported{pw: p}), fmt.Sprintf(verb, exported{PW: p}))
	}
	var text, js bytes.Buffer
	for _, log := range []*slog.Logger{slog.New(slog.NewTextHandler(&text, nil)), slog.New(slog.NewJSONHandler(&js, nil))} {
		log.Info("sign-in", "password", p, "unexported", unexported{pw: p}, "exported", exported{PW: p})
	}
	shown = append(shown, text.String(), js.String())
	for _, v := range []any{p, exported{PW: p}} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("json.Marshal(%T): %v", v, err)
		}
		shown = append(shown, string(b))
	}
	leaks := []string{secret, hex.EncodeToString([]byte(secret)), strings.ToUpper(hex.EncodeToString([]byte(secret))), fmt.Sprintf("%d", []byte(secret))}
	for _, s := range shown {
		for _, leak := range leaks {
			if strings.Contains(s, leak) {
				t.Errorf("the password shows in %q", s)
			}
		}
	}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%10.3s"} {
		if got := fmt.Sprintf(verb, p); got != "[redacted]" {
			t.Errorf("fmt.Sprintf(%q, p) = %q, want [redacted]", verb, got)
		}
	}
	if !strings.Contains(text.String(), "password=[redacted]") {
		t.Errorf("slog's text handler shows %q, want password=[redacted]", text.String())
	}
	if !strings.Contains(js.String(), `"password":"[redacted]"`) {
		t.Errorf("slog's JSON handler shows %q, want \"password\":\"[redacted]\"", js.String())
	}
}

// FuzzParsePassword checks ParsePassword's rule, that its errors are its
// fixed sentinels, and that a password it accepts keeps all of s, with
// nothing trimmed or normalised: CheckPassword finds s inside it.
func FuzzParsePassword(f *testing.F) {
	for _, s := range []string{
		"", strings.Repeat("a", 14), strings.Repeat("a", 15), " correct horse battery staple ",
		strings.Repeat("🔑", 15), strings.Repeat("e\u0301", 8), strings.Repeat("a", 1024),
		strings.Repeat("a", 1025), strings.Repeat("a", 15) + "\xff",
	} {
		f.Add(s)
	}
	messages := map[string]bool{
		auth.ErrPasswordTooShort.Error(): true,
		auth.ErrPasswordTooLong.Error():  true,
		auth.ErrPasswordNotUTF8.Error():  true,
	}
	f.Fuzz(func(t *testing.T, s string) {
		p, err := auth.ParsePassword(s)
		var want error
		switch {
		case len(s) > auth.MaxPasswordBytes:
			want = auth.ErrPasswordTooLong
		case !utf8.ValidString(s):
			want = auth.ErrPasswordNotUTF8
		case utf8.RuneCountInString(s) < auth.MinPasswordRunes:
			want = auth.ErrPasswordTooShort
		}
		if want != nil {
			if !errors.Is(err, want) {
				t.Fatalf("ParsePassword(%q) = %v, want %v", s, err, want)
			}
			if !messages[err.Error()] {
				t.Fatalf("ParsePassword(%q) says %q, not one of its fixed messages", s, err)
			}
			return
		}
		if err != nil {
			t.Fatalf("ParsePassword(%q) = %v, want it accepted", s, err)
		}
		if err := auth.CheckPassword(p, s); !errors.Is(err, auth.ErrContextWord) {
			t.Fatalf("CheckPassword(ParsePassword(%q), %q) = %v, want ErrContextWord", s, s, err)
		}
	})
}

// TestPasswordErrorsGiveTheBounds: the refusals' fixed texts give the
// bounds as the constants hold them, the size in the one form every
// message writes a size in, so that the texts cannot drift from them.
func TestPasswordErrorsGiveTheBounds(t *testing.T) {
	t.Parallel()

	for err, want := range map[error]string{
		auth.ErrPasswordTooShort: fmt.Sprintf("fewer than %d characters", auth.MinPasswordRunes),
		auth.ErrPasswordTooLong:  "longer than " + explain.Bytes(auth.MaxPasswordBytes),
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q does not say %q", err, want)
		}
	}
}
