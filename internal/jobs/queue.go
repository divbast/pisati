// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

// Package jobs runs Pisati's effects as jobs kept in its database. A use
// case writes each effect as a job with Enqueue, in the same transaction
// as the change that causes it, so that the effect is queued if and only
// if the change commits: a transactional outbox. A Worker claims each due
// job under a lease, runs it with the Handler for its Kind, and then marks
// it done, makes it ready again after a backoff, or moves it to the dead
// letter, where it is never tried again.
//
// A job runs at least once, not exactly once: a worker that stops or hangs
// mid-job keeps its lease only until the lease expires, and another worker
// then runs the job again. Every Handler must therefore be idempotent.
package jobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/divbast/pisati/internal/store/storedb"
)

// ErrInvalid is a value its rule refuses: a kind, an idempotency key, a
// job, or something given to NewWorker. Every error the package returns
// for one wraps it and says which rule.
var ErrInvalid = errors.New("invalid")

const (
	// maxKindBytes bounds a Kind's name.
	maxKindBytes = 64
	// maxKeyBytes bounds an IdempotencyKey.
	maxKeyBytes = 200
)

// Kind names what a job does, and so the Handler that runs it. The zero
// Kind names nothing: Enqueue refuses it and a Worker takes no Handler for
// it. Kinds are comparable, so that they can key a map.
type Kind struct {
	name string
}

// NewKind returns s as a Kind, or an error wrapping ErrInvalid unless s is
// at most 64 bytes of two or more words joined by dots, as in
// crypto.reencrypt: each word is a lower-case ASCII letter followed by
// lower-case letters and digits, and every word but the first may also
// hold underscores.
func NewKind(s string) (Kind, error) {
	if !validKind(s) {
		return Kind{}, fmt.Errorf("jobs: %w job kind %q: want at most %d bytes of dotted lower-case words, such as crypto.reencrypt",
			ErrInvalid, s, maxKindBytes)
	}
	return Kind{name: s}, nil
}

// validKind reports whether s follows NewKind's rule.
func validKind(s string) bool {
	if len(s) > maxKindBytes {
		return false
	}
	words := strings.Split(s, ".")
	if len(words) < 2 {
		return false
	}
	for i, word := range words {
		if word == "" || !isLower(word[0]) {
			return false
		}
		for _, c := range []byte(word[1:]) {
			if !isLower(c) && !isDigit(c) && (i == 0 || c != '_') {
				return false
			}
		}
	}
	return true
}

func isLower(c byte) bool { return 'a' <= c && c <= 'z' }
func isDigit(c byte) bool { return '0' <= c && c <= '9' }

// String returns the kind's name, or "" for the zero Kind.
func (k Kind) String() string {
	return k.name
}

// IdempotencyKey names an effect, so that it is queued once however often
// it is enqueued: Enqueue adds no job whose key a job already queued, in
// any state, has. The zero IdempotencyKey is no key, and a job without one
// never conflicts with another. Keys are comparable.
type IdempotencyKey struct {
	key string
}

// NewIdempotencyKey returns s as an IdempotencyKey, or an error wrapping
// ErrInvalid unless s is 1 to 200 bytes of printable ASCII, space to
// tilde. A key may be built from personal data, so the error never
// repeats s.
func NewIdempotencyKey(s string) (IdempotencyKey, error) {
	if len(s) == 0 || len(s) > maxKeyBytes {
		return IdempotencyKey{}, fmt.Errorf("jobs: %w idempotency key: %d bytes, want 1 to %d", ErrInvalid, len(s), maxKeyBytes)
	}
	for i := range len(s) {
		if s[i] < ' ' || s[i] > '~' {
			return IdempotencyKey{}, fmt.Errorf("jobs: %w idempotency key: byte %d is not printable ASCII", ErrInvalid, i)
		}
	}
	return IdempotencyKey{key: s}, nil
}

// Job is a job to enqueue.
type Job struct {
	// Kind picks the Handler that runs the job. It is never the zero Kind.
	Kind Kind
	// Payload is what the Handler is given. nil is stored as empty.
	Payload []byte
	// Key, unless it is the zero key, has Enqueue add the job only if no
	// job queued has the same key.
	Key IdempotencyKey
	// RunAt is the earliest time the job may run, to the millisecond. The
	// zero time means as soon as a worker is free.
	RunAt time.Time
}

// Enqueue adds j to the queue through q, ready to run from j.RunAt, and
// reports whether it did: false, with no error, when j has a key that a
// job already queued has, in which case nothing changes. It is meant to
// run in the caller's transaction (Store.InTx), so that the job commits or
// rolls back with the change that caused it.
//
// Enqueue reads no clock, so the caller gives RunAt; and it wakes no
// Worker, so the caller calls Worker.Notify once the transaction commits,
// or the job waits for a worker's next poll. A job with the zero Kind, or
// a nil q, is refused with an error wrapping ErrInvalid.
func Enqueue(ctx context.Context, q *storedb.Queries, j Job) (bool, error) {
	if j.Kind == (Kind{}) {
		return false, fmt.Errorf("jobs: enqueue: %w job: it has no kind", ErrInvalid)
	}
	if q == nil {
		return false, fmt.Errorf("jobs: enqueue a %s job: %w queries: nil", j.Kind, ErrInvalid)
	}
	payload := j.Payload
	if payload == nil {
		// The column is NOT NULL, and a nil slice binds as NULL.
		payload = []byte{}
	}
	var runAt int64
	if !j.RunAt.IsZero() {
		runAt = j.RunAt.UnixMilli()
	}
	added, err := q.EnqueueJob(ctx, storedb.EnqueueJobParams{
		Kind:           j.Kind.name,
		Payload:        payload,
		IdempotencyKey: sql.NullString{String: j.Key.key, Valid: j.Key != (IdempotencyKey{})},
		RunAt:          runAt,
	})
	if err != nil {
		return false, fmt.Errorf("jobs: enqueue a %s job: %w", j.Kind, err)
	}
	return added == 1, nil
}
