// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package auth_test

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/divbast/pisati/internal/core/auth"
)

// The known answers come from Argon2's reference implementation, its
// argon2 command (release 20190702), and not from the library Pisati uses:
//
//	printf '%s' 'correct horse battery staple' | argon2 'pisati-kat-salt!' -id -t 3 -k 65536 -p 4 -l 32 -e
//	printf '%s' 'correct horse battery staple' | argon2 'pisati-kat-salt!' -id -t 1 -k 64 -p 1 -l 32 -e
const (
	katPhrase  = "correct horse battery staple"
	katSalt    = "pisati-kat-salt!"
	katDefault = "$argon2id$v=19$m=65536,t=3,p=4$cGlzYXRpLWthdC1zYWx0IQ$zz2P4sjGxYbCPvEs7VKcnWKwgM5UFatLJzS4VzXEeyg"
	katSmall   = "$argon2id$v=19$m=64,t=1,p=1$cGlzYXRpLWthdC1zYWx0IQ$JGmZyYl+qr6fuAtAQc9JgcKhIod/JMvdoEk9q6LrRwI"
	// The segments of a valid hash, to build others from.
	katSaltB64   = "cGlzYXRpLWthdC1zYWx0IQ"
	katDigestB64 = "JGmZyYl+qr6fuAtAQc9JgcKhIod/JMvdoEk9q6LrRwI"
)

// small is cheap to hash with, so that most tests run quickly.
var small = auth.Params{Memory: 64, Time: 1, Threads: 1}

func password(t testing.TB, s string) auth.Password {
	t.Helper()
	p, err := auth.ParsePassword(s)
	if err != nil {
		t.Fatalf("ParsePassword: %v", err)
	}
	return p
}

func parseHash(t testing.TB, s string) auth.Hash {
	t.Helper()
	h, err := auth.ParseHash(s)
	if err != nil {
		t.Fatalf("ParseHash(%q): %v", s, err)
	}
	return h
}

func hashPassword(t testing.TB, p auth.Password, s auth.Salt, params auth.Params) auth.Hash {
	t.Helper()
	h, err := auth.HashPassword(p, s, params)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	return h
}

func TestHashFormat(t *testing.T) {
	if got, want := auth.DefaultParams(), (auth.Params{Memory: 65536, Time: 3, Threads: 4}); got != want {
		t.Errorf("DefaultParams() = %+v, want RFC 9106's second recommended setting %+v", got, want)
	}
	salt := auth.Salt([]byte(katSalt))
	h := hashPassword(t, password(t, katPhrase), salt, auth.DefaultParams())
	got := h.PHC()
	format := regexp.MustCompile(`^\$argon2id\$v=19\$m=65536,t=3,p=4\$[A-Za-z0-9+/]{22}\$[A-Za-z0-9+/]{43}$`)
	if !format.MatchString(got) {
		t.Errorf("HashPassword(…, DefaultParams()).PHC() = %q, want %v", got, format)
	}
	if got != katDefault {
		t.Errorf("HashPassword(…, DefaultParams()).PHC() = %q, want the reference implementation's %q", got, katDefault)
	}
	if got, want := h.String(), "$argon2id$v=19$m=65536,t=3,p=4$[redacted]"; got != want {
		t.Errorf("HashPassword(…, DefaultParams()).String() = %q, want %q", got, want)
	}
	if got := hashPassword(t, password(t, katPhrase), salt, small).PHC(); got != katSmall {
		t.Errorf("HashPassword(…, %+v).PHC() = %q, want the reference implementation's %q", small, got, katSmall)
	}
	if got, want := (auth.Hash{}).PHC(), ""; got != want {
		t.Errorf("Hash{}.PHC() = %q, want %q", got, want)
	}
	if got, want := (auth.Hash{}).String(), ""; got != want {
		t.Errorf("Hash{}.String() = %q, want %q", got, want)
	}
}

func TestHashRedacts(t *testing.T) {
	h := parseHash(t, katSmall)
	const shownAs = "$argon2id$v=19$m=64,t=1,p=1$[redacted]"
	type unexported struct{ h auth.Hash }
	type exported struct{ H auth.Hash }
	var shown []string
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%T"} {
		shown = append(shown,
			fmt.Sprintf(verb, h), fmt.Sprintf(verb, &h), fmt.Sprintf(verb, []auth.Hash{h}),
			fmt.Sprintf(verb, unexported{h: h}), fmt.Sprintf(verb, exported{H: h}))
	}
	shown = append(shown, fmt.Errorf("checking %v: %w", h, auth.ErrInvalid).Error())
	var text, js bytes.Buffer
	for _, log := range []*slog.Logger{slog.New(slog.NewTextHandler(&text, nil)), slog.New(slog.NewJSONHandler(&js, nil))} {
		log.Info("sign-in", "hash", h, "unexported", unexported{h: h}, "exported", exported{H: h})
	}
	shown = append(shown, text.String(), js.String())
	for _, v := range []any{h, exported{H: h}} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("json.Marshal(%T): %v", v, err)
		}
		shown = append(shown, string(b))
	}
	salt, err := base64.RawStdEncoding.DecodeString(katSaltB64)
	if err != nil {
		t.Fatal(err)
	}
	key, err := base64.RawStdEncoding.DecodeString(katDigestB64)
	if err != nil {
		t.Fatal(err)
	}
	var leaks []string
	for _, secret := range [][]byte{salt, key} {
		leaks = append(leaks, string(secret), hex.EncodeToString(secret), strings.ToUpper(hex.EncodeToString(secret)),
			strings.Trim(fmt.Sprintf("%d", secret), "[]"), base64.RawStdEncoding.EncodeToString(secret))
	}
	for _, s := range shown {
		for _, leak := range leaks {
			if strings.Contains(s, leak) {
				t.Errorf("the hash's salt or key shows in %q", s)
			}
		}
	}
	for _, s := range []string{text.String(), js.String()} {
		if !strings.Contains(s, shownAs) {
			t.Errorf("slog shows a Hash as %q, want %q", s, shownAs)
		}
	}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%10.3s"} {
		if got := fmt.Sprintf(verb, h); got != shownAs {
			t.Errorf("fmt.Sprintf(%q, h) = %q, want %q", verb, got, shownAs)
		}
	}
	if got := h.String(); got != shownAs {
		t.Errorf("h.String() = %q, want %q", got, shownAs)
	}
}

// TestSecretsDoNotCompare holds Password and Hash to being incomparable:
// == on either would compare secrets in time that depends on where they
// first differ.
func TestSecretsDoNotCompare(t *testing.T) {
	for _, typ := range []reflect.Type{reflect.TypeFor[auth.Password](), reflect.TypeFor[auth.Hash]()} {
		if typ.Comparable() {
			t.Errorf("%v is comparable with ==", typ)
		}
	}
}

func TestVerifyMatchAndRehash(t *testing.T) {
	kat := password(t, katPhrase)
	for _, c := range []struct {
		name string
		p    auth.Password
		h    auth.Hash
		want auth.Verdict
	}{
		{"the password, hashed with DefaultParams", kat, parseHash(t, katDefault), auth.Match{NeedsRehash: false}},
		{"the password, hashed with other params", kat, parseHash(t, katSmall), auth.Match{NeedsRehash: true}},
		{"another password", password(t, "correct horse battery stapler"), parseHash(t, katSmall), auth.Mismatch{}},
		{"the zero Password", auth.Password{}, parseHash(t, katSmall), auth.Mismatch{}},
		{"the zero Hash", kat, auth.Hash{}, auth.Mismatch{}},
		{"the zero Password and the zero Hash", auth.Password{}, auth.Hash{}, auth.Mismatch{}},
	} {
		if got := auth.Verify(c.p, c.h); got != c.want {
			t.Errorf("%s: Verify = %s, want %s", c.name, verdict(got), verdict(c.want))
		}
	}

	// Each pair differs only in what a normalising parser would change, so
	// each Mismatch shows that the password is hashed as it was typed.
	salt := auth.Salt([]byte(katSalt))
	for _, c := range []struct{ hashed, tried string }{
		{katPhrase + " ", katPhrase},
		{" " + katPhrase, katPhrase},
		{strings.ToUpper(katPhrase), katPhrase},
		{"cr\u00e8me br\u00fbl\u00e9e for two", "cre\u0300me bru\u0302le\u0301e for two"}, // NFC and NFD
		{katPhrase + "\t", katPhrase + " "},
	} {
		h := hashPassword(t, password(t, c.hashed), salt, small)
		if got := auth.Verify(password(t, c.hashed), h); got != (auth.Match{NeedsRehash: true}) {
			t.Errorf("Verify(%q, its own hash) = %s, want Match{NeedsRehash: true}", c.hashed, verdict(got))
		}
		if got := auth.Verify(password(t, c.tried), h); got != (auth.Mismatch{}) {
			t.Errorf("Verify(%q, the hash of %q) = %s, want Mismatch", c.tried, c.hashed, verdict(got))
		}
	}

	// A hash whose params differ from DefaultParams in any one field needs
	// rehashing. None costs more than DefaultParams, so that the test stays
	// quick under the race detector.
	for _, params := range []auth.Params{
		{Memory: 64, Time: 3, Threads: 4},
		{Memory: 65536, Time: 1, Threads: 4},
		{Memory: 65536, Time: 3, Threads: 1},
	} {
		h := hashPassword(t, kat, salt, params)
		if got := auth.Verify(kat, h); got != (auth.Match{NeedsRehash: true}) {
			t.Errorf("Verify with params %+v = %s, want Match{NeedsRehash: true}", params, verdict(got))
		}
		if got := auth.Verify(kat, parseHash(t, h.PHC())); got != (auth.Match{NeedsRehash: true}) {
			t.Errorf("Verify with params %+v, after a round trip through ParseHash = %s, want Match{NeedsRehash: true}", params, verdict(got))
		}
	}
}

// verdict names v, listing every variant so that a new one fails the build
// until it is named here too.
func verdict(v auth.Verdict) string {
	switch v := v.(type) {
	case auth.Match:
		if v.NeedsRehash {
			return "Match{NeedsRehash: true}"
		}
		return "Match{NeedsRehash: false}"
	case auth.Mismatch:
		return "Mismatch"
	case nil:
		return "nil"
	default:
		panic("unknown Verdict")
	}
}

func TestHashPasswordRefuses(t *testing.T) {
	kat := password(t, katPhrase)
	salt := auth.Salt([]byte(katSalt))
	for _, c := range []struct {
		name   string
		p      auth.Password
		s      auth.Salt
		params auth.Params
		want   error
	}{
		{"the zero Password", auth.Password{}, salt, small, auth.ErrPasswordTooShort},
		{"the zero Salt", kat, auth.Salt{}, small, auth.ErrInvalid},
		{"the zero Params", kat, salt, auth.Params{}, auth.ErrInvalid},
		{"time 0", kat, salt, auth.Params{Memory: 64, Time: 0, Threads: 1}, auth.ErrInvalid},
		{"time 17", kat, salt, auth.Params{Memory: 64, Time: 17, Threads: 1}, auth.ErrInvalid},
		{"no threads", kat, salt, auth.Params{Memory: 64, Time: 1, Threads: 0}, auth.ErrInvalid},
		{"less memory than 8 KiB a thread", kat, salt, auth.Params{Memory: 31, Time: 1, Threads: 4}, auth.ErrInvalid},
		{"more memory than 1 GiB", kat, salt, auth.Params{Memory: 1<<20 + 1, Time: 1, Threads: 1}, auth.ErrInvalid},
		{"the most memory a uint32 holds", kat, salt, auth.Params{Memory: 1<<32 - 1, Time: 1, Threads: 1}, auth.ErrInvalid},
	} {
		h, err := auth.HashPassword(c.p, c.s, c.params)
		if !errors.Is(err, c.want) {
			t.Errorf("%s: HashPassword = %v, want %v", c.name, err, c.want)
		}
		if got := h.PHC(); got != "" {
			t.Errorf("%s: HashPassword returned %q with its error, want the zero Hash", c.name, got)
		}
	}
	// The smallest params of all, and the most passes: cheap, and both
	// accepted.
	for _, params := range []auth.Params{{Memory: 8, Time: 1, Threads: 1}, {Memory: 8, Time: 16, Threads: 1}} {
		if _, err := auth.HashPassword(kat, salt, params); err != nil {
			t.Errorf("HashPassword(…, %+v) = %v, want it accepted", params, err)
		}
	}
}

// phc builds a hash string from its parameter field and the known answer's
// salt and key.
func phc(params string) string {
	return "$argon2id$v=19$" + params + "$" + katSaltB64 + "$" + katDigestB64
}

func TestParseHashBounds(t *testing.T) {
	for _, s := range []string{
		katDefault,
		katSmall,
		phc("m=1048576,t=16,p=255"), // the most of each
		phc("m=8,t=1,p=1"),          // the least of each
		phc("m=2040,t=1,p=255"),     // 8 KiB for each of 255 threads
		phc("m=32,t=1,p=4"),
		"$argon2id$v=19$m=64,t=1,p=1$AAAAAAAAAAAAAAAAAAAAAQ$" + katDigestB64,        // the salt's last byte alone is set
		"$argon2id$v=19$m=64,t=1,p=1$" + katSaltB64 + "$" + strings.Repeat("A", 43), // a key of zeros is a key
	} {
		h, err := auth.ParseHash(s)
		if err != nil {
			t.Errorf("ParseHash(%q) = %v, want it accepted", s, err)
			continue
		}
		if got := h.PHC(); got != s {
			t.Errorf("ParseHash(%q).PHC() = %q, want it back unchanged", s, got)
		}
	}
}

func TestParseHashRefuses(t *testing.T) {
	for _, s := range []string{
		"",
		"$",
		"argon2id$v=19$m=64,t=1,p=1$" + katSaltB64 + "$" + katDigestB64, // no leading $
		phc("m=64,t=1,p=1") + "$",
		" " + phc("m=64,t=1,p=1"),
		phc("m=64,t=1,p=1") + "\n",
		strings.Replace(katSmall, "argon2id", "argon2i", 1),
		strings.Replace(katSmall, "argon2id", "argon2d", 1),
		strings.Replace(katSmall, "argon2id", "ARGON2ID", 1),
		strings.Replace(katSmall, "v=19", "v=16", 1),
		strings.Replace(katSmall, "v=19", "v=019", 1),
		strings.Replace(katSmall, "$v=19", "", 1),
		phc("t=1,m=64,p=1"),
		phc("m=64,p=1,t=1"),
		phc("m=64,t=1"),
		phc("m=64,t=1,p=1,data=c29tZQ"),
		phc("m=64,t=1,p=1,"),
		phc("m=064,t=1,p=1"),
		phc("m=+64,t=1,p=1"),
		phc("m=-64,t=1,p=1"),
		phc("m= 64,t=1,p=1"),
		phc("m=,t=1,p=1"),
		phc("m=0x40,t=1,p=1"),
		phc("m=6_4,t=1,p=1"),
		phc("m=0,t=1,p=1"),
		phc("m=7,t=1,p=1"),          // under 8 KiB for one thread
		phc("m=31,t=1,p=4"),         // under 8 KiB for each of 4
		phc("m=1048577,t=1,p=1"),    // over 1 GiB
		phc("m=4294967295,t=1,p=1"), // over 1 GiB
		phc("m=4294967296,t=1,p=1"), // over a uint32
		phc("m=99999999999999999999,t=1,p=1"),
		phc("m=64,t=0,p=1"),
		phc("m=64,t=17,p=1"),
		phc("m=64,t=1,p=0"),
		phc("m=4096,t=1,p=256"),
		phc("M=64,T=1,P=1"),
		"$argon2id$v=19$m=64,t=1,p=1$" + katSaltB64[:21] + "$" + katDigestB64,         // a salt 1 short
		"$argon2id$v=19$m=64,t=1,p=1$" + katSaltB64 + "A$" + katDigestB64,             // a salt 1 long
		"$argon2id$v=19$m=64,t=1,p=1$" + katSaltB64 + "==$" + katDigestB64,            // padded
		"$argon2id$v=19$m=64,t=1,p=1$cGlzYXRpLWthdC1zYWx0IR$" + katDigestB64,          // unused bits set
		"$argon2id$v=19$m=64,t=1,p=1$cGlzYXRpLWth\ndC1zYWx0I$" + katDigestB64,         // a line break, which base64 skips
		"$argon2id$v=19$m=64,t=1,p=1$cGlzYXRpLWthdC1zYWx0I-$" + katDigestB64,          // URL-safe alphabet
		"$argon2id$v=19$m=64,t=1,p=1$" + strings.Repeat("A", 22) + "$" + katDigestB64, // the zero Salt
		"$argon2id$v=19$m=64,t=1,p=1$" + katSaltB64 + "$" + katDigestB64[:42],
		"$argon2id$v=19$m=64,t=1,p=1$" + katSaltB64 + "$" + katDigestB64 + "A",
		"$argon2id$v=19$m=64,t=1,p=1$" + katSaltB64 + "$" + katDigestB64 + "=",
		"$argon2id$v=19$m=64,t=1,p=1$" + katSaltB64 + "$JGmZyYl+qr6fuAtAQc9JgcKhIod/JMvdoEk9q6LrRwJ", // unused bits set
		"$argon2id$v=19$m=64,t=1,p=1$" + katSaltB64 + "$" + strings.Repeat("A", 86),                  // a 64-byte key
		"$argon2id$v=19$m=64,t=1,p=1$" + katSaltB64 + "$" + strings.Repeat("A", 42) + "\n",           // 43 characters, which decode to 31 bytes
		"$argon2id$v=19$m=64,t=1,p=1$" + strings.Repeat("A", 19) + "B\n\n$" + katDigestB64,           // 22 characters, which decode to 15 bytes
		phc("m=1048576,t=16,p=255") + "A",                                                            // one past the longest hash, refused unparsed
		"$argon2id$v=19$" + strings.Repeat("$", 1<<10),
		"$argon2id$v=19$m=64,t=1,p=1$" + katSaltB64,
		"$argon2id$v=19$m=64,t=1,p=1$$" + katDigestB64,
	} {
		h, err := auth.ParseHash(s)
		if !errors.Is(err, auth.ErrInvalid) {
			t.Errorf("ParseHash(%q) = %v, want ErrInvalid", s, err)
			continue
		}
		if got := h.PHC(); got != "" {
			t.Errorf("ParseHash(%q) returned %q with its error, want the zero Hash", s, got)
		}
		for part := range strings.SplitSeq(s, "$") {
			if len(part) >= 16 && strings.Contains(err.Error(), part) {
				t.Errorf("ParseHash(%q)'s error %q repeats %q from the hash", s, err, part)
			}
		}
	}
}

// TestParseHashBoundsItsWork checks that ParseHash refuses an oversized
// input before splitting it, where the prefix and then 1 MiB of
// separators would otherwise cost 16 MiB of fields.
func TestParseHashBoundsItsWork(t *testing.T) {
	huge := "$argon2id$v=19$" + strings.Repeat("$", 1<<20)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err := auth.ParseHash(huge)
	runtime.ReadMemStats(&after)
	if !errors.Is(err, auth.ErrInvalid) {
		t.Errorf("ParseHash(the prefix and 1 MiB of $) = %v, want ErrInvalid", err)
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 64<<10 {
		t.Errorf("ParseHash(the prefix and 1 MiB of $) allocated %d bytes, want at most 64 KiB", grew)
	}
}

// FuzzParseHash checks that ParseHash never panics, that every error
// wraps ErrInvalid, that a hash it accepts round-trips exactly (its PHC is
// the input, which parses to a Hash with the same PHC), and that its
// String keeps the params and shows nothing of the salt or key.
func FuzzParseHash(f *testing.F) {
	for _, s := range []string{
		katDefault, katSmall, phc("m=1048576,t=16,p=255"), phc("m=8,t=1,p=1"), phc("m=1048577,t=1,p=1"),
		phc("m=064,t=1,p=1"), "", "$argon2id$v=19$m=64,t=1,p=1$$",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		h, err := auth.ParseHash(s)
		if err != nil {
			if !errors.Is(err, auth.ErrInvalid) {
				t.Fatalf("ParseHash(%q) = %v, want it to wrap ErrInvalid", s, err)
			}
			if got := h.PHC(); got != "" {
				t.Fatalf("ParseHash(%q) returned %q with its error, want the zero Hash", s, got)
			}
			return
		}
		if got := h.PHC(); got != s {
			t.Fatalf("ParseHash(%q).PHC() = %q, want the input", s, got)
		}
		again, err := auth.ParseHash(h.PHC())
		if err != nil || again.PHC() != s {
			t.Fatalf("ParseHash(ParseHash(%q).PHC()) = %q, %v; want the same Hash", s, again.PHC(), err)
		}
		params, secret, _ := strings.Cut(strings.TrimPrefix(s, "$argon2id$v=19$"), "$")
		if got, want := h.String(), "$argon2id$v=19$"+params+"$[redacted]"; got != want || strings.Contains(got, secret) {
			t.Fatalf("ParseHash(%q).String() = %q, want %q", s, got, want)
		}
	})
}

func BenchmarkHashPassword(b *testing.B) {
	p := password(b, katPhrase)
	salt := auth.Salt([]byte(katSalt))
	for b.Loop() {
		if _, err := auth.HashPassword(p, salt, auth.DefaultParams()); err != nil {
			b.Fatal(err)
		}
	}
}
