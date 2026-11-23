// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package jobs_test

import (
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/divbast/pisati/internal/jobs"
	"github.com/divbast/pisati/internal/store/storedb"
)

// The kinds the constructor tests try, each accepted or refused.
var (
	validKinds = []string{
		"crypto.reencrypt",
		"a.b",
		"post.render_html",
		"a1.b2_c3",
		"x.y.z",
		"a." + strings.Repeat("b", 62), // 64 bytes
	}
	invalidKinds = []string{
		"",
		"crypto",
		"Crypto.reencrypt",
		"crypto.Reencrypt",
		"crypto.",
		".crypto",
		"crypto..reencrypt",
		"crypto_x.reencrypt", // an underscore in the first word
		"crypto.1reencrypt",
		"crypto._reencrypt",
		"1crypto.reencrypt",
		"crypto.re-encrypt",
		"crypto.re encrypt",
		"crypto.reencrypt\n",
		"kryptó.reencrypt",
		"a." + strings.Repeat("b", 63), // 65 bytes
	}
)

// The idempotency keys the constructor tests try, each accepted or
// refused.
var (
	validKeys = []string{
		"a",
		" ",
		"~",
		"post:7/federate?to=https://example.org/inbox#1",
		strings.Repeat("k", 200),
	}
	invalidKeys = []string{
		"",
		strings.Repeat("k", 201),
		"tab\there",
		"line\nbreak",
		"del\x7f",
		"nul\x00",
		"clé",
	}
)

func TestNewKind(t *testing.T) {
	for _, s := range validKinds {
		k, err := jobs.NewKind(s)
		if err != nil {
			t.Errorf("NewKind(%q): %v, want it accepted", s, err)
			continue
		}
		if k.String() != s {
			t.Errorf("NewKind(%q).String() = %q", s, k.String())
		}
	}
	for _, s := range invalidKinds {
		k, err := jobs.NewKind(s)
		if !errors.Is(err, jobs.ErrInvalid) {
			t.Errorf("NewKind(%q) = %v, %v; want an error wrapping ErrInvalid", s, k, err)
		}
		if k != (jobs.Kind{}) {
			t.Errorf("NewKind(%q) returned %q with its error, want the zero Kind", s, k)
		}
	}
	if s := (jobs.Kind{}).String(); s != "" {
		t.Errorf("the zero Kind's String() = %q, want empty", s)
	}
}

func TestNewIdempotencyKey(t *testing.T) {
	for _, s := range validKeys {
		if _, err := jobs.NewIdempotencyKey(s); err != nil {
			t.Errorf("NewIdempotencyKey(%q): %v, want it accepted", s, err)
		}
	}
	for _, s := range invalidKeys {
		k, err := jobs.NewIdempotencyKey(s)
		if !errors.Is(err, jobs.ErrInvalid) {
			t.Errorf("NewIdempotencyKey(%q): %v, want an error wrapping ErrInvalid", s, err)
		}
		if k != (jobs.IdempotencyKey{}) {
			t.Errorf("NewIdempotencyKey(%q) returned a key with its error, want the zero key", s)
		}
		// Short keys are skipped: one or two bytes may well appear in any
		// message.
		if err != nil && len(s) > 3 && strings.Contains(err.Error(), s) {
			t.Errorf("NewIdempotencyKey(%q): %v; a key may hold personal data, so the error must not echo it", s, err)
		}
	}
}

// kindRule is the rule NewKind's doc comment states, as a regular
// expression: an oracle written apart from the code it checks.
const kindRule = `^[a-z][a-z0-9]*(\.[a-z][a-z0-9_]*)+$`

func FuzzNewKind(f *testing.F) {
	for _, s := range append(validKinds, invalidKinds...) {
		f.Add(s)
	}
	rule := regexp.MustCompile(kindRule)
	f.Fuzz(func(t *testing.T, s string) {
		want := len(s) <= 64 && rule.MatchString(s)
		k, err := jobs.NewKind(s)
		switch {
		case want && err != nil:
			t.Fatalf("NewKind(%q): %v, want it accepted", s, err)
		case want && k.String() != s:
			t.Fatalf("NewKind(%q).String() = %q", s, k.String())
		case !want && !errors.Is(err, jobs.ErrInvalid):
			t.Fatalf("NewKind(%q) = %q, %v; want an error wrapping ErrInvalid", s, k, err)
		case !want && k != (jobs.Kind{}):
			t.Fatalf("NewKind(%q) returned %q with its error", s, k)
		}
	})
}

func FuzzNewIdempotencyKey(f *testing.F) {
	for _, s := range append(validKeys, invalidKeys...) {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		want := len(s) >= 1 && len(s) <= 200 && strings.IndexFunc(s, func(r rune) bool {
			return r < ' ' || r > '~'
		}) < 0
		k, err := jobs.NewIdempotencyKey(s)
		switch {
		case want && err != nil:
			t.Fatalf("NewIdempotencyKey(%q): %v, want it accepted", s, err)
		case !want && !errors.Is(err, jobs.ErrInvalid):
			t.Fatalf("NewIdempotencyKey(%q): %v, want an error wrapping ErrInvalid", s, err)
		case !want && k != (jobs.IdempotencyKey{}):
			t.Fatalf("NewIdempotencyKey(%q) returned a key with its error", s)
		}
	})
}

func TestEnqueueIsIdempotent(t *testing.T) {
	s := newStore(t)
	k := kind(t, "test.idempotent")
	once := key(t, "post:7:federate")

	if !enqueue(t, s, jobs.Job{Kind: k, Payload: []byte("first"), Key: once}) {
		t.Fatal("the first job with a key was not added")
	}
	for name, j := range map[string]jobs.Job{
		"the same job":    {Kind: k, Payload: []byte("first"), Key: once},
		"another payload": {Kind: k, Payload: []byte("second"), Key: once},
		"another kind":    {Kind: kind(t, "test.other"), Payload: []byte("first"), Key: once},
		"another time":    {Kind: k, Payload: []byte("first"), Key: once, RunAt: time.UnixMilli(1)},
	} {
		if enqueue(t, s, j) {
			t.Errorf("%s with a key already queued was added", name)
		}
	}
	for range 2 {
		if !enqueue(t, s, jobs.Job{Kind: k}) {
			t.Error("a job without a key was not added")
		}
	}

	all := jobRows(t, s)
	if len(all) != 3 {
		t.Fatalf("%d jobs, want the keyed one and the two without a key: %+v", len(all), all)
	}
	if first := all[0]; string(first.Payload) != "first" || first.Kind != "test.idempotent" {
		t.Errorf("the keyed job is now %s %q; a duplicate must change nothing", first.Kind, first.Payload)
	}
	for _, j := range all {
		wantRow(t, j, "ready", 0, "")
	}
}

func TestEnqueueRollsBackWithCallerTx(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	j := jobs.Job{Kind: kind(t, "test.outbox"), Payload: []byte("effect"), Key: key(t, "rolled-back")}

	errStop := errors.New("the domain change failed")
	err := s.InTx(ctx, func(q *storedb.Queries) error {
		added, err := jobs.Enqueue(ctx, q, j)
		if err != nil {
			return err
		}
		if !added {
			t.Error("Enqueue in a transaction did not add the job")
		}
		return errStop
	})
	if !errors.Is(err, errStop) {
		t.Fatalf("InTx: %v, want the error fn returned", err)
	}
	if all := jobRows(t, s); len(all) != 0 {
		t.Fatalf("%d jobs after the caller's transaction rolled back, want none: %+v", len(all), all)
	}
	if !enqueue(t, s, j) {
		t.Error("the key of a job rolled back is still taken")
	}
}

func TestEnqueueStoresTheJob(t *testing.T) {
	s := newStore(t)
	runAt := time.Date(2030, 1, 2, 3, 4, 5, 678_000_000, time.UTC)
	enqueue(t, s, jobs.Job{Kind: kind(t, "test.later"), Payload: []byte{0, 1, 2}, RunAt: runAt})
	enqueue(t, s, jobs.Job{Kind: kind(t, "test.now")})

	all := jobRows(t, s)
	if len(all) != 2 {
		t.Fatalf("%d jobs, want 2", len(all))
	}
	later, now := all[0], all[1]
	if later.RunAt != runAt.UnixMilli() || string(later.Payload) != "\x00\x01\x02" || later.IdempotencyKey.Valid {
		t.Errorf("stored %+v, want run_at %d, the payload and no key", later, runAt.UnixMilli())
	}
	if now.RunAt != 0 || len(now.Payload) != 0 {
		t.Errorf("a job with no RunAt or payload stored as %+v, want run_at 0 and an empty payload", now)
	}
	// A scan cannot tell an empty BLOB from NULL; SQLite can.
	var stored string
	if err := s.DB().QueryRowContext(t.Context(), "SELECT typeof(payload) FROM jobs WHERE id = ?", now.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != "blob" {
		t.Errorf("a nil payload is stored as %s, want an empty blob", stored)
	}
}

func TestEnqueueRefusesLargePayload(t *testing.T) {
	if jobs.MaxPayload != 1<<20 {
		t.Errorf("MaxPayload = %d, want 1 MiB", jobs.MaxPayload)
	}
	s := newStore(t)
	ctx := t.Context()
	k := kind(t, "test.large")
	if !enqueue(t, s, jobs.Job{Kind: k, Payload: make([]byte, jobs.MaxPayload)}) {
		t.Fatal("a payload of exactly 1 MiB was not added")
	}
	// The transaction commits: Enqueue itself must write nothing.
	if err := s.InTx(ctx, func(q *storedb.Queries) error {
		added, err := jobs.Enqueue(ctx, q, jobs.Job{Kind: k, Payload: make([]byte, jobs.MaxPayload+1)})
		if added || !errors.Is(err, jobs.ErrPayloadTooLarge) || !errors.Is(err, jobs.ErrInvalid) {
			t.Errorf("Enqueue of 1 MiB + 1 byte = %t, %v; want false and an error wrapping ErrPayloadTooLarge and ErrInvalid", added, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	all := jobRows(t, s)
	if len(all) != 1 || len(all[0].Payload) != jobs.MaxPayload {
		t.Errorf("%d jobs stored, want only the 1 MiB one", len(all))
	}
}

func TestEnqueueRefusesInvalidJob(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	err := s.InTx(ctx, func(q *storedb.Queries) error {
		added, err := jobs.Enqueue(ctx, q, jobs.Job{Payload: []byte("no kind")})
		if added {
			t.Error("Enqueue added a job with the zero Kind")
		}
		return err
	})
	if !errors.Is(err, jobs.ErrInvalid) {
		t.Errorf("Enqueue of a job with the zero Kind: %v, want an error wrapping ErrInvalid", err)
	}
	if _, err := jobs.Enqueue(ctx, nil, jobs.Job{Kind: kind(t, "test.nowhere")}); !errors.Is(err, jobs.ErrInvalid) {
		t.Errorf("Enqueue with no queries: %v, want an error wrapping ErrInvalid", err)
	}
	if all := jobRows(t, s); len(all) != 0 {
		t.Errorf("%d jobs, want none", len(all))
	}
}
