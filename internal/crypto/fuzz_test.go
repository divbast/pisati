// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package crypto_test

import (
	"bytes"
	"errors"
	"testing"

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
