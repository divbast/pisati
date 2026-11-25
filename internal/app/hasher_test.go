// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package app_test

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/divbast/pisati/internal/app"
	"github.com/divbast/pisati/internal/core/auth"
)

// password returns s as a Password, failing the test if it is not one.
func password(t *testing.T, s string) auth.Password {
	t.Helper()
	p, err := auth.ParsePassword(s)
	if err != nil {
		t.Fatalf("ParsePassword: %v", err)
	}
	return p
}

// newHasher returns a Hasher drawing salts from crypto/rand.
func newHasher(t *testing.T) *app.Hasher {
	t.Helper()
	h, err := app.NewHasher(t.Context(), rand.Reader)
	if err != nil {
		t.Fatalf("NewHasher: %v", err)
	}
	return h
}

// gatedRand is a random source that, once shut, holds each Read until
// the test lets it through, and counts how many Reads it holds at once.
// A Hasher reads each salt while it holds its one slot, so the count is
// how many hashes run at once.
type gatedRand struct {
	shut    atomic.Bool
	release chan struct{}
	inside  atomic.Int32
	most    atomic.Int32
}

func newGatedRand() *gatedRand {
	return &gatedRand{release: make(chan struct{})}
}

func (g *gatedRand) Read(p []byte) (int, error) {
	if g.shut.Load() {
		n := g.inside.Add(1)
		for {
			most := g.most.Load()
			if n <= most || g.most.CompareAndSwap(most, n) {
				break
			}
		}
		<-g.release
		g.inside.Add(-1)
	}
	for i := range p {
		p[i] = byte(i + 1)
	}
	return len(p), nil
}

// laterFailingRand is crypto/rand until failing is set, and then fails.
type laterFailingRand struct {
	failing atomic.Bool
}

func (r *laterFailingRand) Read(p []byte) (int, error) {
	if r.failing.Load() {
		return 0, errors.New("no randomness to be had")
	}
	return rand.Read(p)
}

func TestHasherBoundsConcurrency(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		rnd := newGatedRand()
		h, err := app.NewHasher(ctx, rnd)
		if err != nil {
			t.Fatalf("NewHasher: %v", err)
		}
		p := password(t, "correct horse battery staple")
		stored, err := h.Hash(ctx, p)
		if err != nil {
			t.Fatalf("Hash: %v", err)
		}
		rnd.shut.Store(true)

		// The first hash takes the slot, and holds it while it waits on
		// its salt.
		const hashes = 4
		var hashing sync.WaitGroup
		hash := func() {
			hashing.Go(func() {
				if _, err := h.Hash(ctx, p); err != nil {
					t.Errorf("Hash: %v", err)
				}
			})
		}
		hash()
		synctest.Wait()

		// Everything after it waits: the other hashes, a verification,
		// and two calls whose context ends while they wait, which return
		// its error without hashing.
		for range hashes - 1 {
			hash()
		}
		verified := make(chan auth.Verdict, 1)
		go func() {
			v, err := h.Verify(ctx, p, stored)
			if err != nil {
				t.Errorf("Verify: %v", err)
			}
			verified <- v
		}()
		waiting, stopWaiting := context.WithCancel(ctx)
		gaveUp := make(chan error, 2)
		go func() {
			_, err := h.Hash(waiting, p)
			gaveUp <- err
		}()
		go func() {
			_, err := h.Verify(waiting, p, stored)
			gaveUp <- err
		}()
		synctest.Wait()
		select {
		case v := <-verified:
			t.Errorf("Verify returned %#v while a hash held the slot", v)
		default:
		}
		stopWaiting()
		for range 2 {
			if err := <-gaveUp; !errors.Is(err, context.Canceled) {
				t.Errorf("a wait whose context ended returned %v, want context.Canceled", err)
			}
		}

		for i := range hashes {
			synctest.Wait()
			if n := rnd.inside.Load(); n != 1 {
				t.Errorf("round %d: %d hashes at once, want 1", i, n)
			}
			rnd.release <- struct{}{}
		}
		hashing.Wait()
		if v := <-verified; v != (auth.Match{}) {
			t.Errorf("Verify = %#v, want a Match", v)
		}
		if most := rnd.most.Load(); most != 1 {
			t.Errorf("at most %d hashes ran at once, want 1", most)
		}

		// A context that has already ended never waits.
		ended, end := context.WithCancel(ctx)
		end()
		if _, err := h.Hash(ended, p); !errors.Is(err, context.Canceled) {
			t.Errorf("Hash with an ended context: %v, want context.Canceled", err)
		}
		if _, err := h.Verify(ended, p, stored); !errors.Is(err, context.Canceled) {
			t.Errorf("Verify with an ended context: %v, want context.Canceled", err)
		}
	})
}

func TestHasherVerify(t *testing.T) {
	ctx := t.Context()
	h := newHasher(t)
	p := password(t, "correct horse battery staple")
	stored, err := h.Hash(ctx, p)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if !strings.HasPrefix(stored.String(), "$argon2id$v=19$m=65536,t=3,p=4$") {
		t.Errorf("Hash made %s, want DefaultParams", stored)
	}
	again, err := h.Hash(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if again.PHC() == stored.PHC() {
		t.Error("two hashes of one password are the same: the salt is not new each time")
	}

	for _, tc := range []struct {
		name   string
		p      auth.Password
		stored auth.Hash
		want   auth.Verdict
	}{
		{name: "the password", p: p, stored: stored, want: auth.Match{}},
		{name: "another password", p: password(t, "correct horse battery stapler"), stored: stored, want: auth.Mismatch{}},
		// The zero Hash stands for a user that does not exist, and never
		// matches.
		{name: "no user", p: p, stored: auth.Hash{}, want: auth.Mismatch{}},
		{name: "the zero Password", p: auth.Password{}, stored: stored, want: auth.Mismatch{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, err := h.Verify(ctx, tc.p, tc.stored)
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if v != tc.want {
				t.Errorf("Verify = %#v, want %#v", v, tc.want)
			}
		})
	}

	if _, err := h.Hash(ctx, auth.Password{}); !errors.Is(err, auth.ErrPasswordTooShort) {
		t.Errorf("Hash(zero Password): %v, want ErrPasswordTooShort", err)
	}
}

// TestVerifyTakesAsLongWithoutUser checks a sign-in for a user that does
// not exist, verified against the zero Hash, costs an argon2id call as one
// for a user that does: skipping it takes microseconds rather than tens
// of milliseconds, far outside the margin of four allowed for noise. Each
// side is timed at its quickest of three.
func TestVerifyTakesAsLongWithoutUser(t *testing.T) {
	ctx := t.Context()
	h := newHasher(t)
	p := password(t, "correct horse battery staple")
	stored, err := h.Hash(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	quickest := func(against auth.Hash) time.Duration {
		best := time.Duration(math.MaxInt64)
		for range 3 {
			start := time.Now()
			if _, err := h.Verify(ctx, p, against); err != nil {
				t.Fatal(err)
			}
			best = min(best, time.Since(start))
		}
		return best
	}
	known, absent := quickest(stored), quickest(auth.Hash{})
	if absent < known/4 {
		t.Errorf("Verify took %s for no user and %s for a real hash: the time shows whether the user exists", absent, known)
	}
}

func TestNewHasherRefuses(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ended bool
		rnd   io.Reader
	}{
		{name: "nil random source"},
		{name: "failing random source", rnd: failingRand{}},
		{name: "short random source", rnd: &shortRand{n: 40}},
		{name: "ended context", ended: true, rnd: rand.Reader},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.ended {
				cancel()
			}
			h, err := app.NewHasher(ctx, tc.rnd)
			if err == nil || h != nil {
				t.Errorf("NewHasher = %v, %v; want nil and an error", h, err)
			}
		})
	}

	t.Run("a random source that fails later", func(t *testing.T) {
		rnd := &laterFailingRand{}
		h, err := app.NewHasher(t.Context(), rnd)
		if err != nil {
			t.Fatalf("NewHasher: %v", err)
		}
		rnd.failing.Store(true)
		if got, err := h.Hash(t.Context(), password(t, "correct horse battery staple")); err == nil {
			t.Errorf("Hash with no salt to draw = %s, nil; want an error", got)
		}
	})

	t.Run("the zero Hasher", func(t *testing.T) {
		p := password(t, "correct horse battery staple")
		for name, h := range map[string]*app.Hasher{"zero": {}, "nil": nil} {
			if _, err := h.Hash(t.Context(), p); err == nil {
				t.Errorf("Hash on the %s Hasher: no error", name)
			}
			if _, err := h.Verify(t.Context(), p, auth.Hash{}); err == nil {
				t.Errorf("Verify on the %s Hasher: no error", name)
			}
		}
	})
}

// TestHashRoundTripsThroughDatabase stores a hash as its PHC form, the
// one form that keeps its salt and key, and reads it back.
func TestHashRoundTripsThroughDatabase(t *testing.T) {
	ctx := t.Context()
	s, _ := newStore(t)
	h := newHasher(t)
	p := password(t, "correct horse battery staple")
	hash, err := h.Hash(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(ctx, `CREATE TABLE test_hashes (phc TEXT NOT NULL) STRICT`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(ctx, `INSERT INTO test_hashes (phc) VALUES (?)`, hash.PHC()); err != nil {
		t.Fatalf("store the PHC form: %v", err)
	}
	// The Hash itself is no value the driver takes, so it cannot be
	// written in its redacted form by mistake.
	if _, err := s.DB().ExecContext(ctx, `INSERT INTO test_hashes (phc) VALUES (?)`, hash); err == nil {
		t.Error("the driver stored a Hash given as it is")
	}

	var stored string
	if err := s.DB().QueryRowContext(ctx, `SELECT phc FROM test_hashes`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != hash.PHC() || strings.Contains(stored, "redacted") {
		t.Errorf("stored %q, want the PHC form", stored)
	}
	back, err := auth.ParseHash(stored)
	if err != nil {
		t.Fatalf("ParseHash(stored): %v", err)
	}
	if back.PHC() != hash.PHC() {
		t.Error("the hash read back has another PHC form")
	}
	if v, err := h.Verify(ctx, p, back); err != nil || v != (auth.Match{}) {
		t.Errorf("Verify(read back) = %#v, %v; want a Match", v, err)
	}
}
