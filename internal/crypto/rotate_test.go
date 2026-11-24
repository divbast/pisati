// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package crypto_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/divbast/pisati/internal/crypto"
	"github.com/divbast/pisati/internal/jobs"
	"github.com/divbast/pisati/internal/store"
)

func TestRewrap(t *testing.T) {
	s := newStore(t)
	old, next := newMainKey(t), newMainKey(t)
	k := openKeyring(t, s, old)
	first := seal(t, k, "first", "assoc")
	if err := crypto.Rotate(t.Context(), s, k); err != nil {
		t.Fatal(err)
	}
	second := seal(t, k, "second", "assoc")
	before := dataKeys(t, s)

	t.Run("refuses a main key that does not unwrap the keys", func(t *testing.T) {
		if err := crypto.Rewrap(t.Context(), s, newMainKey(t), next); !errors.Is(err, crypto.ErrMainKeyMismatch) {
			t.Fatalf("Rewrap from another main key: %v, want ErrMainKeyMismatch", err)
		}
		if after := dataKeys(t, s); !sameKeys(before, after) {
			t.Error("a refused Rewrap changed the data keys")
		}
	})

	t.Run("refuses a main key that unwraps only some", func(t *testing.T) {
		// The last key is one old cannot unwrap, so a Rewrap that wrote
		// each key as it went would have changed the first by then.
		damageKey(t, s, 2)
		damaged := dataKeys(t, s)
		if err := crypto.Rewrap(t.Context(), s, old, next); !errors.Is(err, crypto.ErrMainKeyMismatch) {
			t.Fatalf("Rewrap with a damaged key: %v, want ErrMainKeyMismatch", err)
		}
		if after := dataKeys(t, s); !sameKeys(damaged, after) {
			t.Error("a refused Rewrap changed the data keys: it committed part of its work")
		}
		setWrapped(t, s, 2, before[1].wrapped)
	})

	t.Run("refuses what it cannot use", func(t *testing.T) {
		var zero crypto.MainKey
		for _, c := range []struct {
			name     string
			s        *store.Store
			from, to crypto.MainKey
		}{
			{"no store", nil, old, next},
			{"the zero old main key", s, zero, next},
			{"the zero new main key", s, old, zero},
		} {
			if err := crypto.Rewrap(t.Context(), c.s, c.from, c.to); !errors.Is(err, crypto.ErrInvalid) {
				t.Errorf("%s: %v, want ErrInvalid", c.name, err)
			}
		}
		if after := dataKeys(t, s); !sameKeys(before, after) {
			t.Error("a refused Rewrap changed the data keys")
		}
	})

	if err := crypto.Rewrap(t.Context(), s, old, next); err != nil {
		t.Fatalf("Rewrap: %v", err)
	}
	after := dataKeys(t, s)
	if len(after) != len(before) {
		t.Fatalf("%d data keys after Rewrap, want %d", len(after), len(before))
	}
	for i := range after {
		if after[i].id != before[i].id || after[i].state != before[i].state || after[i].createdAt != before[i].createdAt {
			t.Errorf("Rewrap changed key %d beyond its wrapping: %+v, was %+v", before[i].id, after[i], before[i])
		}
		if bytes.Equal(after[i].wrapped, before[i].wrapped) {
			t.Errorf("Rewrap left key %d wrapped as it was", before[i].id)
		}
	}
	if _, err := crypto.OpenKeyring(t.Context(), s, old, clock, rand.Reader); !errors.Is(err, crypto.ErrMainKeyMismatch) {
		t.Errorf("OpenKeyring with the old main key after Rewrap: %v, want ErrMainKeyMismatch", err)
	}
	reopened := openKeyring(t, s, next)
	wantOpens(t, reopened, first, "assoc", "first")
	wantOpens(t, reopened, second, "assoc", "second")
	if keyIDOf(seal(t, reopened, "third", "")) != 2 {
		t.Error("after Rewrap the current data key is not the one it was")
	}
}

func TestRotateSwitchesOnlyAfterCommit(t *testing.T) {
	s := newStore(t)
	m := newMainKey(t)
	k := openKeyring(t, s, m)
	// A trigger on the job Rotate enqueues adds a row that breaks a
	// deferred foreign key, which SQLite checks only at the commit: the
	// commit fails, after everything else Rotate does has succeeded.
	exec(t, s,
		`CREATE TABLE guard_parent (id INTEGER PRIMARY KEY) STRICT`,
		`CREATE TABLE guard (parent INTEGER REFERENCES guard_parent (id) DEFERRABLE INITIALLY DEFERRED) STRICT`,
		`CREATE TRIGGER guard_jobs AFTER INSERT ON jobs BEGIN INSERT INTO guard (parent) VALUES (1); END`)
	before := dataKeys(t, s)

	if err := crypto.Rotate(t.Context(), s, k); err == nil {
		t.Fatal("Rotate committed despite the guard")
	}
	if after := dataKeys(t, s); !sameKeys(before, after) {
		t.Fatalf("a Rotate that did not commit changed the data keys: %+v", after)
	}
	if j := jobRows(t, s); len(j) != 0 {
		t.Fatalf("a Rotate that did not commit left %d jobs", len(j))
	}
	sealed := seal(t, k, "value", "assoc")
	if id := keyIDOf(sealed); id != 1 {
		t.Fatalf("after a Rotate that did not commit, Seal uses key %d, want 1: the keyring switched before the commit", id)
	}
	wantOpens(t, openKeyring(t, s, m), sealed, "assoc", "value")

	exec(t, s, `DROP TRIGGER guard_jobs`)
	if err := crypto.Rotate(t.Context(), s, k); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if id := keyIDOf(seal(t, k, "value", "assoc")); id != 2 {
		t.Errorf("after Rotate, Seal uses key %d, want 2", id)
	}
	wantOpens(t, k, sealed, "assoc", "value")
	keys := dataKeys(t, s)
	if len(keys) != 2 || keys[0].state != "retired" || keys[1].state != "current" || keys[1].createdAt != clock().UnixMilli() {
		t.Errorf("data keys %+v, want key 1 retired and key 2 current", keys)
	}
}

func TestRotateRefusesWhatItCannotUse(t *testing.T) {
	s := newStore(t)
	m := newMainKey(t)
	k := openKeyring(t, s, m)
	before := dataKeys(t, s)
	for _, c := range []struct {
		name string
		s    *store.Store
		k    *crypto.Keyring
		want error
	}{
		{"no keyring", s, nil, crypto.ErrInvalid},
		{"the zero keyring", s, new(crypto.Keyring), crypto.ErrInvalid},
		{"another store", newStore(t), k, crypto.ErrInvalid},
	} {
		if err := crypto.Rotate(t.Context(), c.s, c.k); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}

	// A keyring whose main key the database's keys are no longer wrapped
	// by would wrap a new key so that nothing could open the keyring.
	if err := crypto.Rewrap(t.Context(), s, m, newMainKey(t)); err != nil {
		t.Fatal(err)
	}
	rewrapped := dataKeys(t, s)
	if err := crypto.Rotate(t.Context(), s, k); !errors.Is(err, crypto.ErrMainKeyMismatch) {
		t.Errorf("Rotate after Rewrap, on the keyring opened before: %v, want ErrMainKeyMismatch", err)
	}
	if after := dataKeys(t, s); !sameKeys(rewrapped, after) || len(after) != len(before) {
		t.Error("a refused Rotate changed the data keys")
	}
	if id := keyIDOf(seal(t, k, "value", "")); id != 1 {
		t.Errorf("after a refused Rotate, Seal uses key %d, want 1", id)
	}
}

// secrets is the test table whose fields the tests register.
const secrets = `CREATE TABLE secrets (id INTEGER PRIMARY KEY, value BLOB, note BLOB) STRICT`

// insertSecret stores value and note, each sealed with k for its field
// and row unless nil, as row id of secrets.
func insertSecret(t *testing.T, s *store.Store, k *crypto.Keyring, id int64, value, note []byte) {
	t.Helper()
	var sealed [2][]byte
	for i, c := range []struct {
		f     crypto.Field
		plain []byte
	}{{valueField(), value}, {noteField(), note}} {
		if c.plain == nil {
			continue
		}
		var err error
		if sealed[i], err = k.Seal(c.plain, c.f.Assoc(id)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.DB().ExecContext(t.Context(), `INSERT INTO secrets (id, value, note) VALUES (?, ?, ?)`,
		id, sealed[0], sealed[1]); err != nil {
		t.Fatal(err)
	}
}

func valueField() crypto.Field {
	return crypto.Field{Table: "secrets", IDColumn: "id", Column: "value"}
}

func noteField() crypto.Field { return crypto.Field{Table: "secrets", IDColumn: "id", Column: "note"} }

// secretRow is one row of secrets, as stored.
type secretRow struct {
	id          int64
	value, note []byte
}

// secretRows returns every row of secrets, in the order of their IDs.
func secretRows(t *testing.T, s *store.Store) []secretRow {
	t.Helper()
	rows, err := s.DB().QueryContext(t.Context(), `SELECT id, value, note FROM secrets ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	var all []secretRow
	for rows.Next() {
		var r secretRow
		if err := rows.Scan(&r.id, &r.value, &r.note); err != nil {
			t.Fatal(errors.Join(err, rows.Close()))
		}
		all = append(all, r)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	return all
}

// plainValue and plainNote are what the tests seal in row id; every third
// row has no note.
func plainValue(id int64) []byte { return fmt.Appendf(nil, "value %d", id) }

func plainNote(id int64) []byte {
	if id%3 == 0 {
		return nil
	}
	return fmt.Appendf(nil, "note %d", id)
}

func TestRotateReencryptsRegisteredFields(t *testing.T) {
	s := newStore(t)
	exec(t, s, secrets)
	k := openKeyring(t, s, newMainKey(t))
	re, err := crypto.NewReencrypter(k, valueField(), noteField())
	if err != nil {
		t.Fatalf("NewReencrypter: %v", err)
	}
	if re.Kind().String() != "crypto.reencrypt" {
		t.Errorf("the job kind is %q, want crypto.reencrypt", re.Kind())
	}
	// More than two batches of 100.
	const rows = 250
	for id := int64(1); id <= rows; id++ {
		insertSecret(t, s, k, id, plainValue(id), plainNote(id))
	}

	if err := crypto.Rotate(t.Context(), s, k); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	queued := jobRows(t, s)
	if len(queued) != 1 || queued[0].kind != "crypto.reencrypt" || queued[0].state != "ready" || queued[0].key == "" {
		t.Fatalf("jobs %+v, want one crypto.reencrypt job, ready, with an idempotency key", queued)
	}
	if !strings.Contains(queued[0].key, "2") {
		t.Errorf("the job's idempotency key %q does not come from the new key's ID, 2", queued[0].key)
	}
	// Rows written after the rotation are sealed under the new key from
	// the start, and the job leaves them as they are.
	insertSecret(t, s, k, rows+1, plainValue(rows+1), plainNote(rows+1))
	fresh := secretRows(t, s)[rows]

	synctest.Test(t, func(t *testing.T) {
		log, rec := newLogger()
		opts := jobs.DefaultOptions()
		opts.Workers = 1
		w, err := jobs.NewWorker(s, map[jobs.Kind]jobs.Handler{re.Kind(): re.Handle}, time.Now, rand.Reader, log, opts)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- w.Run(ctx) }()
		synctest.Wait()
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run: %v", err)
		}
		for _, r := range rec.all() {
			if r.level >= slog.LevelWarn {
				t.Errorf("the worker logged %s %q %v", r.level, r.msg, r.attrs)
			}
		}

		ran := jobRows(t, s)
		if len(ran) != 1 || ran[0].state != "done" || ran[0].attempts != 1 {
			t.Fatalf("jobs %+v, want the one done at its first attempt", ran)
		}
		stored := secretRows(t, s)
		if len(stored) != rows+1 {
			t.Fatalf("%d rows, want %d", len(stored), rows+1)
		}
		for _, r := range stored {
			for _, c := range []struct {
				f             crypto.Field
				sealed, plain []byte
			}{{valueField(), r.value, plainValue(r.id)}, {noteField(), r.note, plainNote(r.id)}} {
				if c.plain == nil {
					if c.sealed != nil {
						t.Errorf("row %d %s: an empty field was given a value", r.id, c.f.Column)
					}
					continue
				}
				if id := keyIDOf(c.sealed); id != 2 {
					t.Errorf("row %d %s: sealed under key %d, want 2", r.id, c.f.Column, id)
				}
				// Re-sealed for the field and row it lives in.
				wantOpens(t, k, c.sealed, string(c.f.Assoc(r.id)), string(c.plain))
			}
		}
		if last := stored[rows]; !bytes.Equal(last.value, fresh.value) || !bytes.Equal(last.note, fresh.note) {
			t.Error("the job re-sealed a value already under the current key")
		}

		// Run again, it finds nothing to do: idempotent.
		if err := re.Handle(t.Context(), ran[0].payload); err != nil {
			t.Fatalf("Handle again: %v", err)
		}
		again := secretRows(t, s)
		for i := range again {
			if !bytes.Equal(again[i].value, stored[i].value) || !bytes.Equal(again[i].note, stored[i].note) {
				t.Fatalf("running the job again changed row %d", again[i].id)
			}
		}
	})
}

func TestReencryptLeavesWhatItCannotOpen(t *testing.T) {
	s := newStore(t)
	exec(t, s, secrets)
	k := openKeyring(t, s, newMainKey(t))
	re, err := crypto.NewReencrypter(k, valueField())
	if err != nil {
		t.Fatal(err)
	}
	for id := int64(1); id <= 5; id++ {
		insertSecret(t, s, k, id, plainValue(id), nil)
	}
	// The cursor starts before the first row, whatever its ID.
	insertSecret(t, s, k, math.MinInt64, plainValue(0), nil)
	// Row 2 holds a value sealed for another row; row 4, no sealed value.
	moved, err := k.Seal(plainValue(2), valueField().Assoc(99))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(t.Context(), `UPDATE secrets SET value = ? WHERE id = 2`, moved); err != nil {
		t.Fatal(err)
	}
	exec(t, s, `UPDATE secrets SET value = x'0102' WHERE id = 4`)
	if err := crypto.Rotate(t.Context(), s, k); err != nil {
		t.Fatal(err)
	}
	payload := jobRows(t, s)[0].payload

	err = re.Handle(t.Context(), payload)
	if err == nil {
		t.Fatal("Handle with two values it cannot open: no error")
	}
	if _, permanent := errors.AsType[*jobs.Permanent](err); permanent {
		t.Errorf("Handle: %v, a permanent failure; the job should be tried again", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "secrets.value") || !strings.Contains(msg, "2") {
		t.Errorf("Handle: %q; want it to name the field and how many values it left", msg)
	}
	for _, r := range secretRows(t, s) {
		switch r.id {
		case 2:
			if !bytes.Equal(r.value, moved) {
				t.Error("row 2 changed")
			}
		case 4:
			if !bytes.Equal(r.value, []byte{1, 2}) {
				t.Error("row 4 changed")
			}
		default:
			if r.id == math.MinInt64 {
				wantOpens(t, k, r.value, string(valueField().Assoc(r.id)), string(plainValue(0)))
			}
			if keyIDOf(r.value) != 2 {
				t.Errorf("row %d: sealed under key %d, want 2: the rows it can open are re-sealed all the same", r.id, keyIDOf(r.value))
			}
			if r.id != math.MinInt64 {
				wantOpens(t, k, r.value, string(valueField().Assoc(r.id)), string(plainValue(r.id)))
			}
		}
	}

	t.Run("a payload that is no key ID", func(t *testing.T) {
		for _, p := range [][]byte{nil, {1, 2, 3}, {0, 0, 0, 0, 2}} {
			err := re.Handle(t.Context(), p)
			if _, permanent := errors.AsType[*jobs.Permanent](err); !permanent {
				t.Errorf("Handle(% x): %v, want a permanent failure", p, err)
			}
		}
	})
	t.Run("a key the keyring does not hold", func(t *testing.T) {
		err := re.Handle(t.Context(), binary.BigEndian.AppendUint32(nil, 9))
		if err == nil {
			t.Fatal("Handle for key 9: no error")
		}
		if _, permanent := errors.AsType[*jobs.Permanent](err); permanent {
			t.Errorf("Handle for key 9: %v, a permanent failure; a restart would know the key", err)
		}
	})
}

func TestReencrypterRegistersFields(t *testing.T) {
	s := newStore(t)
	k := openKeyring(t, s, newMainKey(t))
	good := crypto.Field{Table: "_t1", IDColumn: "id", Column: "a_b"}
	if _, err := crypto.NewReencrypter(k, good); err != nil {
		t.Errorf("NewReencrypter(%+v): %v", good, err)
	}
	if _, err := crypto.NewReencrypter(k); err != nil {
		t.Errorf("NewReencrypter with no fields, as stage 0 has: %v", err)
	}
	for _, bad := range []string{"", "Users", "1st", "a-b", "a b", `a"b`, "a.b", "ä", "a;b"} {
		for _, f := range []crypto.Field{
			{Table: bad, IDColumn: "id", Column: "value"},
			{Table: "secrets", IDColumn: bad, Column: "value"},
			{Table: "secrets", IDColumn: "id", Column: bad},
		} {
			if _, err := crypto.NewReencrypter(k, f); !errors.Is(err, crypto.ErrInvalid) {
				t.Errorf("NewReencrypter(%+v): %v, want ErrInvalid", f, err)
			}
		}
	}
	if _, err := crypto.NewReencrypter(k, valueField(), noteField(), valueField()); !errors.Is(err, crypto.ErrInvalid) {
		t.Errorf("NewReencrypter with a field twice: %v, want ErrInvalid", err)
	}
	for _, k := range []*crypto.Keyring{nil, new(crypto.Keyring)} {
		if _, err := crypto.NewReencrypter(k, valueField()); !errors.Is(err, crypto.ErrInvalid) {
			t.Errorf("NewReencrypter(%v): %v, want ErrInvalid", k, err)
		}
	}
}

func TestFieldAssoc(t *testing.T) {
	f := valueField()
	seen := map[string]string{}
	for _, c := range []struct {
		name string
		f    crypto.Field
		id   int64
	}{
		{"the field", f, 1},
		{"another row", f, 2},
		{"row 11", f, 11},
		{"a negative row", f, -1},
		{"another column", crypto.Field{Table: "secrets", IDColumn: "id", Column: "note"}, 1},
		{"another table", crypto.Field{Table: "others", IDColumn: "id", Column: "value"}, 1},
		{"the names split elsewhere", crypto.Field{Table: "secretsv", IDColumn: "id", Column: "alue"}, 1},
		{"the names split elsewhere again", crypto.Field{Table: "secrets_", IDColumn: "id", Column: "value"}, 1},
		{"a column whose name runs into the row ID", crypto.Field{Table: "secrets", IDColumn: "id", Column: "value1"}, 1},
	} {
		a := string(c.f.Assoc(c.id))
		if other, ok := seen[a]; ok {
			t.Errorf("%s and %s share associated data %q", c.name, other, a)
		}
		seen[a] = c.name
	}
	// The ID column names how a row is found, not where the value lives.
	if !bytes.Equal(f.Assoc(1), crypto.Field{Table: "secrets", IDColumn: "rowid", Column: "value"}.Assoc(1)) {
		t.Error("the ID column changes the associated data")
	}
	a := f.Assoc(1)
	a[0] ^= 0xff
	if bytes.Equal(a, f.Assoc(1)) {
		t.Error("Assoc returns a buffer it keeps")
	}
}

func TestOneCurrentKey(t *testing.T) {
	s := newStore(t)
	m := newMainKey(t)
	// Two keyrings on one database, as two processes would have: the
	// second's current key is no longer the database's once the first
	// rotates.
	first, second := openKeyring(t, s, m), openKeyring(t, s, m)
	if err := crypto.Rotate(t.Context(), s, first); err != nil {
		t.Fatalf("Rotate the first keyring: %v", err)
	}
	before := dataKeys(t, s)
	if err := crypto.Rotate(t.Context(), s, second); err == nil {
		t.Fatal("Rotate of a keyring whose current key is retired: no error")
	}
	keys := dataKeys(t, s)
	if !sameKeys(before, keys) {
		t.Errorf("the refused Rotate changed the data keys: %+v, was %+v", keys, before)
	}
	var current []int64
	for _, k := range keys {
		if k.state == "current" {
			current = append(current, k.id)
		}
	}
	if len(current) != 1 || current[0] != 2 {
		t.Errorf("current data keys %v, want key 2 alone", current)
	}

	// The schema holds it too, whatever writes to the table.
	if _, err := s.DB().ExecContext(t.Context(),
		`INSERT INTO data_keys (id, wrapped, created_at, state) VALUES (9, x'00', 0, 'current')`); err == nil {
		t.Error("the schema let a second data key be current")
	}
	if _, err := s.DB().ExecContext(t.Context(),
		`INSERT INTO data_keys (id, wrapped, created_at, state) VALUES (10, x'00', 0, 'retired')`); err != nil {
		t.Errorf("the schema refused another retired data key: %v", err)
	}
}

func TestReencryptCommitsEachBatch(t *testing.T) {
	s := newStore(t)
	exec(t, s, secrets)
	k := openKeyring(t, s, newMainKey(t))
	re, err := crypto.NewReencrypter(k, valueField())
	if err != nil {
		t.Fatal(err)
	}
	const rows = 250
	for id := int64(1); id <= rows; id++ {
		insertSecret(t, s, k, id, plainValue(id), nil)
	}
	if err := crypto.Rotate(t.Context(), s, k); err != nil {
		t.Fatal(err)
	}
	payload := jobRows(t, s)[0].payload

	// Stopped in its second batch, the job keeps the first.
	exec(t, s, `CREATE TRIGGER stop_at_150 BEFORE UPDATE ON secrets WHEN NEW.id = 150
		BEGIN SELECT RAISE(ABORT, 'stopped at row 150'); END`)
	if err := re.Handle(t.Context(), payload); err == nil {
		t.Fatal("Handle stopped at row 150: no error")
	}
	for _, r := range secretRows(t, s) {
		want := uint32(1)
		if r.id <= 100 {
			want = 2
		}
		if got := keyIDOf(r.value); got != want {
			t.Errorf("row %d under key %d after the stop, want %d: each batch of 100 commits on its own", r.id, got, want)
		}
		wantOpens(t, k, r.value, string(valueField().Assoc(r.id)), string(plainValue(r.id)))
	}

	// Run again, it takes up where it stopped.
	exec(t, s, `DROP TRIGGER stop_at_150`)
	if err := re.Handle(t.Context(), payload); err != nil {
		t.Fatalf("Handle again: %v", err)
	}
	for _, r := range secretRows(t, s) {
		if got := keyIDOf(r.value); got != 2 {
			t.Errorf("row %d under key %d, want 2", r.id, got)
		}
		wantOpens(t, k, r.value, string(valueField().Assoc(r.id)), string(plainValue(r.id)))
	}
}

func TestReencryptRefusesAnIDColumnThatIsNotUnique(t *testing.T) {
	s := newStore(t)
	exec(t, s, `CREATE TABLE dupes (rid INTEGER, value BLOB) STRICT`)
	f := crypto.Field{Table: "dupes", IDColumn: "rid", Column: "value"}
	k := openKeyring(t, s, newMainKey(t))
	re, err := crypto.NewReencrypter(k, f)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		id    int64
		value string
	}{{1, "one"}, {1, "another one"}, {2, "two"}} {
		if _, err := s.DB().ExecContext(t.Context(), `INSERT INTO dupes (rid, value) VALUES (?, ?)`,
			row.id, seal(t, k, row.value, string(f.Assoc(row.id)))); err != nil {
			t.Fatal(err)
		}
	}
	if err := crypto.Rotate(t.Context(), s, k); err != nil {
		t.Fatal(err)
	}
	before := dupeValues(t, s)

	// Re-sealing row 1 would write both rows of ID 1, the one value over
	// the other.
	err = re.Handle(t.Context(), jobRows(t, s)[0].payload)
	if err == nil || !strings.Contains(err.Error(), "unique") {
		t.Fatalf("Handle with an ID column that is not unique: %v; want an error saying it must be unique", err)
	}
	if after := dupeValues(t, s); !slices.EqualFunc(before, after, bytes.Equal) {
		t.Error("Handle changed rows of an ID column that is not unique")
	}
}

// dupeValues returns the values of the dupes table, in the order of its
// rows.
func dupeValues(t *testing.T, s *store.Store) [][]byte {
	t.Helper()
	rows, err := s.DB().QueryContext(t.Context(), `SELECT value FROM dupes ORDER BY rowid`)
	if err != nil {
		t.Fatal(err)
	}
	var all [][]byte
	for rows.Next() {
		var v []byte
		if err := rows.Scan(&v); err != nil {
			t.Fatal(errors.Join(err, rows.Close()))
		}
		all = append(all, v)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	return all
}
