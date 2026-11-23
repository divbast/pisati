// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package crypto

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"

	"github.com/divbast/pisati/internal/jobs"
	"github.com/divbast/pisati/internal/store"
	"github.com/divbast/pisati/internal/store/storedb"
)

const (
	// reencryptKind is the kind of job Rotate queues, and a Reencrypter
	// runs.
	reencryptKind = "crypto.reencrypt"
	// reencryptBatch is how many values one transaction of the job
	// re-seals at most, so that the job holds the write lock only
	// briefly at a time, however many values it has to re-seal.
	reencryptBatch = 100
)

// Rewrap wraps every data key in the database s under to in place of
// from, in one IMMEDIATE transaction: once it returns nil, only to opens
// the database's keyring. It returns an error wrapping ErrMasterMismatch,
// changing nothing, unless from unwraps every key. A nil or unopened s,
// or the zero Master for either, is refused with an error wrapping
// ErrInvalid. The nonces of the new wrapping come from crypto/rand. A
// Keyring opened before Rewrap still holds from, so Rotate refuses it:
// open it again with to.
func Rewrap(ctx context.Context, s *store.Store, from, to Master) error {
	var errs []error
	if s.DB() == nil {
		errs = append(errs, fmt.Errorf("crypto: rewrap the data keys: %w store: it is not open", ErrInvalid))
	}
	if from.key == nil {
		errs = append(errs, fmt.Errorf("crypto: rewrap the data keys: %w old master key: the zero Master holds none", ErrInvalid))
	}
	if to.key == nil {
		errs = append(errs, fmt.Errorf("crypto: rewrap the data keys: %w new master key: the zero Master holds none", ErrInvalid))
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	err := s.InTx(ctx, func(q *storedb.Queries) error {
		rows, err := q.ListDataKeys(ctx)
		if err != nil {
			return err
		}
		// Every key is unwrapped before any is written, so that a key
		// from cannot unwrap leaves all of them as they were.
		rewrapped := make([]storedb.RewrapDataKeyParams, 0, len(rows))
		for _, row := range rows {
			id, ok := toKeyID(row.ID)
			if !ok {
				return fmt.Errorf("data key %d: its ID is not from 1 to %d", row.ID, uint32(math.MaxUint32))
			}
			key, err := from.unwrap(id, row.Wrapped)
			if errors.Is(err, errUnwrap) {
				continue
			}
			if err != nil {
				return err
			}
			var nonce [nonceSize]byte
			if _, err := rand.Read(nonce[:]); err != nil {
				return err
			}
			wrapped, err := to.wrap(nonce, id, key)
			if err != nil {
				return err
			}
			rewrapped = append(rewrapped, storedb.RewrapDataKeyParams{Wrapped: wrapped, ID: row.ID})
		}
		if len(rewrapped) < len(rows) {
			return fmt.Errorf("%w: the old master key unwraps %d of the %d data keys, so none is rewrapped",
				ErrMasterMismatch, len(rewrapped), len(rows))
		}
		for _, p := range rewrapped {
			n, err := q.RewrapDataKey(ctx, p)
			if err != nil {
				return err
			}
			if n != 1 {
				return fmt.Errorf("data key %d: %d rows rewrapped, want 1", p.ID, n)
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("crypto: rewrap the data keys: %w", err)
	}
	return nil
}

// Rotate makes a new data key the current one, in one IMMEDIATE
// transaction on s, the database k was opened on: the key that was
// current is retired, the new one is stored wrapped by k's master and
// created at its clock's time, and a crypto.reencrypt job is queued,
// keyed by the new key's ID, for a Reencrypter to re-seal the registered
// fields under it. Only once that commits does Seal use the new key: a
// Rotate that fails leaves k and the database as they were. The job waits
// for a worker's next poll, as Rotate has no Worker to notify.
//
// Rotate refuses, with an error wrapping ErrInvalid, a Keyring OpenKeyring
// did not return, or an s other than the one it was opened on. It returns
// an error wrapping ErrMasterMismatch when the database's keys are no
// longer wrapped by k's master, as after a Rewrap, and an error when k's
// current key is no longer the database's: either way, open the keyring
// again. Rotates of one Keyring run one at a time.
func Rotate(ctx context.Context, s *store.Store, k *Keyring) error {
	if err := k.opened(); err != nil {
		return fmt.Errorf("crypto: rotate the data key: %w", err)
	}
	if s != k.store {
		return fmt.Errorf("crypto: rotate the data key: %w store: it is not the one the keyring was opened on", ErrInvalid)
	}
	kind, err := jobs.NewKind(reencryptKind)
	if err != nil {
		return fmt.Errorf("crypto: rotate the data key: %w", err)
	}
	k.rotating.Lock()
	defer k.rotating.Unlock()
	old, _ := k.currentKey()

	var id keyID
	var dk dataKey
	err = s.InTx(ctx, func(q *storedb.Queries) error {
		rows, err := q.ListDataKeys(ctx)
		if err != nil {
			return err
		}
		// Checked here, under the write lock: after a Rewrap, a key wrapped
		// by the keyring's old master, beside keys wrapped by the new one,
		// would leave no master that opens the whole keyring.
		_, current, err := unwrapAll(k.master, rows)
		if err != nil {
			return err
		}
		if current != old {
			return fmt.Errorf("the database's current data key is %d, not the keyring's, %d: open the keyring again", current, old)
		}
		retired, err := q.RetireDataKey(ctx, int64(old))
		if err != nil {
			return err
		}
		if retired != 1 {
			return fmt.Errorf("data key %d: %d rows retired, want 1", old, retired)
		}
		if id, dk, err = k.addKey(ctx, q); err != nil {
			return err
		}
		key, err := jobs.NewIdempotencyKey(reencryptKind + " " + strconv.FormatUint(uint64(id), 10))
		if err != nil {
			return err
		}
		added, err := jobs.Enqueue(ctx, q, jobs.Job{Kind: kind, Payload: id.bytes(), Key: key})
		if err == nil && !added {
			// No key's ID is used twice, so this is a bug: say so, rather
			// than leave the fields under the old key with no job to move
			// them.
			err = fmt.Errorf("a %s job for data key %d is queued already", reencryptKind, id)
		}
		return err
	})
	if err != nil {
		return fmt.Errorf("crypto: rotate the data key: %w", err)
	}
	// Installed only now: a value sealed under a key whose transaction
	// then failed would have no key left to open it.
	k.install(id, dk)
	return nil
}

// identifier is what each name of a Field must be: a table or column
// name SQLite takes quoted, with nothing in it that quoting must escape.
var identifier = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// Field is a column whose values are sealed: Column of table Table, in
// the row whose integer ID is in IDColumn. A Reencrypter re-seals the
// fields it is given; each column must hold BLOBs that Seal made, or
// NULL, which is left as it is.
type Field struct {
	Table, IDColumn, Column string
}

// Assoc returns the associated data a value of f is sealed with in the
// row id: Table, Column and id, so that the value opens only in that
// column of that row, never copied to another. Callers sealing a value of
// a registered Field must use it, as the Reencrypter does. It returns a
// new buffer on each call, and no two fields and rows share one: the
// names are quoted and the ID last.
func (f Field) Assoc(id int64) []byte {
	b := []byte("field ")
	b = strconv.AppendQuote(b, f.Table)
	b = append(b, '.')
	b = strconv.AppendQuote(b, f.Column)
	b = append(b, '#')
	return strconv.AppendInt(b, id, 10)
}

// check returns an error wrapping ErrInvalid unless each of f's names is
// an identifier: a lower-case ASCII letter or underscore, then any of
// those and digits.
func (f Field) check() error {
	for _, n := range []struct{ what, name string }{
		{"table", f.Table}, {"ID column", f.IDColumn}, {"column", f.Column},
	} {
		if !identifier.MatchString(n.name) {
			return fmt.Errorf("crypto: register a field: %w %s name %q: want a lower-case letter or underscore, then lower-case letters, digits and underscores",
				ErrInvalid, n.what, n.name)
		}
	}
	return nil
}

// Reencrypter runs the crypto.reencrypt job, which re-seals the values of
// the fields registered with it under the current data key. Make one
// with NewReencrypter, once, at start. The zero Reencrypter has no keyring:
// its Handle fails permanently.
type Reencrypter struct {
	keyring *Keyring
	kind    jobs.Kind
	fields  []registered
}

// registered is a Field, with the statements that re-seal it.
type registered struct {
	Field
	// pending selects the next rows, after an ID or from the first when
	// it is NULL, whose value is not sealed under the data key a header
	// names, up to a number of rows; reseal writes one row's value.
	pending, reseal string
}

// NewReencrypter returns the Reencrypter of k, for fields: the registry
// of sealed fields, made once at start. It refuses, with an error
// wrapping ErrInvalid naming every problem, a Keyring OpenKeyring did not
// return, a Field any of whose names is not a lower-case identifier, and a
// Field given twice. Every name goes into SQL quoted.
func NewReencrypter(k *Keyring, fields ...Field) (*Reencrypter, error) {
	var errs []error
	if err := k.opened(); err != nil {
		errs = append(errs, fmt.Errorf("crypto: register the fields: %w", err))
	}
	regs := make([]registered, 0, len(fields))
	seen := map[[2]string]bool{}
	for _, f := range fields {
		if err := f.check(); err != nil {
			errs = append(errs, err)
			continue
		}
		where := [2]string{f.Table, f.Column}
		if seen[where] {
			errs = append(errs, fmt.Errorf("crypto: register a field: %w field %s.%s: it is registered already", ErrInvalid, f.Table, f.Column))
			continue
		}
		seen[where] = true
		regs = append(regs, register(f))
	}
	kind, err := jobs.NewKind(reencryptKind)
	errs = append(errs, err)
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return &Reencrypter{keyring: k, kind: kind, fields: regs}, nil
}

// register returns f with its statements. f's names are identifiers, as
// check says, so quoting them in double quotes needs no escape.
func register(f Field) registered {
	table, id, col := `"`+f.Table+`"`, `"`+f.IDColumn+`"`, `"`+f.Column+`"`
	return registered{
		Field: f,
		pending: "SELECT " + id + ", " + col + " FROM " + table +
			" WHERE (?1 IS NULL OR " + id + " > ?1) AND " + col + " IS NOT NULL" +
			" AND substr(" + col + ", 1, " + strconv.Itoa(headerSize) + ") <> ?2" +
			" ORDER BY " + id + " LIMIT ?3",
		reseal: "UPDATE " + table + " SET " + col + " = ? WHERE " + id + " = ?",
	}
}

// Kind returns the kind of job the Reencrypter runs, crypto.reencrypt, to
// give the Worker as the key of Handle.
func (r *Reencrypter) Kind() jobs.Kind {
	if r == nil {
		return jobs.Kind{}
	}
	return r.kind
}

// Handle runs a crypto.reencrypt job, whose payload is the four-byte ID of
// the data key Rotate made. It re-seals, under the keyring's current key,
// each value of each registered field that is sealed under another, with
// the Field's Assoc for its row, in batches of up to 100 rows, each in an
// IMMEDIATE transaction of its own. So it can stop at any point and be
// run again: what it committed stays done, and what it did not is
// pending still. It makes passes over each field until one re-seals
// nothing, which also catches a value written under the old key just as
// the key rotated, and so it is idempotent too.
//
// A value that no key in the keyring opens, under its row's Assoc, is
// left as it is, and once every other is re-sealed Handle fails, naming
// the field and how many, so that the job is tried again and, failing
// still, lands in the dead letter for an operator to see. A payload that
// is no key ID fails permanently. A key the keyring does not hold, as when
// another process rotated it, fails for a later try, once Pisati restarts
// with it.
func (r *Reencrypter) Handle(ctx context.Context, payload []byte) error {
	if r == nil || r.keyring == nil {
		return &jobs.Permanent{Err: errors.New("crypto: re-encrypt: the Reencrypter was not made by NewReencrypter")}
	}
	if len(payload) != 4 {
		return &jobs.Permanent{Err: fmt.Errorf("crypto: re-encrypt: the payload is %d bytes, want a data key's four-byte ID", len(payload))}
	}
	target := keyID(binary.BigEndian.Uint32(payload))
	if _, ok := r.keyring.key(target); !ok {
		return fmt.Errorf("crypto: re-encrypt for data key %d: the keyring does not hold it, as the key was made since the keyring was opened; it will once Pisati restarts", target)
	}
	for _, f := range r.fields {
		if err := r.reencrypt(ctx, f); err != nil {
			return err
		}
	}
	return nil
}

// reencrypt re-seals f's values in passes, until a pass re-seals none.
func (r *Reencrypter) reencrypt(ctx context.Context, f registered) error {
	for {
		resealed, unopened, err := r.pass(ctx, f)
		if err != nil {
			return fmt.Errorf("crypto: re-encrypt %s.%s: %w", f.Table, f.Column, err)
		}
		if resealed > 0 {
			continue
		}
		if unopened > 0 {
			return fmt.Errorf("crypto: re-encrypt %s.%s: %d values open under no key in the keyring, so they are left as they are",
				f.Table, f.Column, unopened)
		}
		return nil
	}
}

// pass re-seals the values of f in ID order, a batch at a time, and
// returns how many it re-sealed and how many it could not open.
func (r *Reencrypter) pass(ctx context.Context, f registered) (resealed, unopened int, err error) {
	// None at first, so that the first batch starts at the first row,
	// whatever its ID.
	var after sql.NullInt64
	for {
		b, err := r.batch(ctx, f, after)
		if err != nil {
			return resealed, unopened, err
		}
		resealed += b.resealed
		unopened += b.unopened
		if b.rows < reencryptBatch {
			return resealed, unopened, nil
		}
		after = sql.NullInt64{Int64: b.last, Valid: true}
	}
}

// batchResult is what one batch did: how many rows it read, the last of
// their IDs, and how many it re-sealed and could not open.
type batchResult struct {
	rows, resealed, unopened int
	last                     int64
}

// batch re-seals, in one transaction, the values of f in the next batch
// of rows after the ID after, or from the first when it is NULL, that are
// not sealed under the current key.
func (r *Reencrypter) batch(ctx context.Context, f registered, after sql.NullInt64) (batchResult, error) {
	var res batchResult
	err := inTx(ctx, r.keyring.store.DB(), func(tx *sql.Tx) error {
		id, _ := r.keyring.currentKey()
		header := append([]byte{version}, id.bytes()...)
		rows, err := tx.QueryContext(ctx, f.pending, after, header, reencryptBatch)
		if err != nil {
			return err
		}
		type value struct {
			id     int64
			sealed []byte
		}
		var values []value
		for rows.Next() {
			var v value
			if err := rows.Scan(&v.id, &v.sealed); err != nil {
				return errors.Join(err, rows.Close())
			}
			values = append(values, v)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		for _, v := range values {
			res.rows++
			res.last = v.id
			assoc := f.Assoc(v.id)
			plain, err := r.keyring.Open(v.sealed, assoc)
			if err != nil {
				res.unopened++
				continue
			}
			sealed, err := r.keyring.Seal(plain, assoc)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, f.reseal, sealed, v.id); err != nil {
				return err
			}
			res.resealed++
		}
		return nil
	})
	return res, err
}

// inTx runs fn in a transaction on db, and commits it when fn returns nil;
// otherwise, or when fn panics, it rolls it back. The store's connections
// begin every transaction IMMEDIATE, as Store.InTx says, so this one
// takes the write lock as it begins too. It serves the statements built
// from a Field's names, which the generated queries cannot hold.
func inTx(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) (err error) {
	if db == nil {
		return errors.New("the keyring's store is not open")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	committing := false
	defer func() {
		if committing {
			return
		}
		if rerr := tx.Rollback(); rerr != nil && !errors.Is(rerr, sql.ErrTxDone) {
			err = errors.Join(err, rerr)
		}
	}()
	if err := fn(tx); err != nil {
		return err
	}
	committing = true
	return tx.Commit()
}
