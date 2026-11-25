// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package app

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"

	"github.com/divbast/pisati/internal/core/auth"
)

// bug ends the message of what only a mistake in Pisati's code does.
const bug = "which is a bug in Pisati"

// hashSlots is how many argon2id calls run at once: one. Each takes
// 64 MiB and four threads for tens of milliseconds, so a burst of
// sign-ins run all at once could exhaust the server's memory; one at a
// time, a burst waits instead.
const hashSlots = 1

// Hasher hashes passwords, and verifies them against their hashes, with
// argon2id at auth.DefaultParams, one call at a time. A call waits for its
// turn until its context ends. Its methods are safe for concurrent use.
// The zero Hasher has no slot to take: its methods return an error.
type Hasher struct {
	slots chan struct{}
	rnd   io.Reader
	// decoy is a real hash of a password no one knows, which a sign-in for
	// a user that does not exist is verified against: see Verify.
	decoy auth.Hash
}

// NewHasher returns a Hasher drawing its salts from rnd, which must be
// safe for concurrent use, as crypto/rand's Reader is. It makes the decoy
// Verify uses at once, a hash of a password drawn from rnd and then
// dropped, so it takes as long as one hash, and 64 MiB while it does. It
// returns an error for a nil rnd, when rnd fails, or when ctx has ended.
func NewHasher(ctx context.Context, rnd io.Reader) (*Hasher, error) {
	if rnd == nil {
		return nil, errors.New("app.NewHasher was given no random source, " + bug)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("making the password hasher was stopped before it began: %w", err)
	}
	var secret [32]byte
	if _, err := io.ReadFull(rnd, secret[:]); err != nil {
		return nil, fmt.Errorf("the %d random bytes of the password hasher's decoy cannot be read: %w", len(secret), err)
	}
	p, err := auth.ParsePassword(base64.RawURLEncoding.EncodeToString(secret[:]))
	if err != nil {
		return nil, fmt.Errorf("the password hasher's decoy password cannot be made, %s: %w", bug, err)
	}
	decoy, err := hashWith(rnd, p)
	if err != nil {
		return nil, fmt.Errorf("the password hasher's decoy cannot be made: %w", err)
	}
	return &Hasher{slots: make(chan struct{}, hashSlots), rnd: rnd, decoy: decoy}, nil
}

// Hash returns p's hash, made with auth.DefaultParams and a salt of 16
// bytes from the random source, drawn once it has its turn. It returns
// auth.ErrPasswordTooShort for the zero Password, and its context's error
// when that ends first. Store the hash as its PHC form, which alone
// holds its salt and key: every other form of it is redacted.
func (h *Hasher) Hash(ctx context.Context, p auth.Password) (auth.Hash, error) {
	if err := h.acquire(ctx); err != nil {
		return auth.Hash{}, err
	}
	defer h.release()
	return hashWith(h.rnd, p)
}

// Verify returns auth.Verify's verdict on p and stored, once it has its
// turn, or its context's error when that ends first.
//
// It takes the zero Hash to stand for a user that does not exist: it then
// verifies p against the decoy NewHasher made, a real hash with
// auth.DefaultParams, and returns auth.Mismatch. So a sign-in for a user
// that does not exist takes as long as one for a user that does, and
// passing the zero Hash for a user not found is all a caller needs to do
// for that.
func (h *Hasher) Verify(ctx context.Context, p auth.Password, stored auth.Hash) (auth.Verdict, error) {
	if err := h.acquire(ctx); err != nil {
		return nil, err
	}
	defer h.release()
	// String is empty for the zero Hash alone, and shows no secret.
	if stored.String() == "" {
		// Only the time the decoy's verification takes is wanted.
		auth.Verify(p, h.decoy)
		return auth.Mismatch{}, nil
	}
	return auth.Verify(p, stored), nil
}

// hashWith returns p's hash with auth.DefaultParams and a salt drawn from
// rnd.
func hashWith(rnd io.Reader, p auth.Password) (auth.Hash, error) {
	var salt auth.Salt
	if _, err := io.ReadFull(rnd, salt[:]); err != nil {
		return auth.Hash{}, fmt.Errorf("the %d random bytes of a password's salt cannot be read: %w", len(salt), err)
	}
	hash, err := auth.HashPassword(p, salt, auth.DefaultParams())
	if err != nil {
		return auth.Hash{}, fmt.Errorf("the password cannot be hashed: %w", err)
	}
	return hash, nil
}

// acquire waits for a slot, until ctx ends. It returns an error for a
// Hasher NewHasher did not make, and ctx's when it has ended.
func (h *Hasher) acquire(ctx context.Context) error {
	if h == nil || h.slots == nil {
		return errors.New("app.Hasher was used without app.NewHasher, " + bug)
	}
	// A context that has already ended never takes a free slot, which
	// select would pick at random.
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("the wait to hash a password was cut short: %w", err)
	}
	select {
	case h.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("the wait to hash a password was cut short: %w", ctx.Err())
	}
}

// release gives back the slot acquire took.
func (h *Hasher) release() {
	<-h.slots
}
