// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

// Package jobs runs Pisati's effects as jobs kept in its database. A use
// case writes each effect as a job with Enqueue, in the same transaction
// as the change that causes it, so that the effect is queued if and only
// if the change commits: a transactional outbox. A Worker claims each due
// job under a lease, runs it with the Handler for its Kind, and then marks
// it done, makes it ready again after a backoff, or moves it to the dead
// letter, where it is never tried again. The Worker deletes done and dead
// jobs once they are past their retention, and their idempotency keys
// with them.
//
// A job runs at least once, not exactly once: a worker that stops or hangs
// mid-job keeps its lease only until the lease expires, and another worker
// then runs the job again. Every Handler must therefore be idempotent.
package jobs

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/divbast/pisati/internal/explain"
	"github.com/divbast/pisati/internal/store/storedb"
)

// ErrInvalid is a value its rule refuses: a kind, an idempotency key, a
// job, or something given to NewWorker. Every error the package returns
// for one wraps it and says which rule.
var ErrInvalid = errors.New("invalid")

// ErrPayloadTooLarge is a job whose payload is larger than MaxPayload.
// Enqueue's error for one wraps it, and ErrInvalid too.
var ErrPayloadTooLarge = errors.New("payload too large")

// MaxPayload is the largest payload Enqueue accepts, in bytes: 1 MiB.
const MaxPayload = 1 << 20

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
		return Kind{}, explain.New(ErrInvalid, "jobs.NewKind was given %q, which is not a job kind, at most %d bytes of "+
			"lower-case words joined by dots, such as crypto.reencrypt. This is a bug in Pisati", s, maxKindBytes)
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
// it is enqueued: Enqueue adds no job whose key a job still kept, in any
// state, has. A done or dead job is kept until a sweep deletes it, once
// it is past Options.KeepDone or Options.KeepDead, and its key is free
// from then. The zero IdempotencyKey is no key, and a job without one
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
		return IdempotencyKey{}, explain.New(ErrInvalid, "jobs.NewIdempotencyKey was given %d bytes, but a key is 1 to %d "+
			"bytes long. This is a bug in Pisati", len(s), maxKeyBytes)
	}
	for i := range len(s) {
		if s[i] < ' ' || s[i] > '~' {
			return IdempotencyKey{}, explain.New(ErrInvalid, "jobs.NewIdempotencyKey was given a key whose byte %d is not "+
				"a printable ASCII character. This is a bug in Pisati", i)
		}
	}
	return IdempotencyKey{key: s}, nil
}

// Job is a job to enqueue.
type Job struct {
	// Kind picks the Handler that runs the job. It is never the zero Kind.
	Kind Kind
	// Payload is what the Handler is given: at most MaxPayload bytes. nil
	// is stored as empty.
	Payload []byte
	// Key, unless it is the zero key, has Enqueue add the job only if no
	// job still kept has the same key.
	Key IdempotencyKey
	// RunAt is the earliest time the job may run, to the millisecond. The
	// zero time means as soon as a worker is free.
	RunAt time.Time
}

// Enqueue adds j to the queue through q, ready to run from j.RunAt, and
// reports whether it did: false, with no error, when j has a key that a
// job still kept has, in which case nothing changes. It is meant to
// run in the caller's transaction (Store.InTx), so that the job commits or
// rolls back with the change that caused it.
//
// Enqueue reads no clock, so the caller gives RunAt; and it wakes no
// Worker, so the caller calls Worker.Notify once the transaction commits,
// or the job waits for a worker's next poll. A job with the zero Kind, or
// a nil q, is refused with an error wrapping ErrInvalid; a payload larger
// than MaxPayload, with one wrapping ErrPayloadTooLarge and ErrInvalid.
// Either way, Enqueue writes nothing.
func Enqueue(ctx context.Context, q *storedb.Queries, j Job) (bool, error) {
	if j.Kind == (Kind{}) {
		return false, explain.New(ErrInvalid, "jobs.Enqueue was given a job with no kind, which is a bug in Pisati")
	}
	if q == nil {
		return false, explain.New(ErrInvalid, "jobs.Enqueue was given no queries for a %s job, which is a bug in Pisati", j.Kind)
	}
	if len(j.Payload) > MaxPayload {
		return false, explain.New(errors.Join(ErrInvalid, ErrPayloadTooLarge), "the payload of a %s job is %s, more "+
			"than the %s a job can hold, so Pisati does not queue it", j.Kind, explain.Bytes(int64(len(j.Payload))),
			explain.Bytes(MaxPayload))
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
		return false, explain.Wrap(err, "Pisati cannot queue a %s job", j.Kind)
	}
	return added == 1, nil
}
