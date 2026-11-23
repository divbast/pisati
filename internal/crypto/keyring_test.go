// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package crypto_test

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/chacha20poly1305"

	"github.com/divbast/pisati/internal/config"
	"github.com/divbast/pisati/internal/crypto"
	"github.com/divbast/pisati/internal/store"
)

func TestSealOpenRoundTrip(t *testing.T) {
	s := newStore(t)
	m := newMaster(t)
	k := openKeyring(t, s, m)
	const plain, assoc = "a value worth keeping", "where it lives"

	first, second := seal(t, k, plain, assoc), seal(t, k, plain, assoc)
	if bytes.Equal(first, second) {
		t.Error("two seals of one value are the same: each needs a nonce of its own")
	}
	for _, sealed := range [][]byte{first, second} {
		wantOpens(t, k, sealed, assoc, plain)
		if len(sealed) != overhead+len(plain) {
			t.Errorf("sealed value of %d bytes, want %d: a header, a nonce, the ciphertext and a tag", len(sealed), overhead+len(plain))
		}
		if sealed[0] != 0x01 || keyIDOf(sealed) != 1 {
			t.Errorf("header % x, want version 1 and the first data key, 1", sealed[:headerSize])
		}
	}

	t.Run("empty", func(t *testing.T) {
		wantOpens(t, k, seal(t, k, "", ""), "", "")
		if got, err := k.Open(seal(t, k, "", ""), nil); err != nil || got == nil {
			t.Errorf("Open of an empty value: %v, %v; want an empty value, not nil, which would bind as NULL", got, err)
		}
		sealed, err := k.Seal(nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		wantOpens(t, k, sealed, "", "")
	})

	t.Run("the caller's buffers are its own", func(t *testing.T) {
		p, a := []byte(plain), []byte(assoc)
		sealed, err := k.Seal(p, a)
		if err != nil {
			t.Fatal(err)
		}
		if string(p) != plain || string(a) != assoc {
			t.Fatal("Seal changed its arguments")
		}
		opened, err := k.Open(sealed, a)
		if err != nil {
			t.Fatal(err)
		}
		opened[0] ^= 0xff
		wantOpens(t, k, sealed, assoc, plain)
	})

	t.Run("each byte flipped", func(t *testing.T) {
		for i := range first {
			for _, bit := range []byte{0x01, 0x80} {
				bad := bytes.Clone(first)
				bad[i] ^= bit
				var want error
				switch {
				case i == 0:
					want = crypto.ErrUnknownVersion
				case i < headerSize:
					want = crypto.ErrUnknownKey
				default:
					want = crypto.ErrNotAuthentic
				}
				if _, err := k.Open(bad, []byte(assoc)); !errors.Is(err, want) {
					t.Errorf("byte %d flipped by %#x: %v, want %v", i, bit, err, want)
				}
			}
		}
	})

	t.Run("refused", func(t *testing.T) {
		for _, c := range []struct {
			name   string
			sealed []byte
			assoc  string
			want   error
		}{
			{"the wrong associated data", first, "where it does not live", crypto.ErrNotAuthentic},
			{"no associated data", first, "", crypto.ErrNotAuthentic},
			{"version 0", append([]byte{0x00}, first[1:]...), assoc, crypto.ErrUnknownVersion},
			{"version 2", append([]byte{0x02}, first[1:]...), assoc, crypto.ErrUnknownVersion},
			{"an unknown key ID", withKeyID(first, 7), assoc, crypto.ErrUnknownKey},
			{"key ID 0", withKeyID(first, 0), assoc, crypto.ErrUnknownKey},
			{"a byte more", append(bytes.Clone(first), 0), assoc, crypto.ErrNotAuthentic},
			{"a byte less", first[:len(first)-1], assoc, crypto.ErrNotAuthentic},
			{"nothing", nil, assoc, crypto.ErrTooShort},
		} {
			if _, err := k.Open(c.sealed, []byte(c.assoc)); !errors.Is(err, c.want) {
				t.Errorf("%s: %v, want %v", c.name, err, c.want)
			}
		}
		empty := seal(t, k, "", assoc)
		for n := range len(empty) {
			if _, err := k.Open(empty[:n], []byte(assoc)); !errors.Is(err, crypto.ErrTooShort) {
				t.Errorf("the first %d bytes of a sealed value: %v, want ErrTooShort", n, err)
			}
		}
	})

	t.Run("the header is authenticated", func(t *testing.T) {
		// A random source of one byte draws the same data key twice, so
		// keys 1 and 2 are one key under two IDs: only the header's
		// authentication tells a value moved from one to the other.
		s := newStore(t)
		k, err := crypto.OpenKeyring(t.Context(), s, newMaster(t), clock, constReader(0x42))
		if err != nil {
			t.Fatal(err)
		}
		if err := crypto.Rotate(t.Context(), s, k); err != nil {
			t.Fatal(err)
		}
		sealed := seal(t, k, plain, assoc)
		if keyIDOf(sealed) != 2 {
			t.Fatalf("sealed under key %d, want 2", keyIDOf(sealed))
		}
		if _, err := k.Open(withKeyID(sealed, 1), []byte(assoc)); !errors.Is(err, crypto.ErrNotAuthentic) {
			t.Errorf("a value moved to key 1: %v, want ErrNotAuthentic", err)
		}

		// The format, checked against the cipher itself: the data key is
		// 32 bytes of 0x42, and so is every nonce.
		aead, err := chacha20poly1305.NewX(bytes.Repeat([]byte{0x42}, 32))
		if err != nil {
			t.Fatal(err)
		}
		if nonce := sealed[headerSize : headerSize+nonceSize]; !bytes.Equal(nonce, bytes.Repeat([]byte{0x42}, nonceSize)) {
			t.Fatalf("nonce % x, want the random source's", nonce)
		}
		body := sealed[headerSize+nonceSize:]
		got, err := aead.Open(nil, sealed[headerSize:headerSize+nonceSize], body, append(bytes.Clone(sealed[:headerSize]), assoc...))
		if err != nil || string(got) != plain {
			t.Errorf("XChaCha20-Poly1305 under the header and then the associated data: %q, %v; want %q", got, err, plain)
		}
		if _, err := aead.Open(nil, sealed[headerSize:headerSize+nonceSize], body, []byte(assoc)); err == nil {
			t.Error("the value opens under the associated data alone: the header is not authenticated")
		}
	})

	t.Run("a random source that fails", func(t *testing.T) {
		// The keyring exists, so opening it draws nothing.
		k, err := crypto.OpenKeyring(t.Context(), s, m, clock, failingReader{})
		if err != nil {
			t.Fatal(err)
		}
		if sealed, err := k.Seal([]byte(plain), nil); err == nil || sealed != nil {
			t.Errorf("Seal with no randomness: % x, %v; want an error", sealed, err)
		}
	})
}

func TestZeroKeyring(t *testing.T) {
	for _, k := range []*crypto.Keyring{nil, new(crypto.Keyring)} {
		if sealed, err := k.Seal([]byte("value"), nil); !errors.Is(err, crypto.ErrInvalid) || sealed != nil {
			t.Errorf("Seal on %#v: % x, %v; want ErrInvalid", k, sealed, err)
		}
		if plain, err := k.Open(make([]byte, 64), nil); !errors.Is(err, crypto.ErrInvalid) || plain != nil {
			t.Errorf("Open on %#v: % x, %v; want ErrInvalid", k, plain, err)
		}
	}
}

func TestOpenKeyringRefusesWrongMaster(t *testing.T) {
	t.Run("a new database gets its first key", func(t *testing.T) {
		s := newStore(t)
		m := newMaster(t)
		k := openKeyring(t, s, m)
		keys := dataKeys(t, s)
		if len(keys) != 1 || keys[0].id != 1 || keys[0].state != "current" || keys[0].createdAt != clock().UnixMilli() {
			t.Fatalf("data keys %+v, want one, current, created at the clock's time", keys)
		}
		sealed := seal(t, k, "value", "assoc")
		// Opening it again unwraps that key, and makes none.
		wantOpens(t, openKeyring(t, s, m), sealed, "assoc", "value")
		if again := dataKeys(t, s); !sameKeys(keys, again) {
			t.Errorf("opening the keyring again changed the data keys: %+v, was %+v", again, keys)
		}
	})

	t.Run("another master", func(t *testing.T) {
		s := newStore(t)
		m := newMaster(t)
		k := openKeyring(t, s, m)
		if err := crypto.Rotate(t.Context(), s, k); err != nil {
			t.Fatal(err)
		}
		sealed := seal(t, k, "value", "assoc")
		before := dataKeys(t, s)

		other, err := crypto.OpenKeyring(t.Context(), s, newMaster(t), clock, rand.Reader)
		if !errors.Is(err, crypto.ErrMasterMismatch) || other != nil {
			t.Fatalf("OpenKeyring with another master: %v, %v; want ErrMasterMismatch", other, err)
		}
		if after := dataKeys(t, s); !sameKeys(before, after) {
			t.Errorf("OpenKeyring with another master changed the data keys: %+v, was %+v", after, before)
		}
		wantOpens(t, openKeyring(t, s, m), sealed, "assoc", "value")
	})

	t.Run("a restored database without its key", func(t *testing.T) {
		// What serve meets on a database restored without its master key:
		// none is given, so Pisati generates one, which wraps nothing.
		s := newStore(t)
		seal(t, openKeyring(t, s, newMaster(t)), "value", "assoc")
		before := dataKeys(t, s)
		state, err := os.OpenRoot(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := state.Close(); err != nil {
				t.Error(err)
			}
		}()
		generated, err := crypto.LoadMaster(t.Context(), config.Generated{}, nil, state, discard())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := crypto.OpenKeyring(t.Context(), s, generated, clock, rand.Reader); !errors.Is(err, crypto.ErrMasterMismatch) {
			t.Fatalf("OpenKeyring with a generated master: %v, want ErrMasterMismatch", err)
		}
		if after := dataKeys(t, s); !sameKeys(before, after) {
			t.Errorf("OpenKeyring with a generated master changed the data keys: %+v, was %+v", after, before)
		}
	})

	t.Run("a wrapped key moved to another row", func(t *testing.T) {
		s := newStore(t)
		m := newMaster(t)
		if err := crypto.Rotate(t.Context(), s, openKeyring(t, s, m)); err != nil {
			t.Fatal(err)
		}
		// Both keys are wrapped by m, each bound to its own ID: swapped,
		// m unwraps neither.
		keys := dataKeys(t, s)
		setWrapped(t, s, 1, keys[1].wrapped)
		setWrapped(t, s, 2, keys[0].wrapped)
		swapped := dataKeys(t, s)
		if _, err := crypto.OpenKeyring(t.Context(), s, m, clock, rand.Reader); !errors.Is(err, crypto.ErrMasterMismatch) {
			t.Fatalf("OpenKeyring with the wrapped keys swapped: %v, want ErrMasterMismatch", err)
		}
		if after := dataKeys(t, s); !sameKeys(swapped, after) {
			t.Error("OpenKeyring changed the data keys")
		}
	})

	t.Run("a wrapped key changed", func(t *testing.T) {
		s := newStore(t)
		m := newMaster(t)
		openKeyring(t, s, m)
		damageKey(t, s, 1)
		if _, err := crypto.OpenKeyring(t.Context(), s, m, clock, rand.Reader); !errors.Is(err, crypto.ErrMasterMismatch) {
			t.Fatalf("OpenKeyring with a wrapped key changed: %v, want ErrMasterMismatch", err)
		}
	})

	t.Run("no current key", func(t *testing.T) {
		s := newStore(t)
		m := newMaster(t)
		openKeyring(t, s, m)
		exec(t, s, `UPDATE data_keys SET state = 'retired'`)
		if _, err := crypto.OpenKeyring(t.Context(), s, m, clock, rand.Reader); err == nil {
			t.Fatal("OpenKeyring with no current key: no error")
		}
		if keys := dataKeys(t, s); len(keys) != 1 {
			t.Errorf("OpenKeyring with no current key made one: %d keys", len(keys))
		}
	})

	t.Run("what it is given", func(t *testing.T) {
		s := newStore(t)
		m := newMaster(t)
		for _, c := range []struct {
			name  string
			s     *store.Store
			m     crypto.Master
			clock func() time.Time
		}{
			{name: "no store", m: m, clock: clock},
			{name: "the zero store", s: new(store.Store), m: m, clock: clock},
			{name: "the zero master", s: s, clock: clock},
			{name: "no clock", s: s, m: m},
		} {
			if _, err := crypto.OpenKeyring(t.Context(), c.s, c.m, c.clock, rand.Reader); !errors.Is(err, crypto.ErrInvalid) {
				t.Errorf("%s: %v, want ErrInvalid", c.name, err)
			}
		}
		if _, err := crypto.OpenKeyring(t.Context(), s, m, clock, nil); !errors.Is(err, crypto.ErrInvalid) {
			t.Errorf("no random source: %v, want ErrInvalid", err)
		}
		if keys := dataKeys(t, s); len(keys) != 0 {
			t.Errorf("a refused OpenKeyring made %d keys", len(keys))
		}
	})
}

// setWrapped stores wrapped as the data key id's wrapped form.
func setWrapped(t *testing.T, s *store.Store, id int64, wrapped []byte) {
	t.Helper()
	if _, err := s.DB().ExecContext(t.Context(), `UPDATE data_keys SET wrapped = ? WHERE id = ?`, wrapped, id); err != nil {
		t.Fatal(err)
	}
}

// damageKey flips a bit of the data key id's wrapped form, in its
// ciphertext.
func damageKey(t *testing.T, s *store.Store, id int64) {
	t.Helper()
	for _, k := range dataKeys(t, s) {
		if k.id == id {
			k.wrapped[30] ^= 0x01
			setWrapped(t, s, id, k.wrapped)
			return
		}
	}
	t.Fatalf("no data key %d", id)
}

func TestKeyringConcurrent(t *testing.T) {
	s := newStore(t)
	k := openKeyring(t, s, newMaster(t))
	const goroutines, rounds, rotations = 8, 50, 3

	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	for g := range goroutines {
		wg.Go(func() {
			for n := range rounds {
				plain := fmt.Appendf(nil, "value %d.%d", g, n)
				sealed, err := k.Seal(plain, []byte("assoc"))
				if err != nil {
					errs <- err
					return
				}
				got, err := k.Open(sealed, []byte("assoc"))
				if err != nil || !bytes.Equal(got, plain) {
					errs <- fmt.Errorf("Open: %q, %w", got, err)
					return
				}
			}
		})
	}
	for range rotations {
		if err := crypto.Rotate(t.Context(), s, k); err != nil {
			t.Error(err)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if keyID := keyIDOf(seal(t, k, "last", "")); keyID != 1+rotations {
		t.Errorf("sealed under key %d after %d rotations, want %d", keyID, rotations, 1+rotations)
	}
}
