// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package crypto_test

import (
	"bytes"
	"encoding/base64"
	"errors"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/divbast/pisati/internal/config"
	"github.com/divbast/pisati/internal/crypto"
)

// FuzzOpen holds Open to its contract on any input: it never panics, and
// it returns either an error that says why it refused, or the plaintext of
// a value Seal made, under the associated data Seal was given. The input
// also goes the other way, as a plaintext and its associated data, which
// must come back from a round trip through Seal and Open.
func FuzzOpen(f *testing.F) {
	s := newStore(f)
	k := openKeyring(f, s, newMaster(f))
	if err := crypto.Rotate(f.Context(), s, k); err != nil {
		f.Fatal(err)
	}

	type sealedValue struct{ plain, assoc, sealed []byte }
	var known []sealedValue
	for _, c := range []struct{ plain, assoc string }{
		{"", ""},
		{"a value", ""},
		{"a value", "secrets.value#1"},
		{"another value, rather longer than the first one was", "secrets.note#2"},
	} {
		sealed, err := k.Seal([]byte(c.plain), []byte(c.assoc))
		if err != nil {
			f.Fatal(err)
		}
		known = append(known, sealedValue{[]byte(c.plain), []byte(c.assoc), sealed})
		f.Add(sealed, []byte(c.assoc))
		f.Add(sealed, []byte(c.assoc+"x"))
		f.Add(sealed[:len(sealed)-1], []byte(c.assoc))
		f.Add(withKeyID(sealed, 1), []byte(c.assoc))
	}
	f.Add([]byte{}, []byte{})
	f.Add([]byte{0x01, 0, 0, 0, 2}, []byte(nil))
	f.Add(bytes.Repeat([]byte{0x01}, overhead), []byte("assoc"))

	f.Fuzz(func(t *testing.T, data, assoc []byte) {
		plain, err := k.Open(data, assoc)
		switch {
		case err == nil:
			authentic := false
			for _, v := range known {
				if bytes.Equal(v.sealed, data) && bytes.Equal(v.assoc, assoc) && bytes.Equal(v.plain, plain) {
					authentic = true
				}
			}
			if !authentic {
				t.Fatalf("Open accepted % x under % x, which Seal never made", data, assoc)
			}
		case !errors.Is(err, crypto.ErrTooShort) && !errors.Is(err, crypto.ErrUnknownVersion) &&
			!errors.Is(err, crypto.ErrUnknownKey) && !errors.Is(err, crypto.ErrNotAuthentic):
			t.Fatalf("Open: %v, an error that is none of the package's", err)
		case plain != nil:
			t.Fatalf("Open returned % x with its error", plain)
		}

		sealed, err := k.Seal(data, assoc)
		if err != nil {
			t.Fatalf("Seal: %v", err)
		}
		got, err := k.Open(sealed, assoc)
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("a round trip of % x gave % x, %v", data, got, err)
		}
	})
}

// FuzzParseKey holds the reading of a key file to its rules, whatever the
// file holds: LoadMaster accepts exactly the canonical spelling of a
// 32-byte key in standard base64, the one that decoding and encoding again
// gives back, optionally followed by one newline. When it refuses, it never
// panics and its error tells nothing of what the file holds: it is the
// error a file of as many zero bytes gets.
func FuzzParseKey(f *testing.F) {
	text := keyText(randomKey(f))
	unused := bytes.Clone(text)
	unused[42] = base64Alphabet[strings.IndexByte(base64Alphabet, unused[42])|1]
	for _, seed := range [][]byte{
		text,
		text[:44],
		append(bytes.Clone(text), '\n'),
		append(bytes.Clone(text[:44]), '\r', '\n'),
		text[:43],
		unused,
		[]byte(base64.StdEncoding.EncodeToString(append(randomKey(f), 7))),
		slices.Concat(text[:20], []byte("\n"), text[20:43]),
		{},
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		_, err := loadKeyFile(t, data)
		canonical := isCanonical(data)
		switch {
		case err == nil && !canonical:
			t.Fatalf("LoadMaster accepted %q, which is no key's canonical spelling", data)
		case err != nil && canonical:
			t.Fatalf("LoadMaster refused the canonical spelling %q: %v", data, err)
		case err != nil:
			if !errors.Is(err, crypto.ErrKeyFile) {
				t.Fatalf("LoadMaster(%q): %v, want ErrKeyFile", data, err)
			}
			_, zeros := loadKeyFile(t, make([]byte, len(data)))
			if zeros == nil || err.Error() != zeros.Error() {
				t.Fatalf("LoadMaster(%q): %q; a file of %d zero bytes gets %v: the error depends on what the file holds",
					data, err, len(data), zeros)
			}
		}
	})
}

// loadKeyFile loads the master key from a credential, kept in memory,
// that holds data at mode 0400.
func loadKeyFile(t *testing.T, data []byte) (crypto.Master, error) {
	t.Helper()
	const name = "pisati-master-key"
	creds := fstest.MapFS{name: &fstest.MapFile{Data: data, Mode: 0o400}}
	return crypto.LoadMaster(t.Context(), config.FromCredential{Name: name}, creds, nil, discard())
}

// isCanonical reports whether data is a 32-byte key in standard base64,
// spelt as encoding it gives, then optionally one newline.
func isCanonical(data []byte) bool {
	text, _ := bytes.CutSuffix(data, []byte("\n"))
	key, err := base64.StdEncoding.DecodeString(string(text))
	return err == nil && len(key) == 32 && base64.StdEncoding.EncodeToString(key) == string(text)
}
