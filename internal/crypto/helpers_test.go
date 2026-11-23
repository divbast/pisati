// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package crypto_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/divbast/pisati/internal/config"
	"github.com/divbast/pisati/internal/crypto"
	"github.com/divbast/pisati/internal/store"
)

// newStore returns a store at Pisati's schema, in a new state directory of
// its own, and closes it when the test ends.
func newStore(t testing.TB) *store.Store {
	t.Helper()
	dir := t.TempDir()
	lock, err := store.AcquireLock(t.Context(), dir)
	if err != nil {
		t.Fatalf("AcquireLock(%s): %v", dir, err)
	}
	s, err := store.Open(t.Context(), store.DatabasePath(dir))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	_, _, err = s.Migrate(t.Context(), lock)
	if rerr := lock.Release(); rerr != nil {
		t.Fatal(rerr)
	}
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return s
}

// exec runs each statement on s, outside any transaction.
func exec(t testing.TB, s *store.Store, statements ...string) {
	t.Helper()
	for _, q := range statements {
		if _, err := s.DB().ExecContext(t.Context(), q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

// randomKey returns a new 32-byte key from crypto/rand. Each test makes
// its own, and no failure message prints one.
func randomKey(t testing.TB) []byte {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	return key
}

// keyText returns key as a key file holds it: standard base64, as
// openssl rand -base64 32 writes it, and a newline.
func keyText(key []byte) []byte {
	return append(base64.StdEncoding.AppendEncode(nil, key), '\n')
}

// writeFile writes data to the file name in dir, with exactly mode, and
// returns its path.
func writeFile(t testing.TB, dir, name string, data []byte, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	// WriteFile's mode passes through the umask; Chmod's does not.
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

// discard returns a logger that drops every record.
func discard() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// fromFile loads the master key from the key file at path.
func fromFile(t testing.TB, path string, log *slog.Logger) (crypto.Master, error) {
	t.Helper()
	return crypto.LoadMaster(t.Context(), config.FromFile{Path: path}, nil, nil, log)
}

// newMasterKey returns a master key loaded from a key file of its own,
// mode 0400, and the key that file holds.
func newMasterKey(t testing.TB) (crypto.Master, []byte) {
	t.Helper()
	key := randomKey(t)
	path := writeFile(t, t.TempDir(), "master.key", keyText(key), 0o400)
	m, err := fromFile(t, path, discard())
	if err != nil {
		t.Fatalf("LoadMaster(%s): %v", path, err)
	}
	return m, key
}

// newMaster returns a master key loaded from a key file of its own.
func newMaster(t testing.TB) crypto.Master {
	t.Helper()
	m, _ := newMasterKey(t)
	return m
}

// clock is the time every keyring in the tests reads, but the one in the
// synctest bubble.
func clock() time.Time {
	return time.UnixMilli(1_790_000_000_000)
}

// openKeyring opens the keyring in s with m, drawing from crypto/rand.
func openKeyring(t testing.TB, s *store.Store, m crypto.Master) *crypto.Keyring {
	t.Helper()
	k, err := crypto.OpenKeyring(t.Context(), s, m, clock, rand.Reader)
	if err != nil {
		t.Fatalf("OpenKeyring: %v", err)
	}
	return k
}

// seal seals plain with k under assoc, failing the test on an error.
func seal(t testing.TB, k *crypto.Keyring, plain, assoc string) []byte {
	t.Helper()
	sealed, err := k.Seal([]byte(plain), []byte(assoc))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	return sealed
}

// wantOpens fails the test unless k opens sealed under assoc to plain.
func wantOpens(t testing.TB, k *crypto.Keyring, sealed []byte, assoc, plain string) {
	t.Helper()
	got, err := k.Open(sealed, []byte(assoc))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if string(got) != plain {
		t.Fatalf("Open gave %q, want %q", got, plain)
	}
}

// The sealed format's parts, as the package documents them.
const (
	headerSize = 5
	nonceSize  = 24
	tagSize    = 16
	overhead   = headerSize + nonceSize + tagSize
)

// keyIDOf returns the data key ID a sealed value names.
func keyIDOf(sealed []byte) uint32 {
	return binary.BigEndian.Uint32(sealed[1:headerSize])
}

// withKeyID returns a copy of sealed that names the data key id.
func withKeyID(sealed []byte, id uint32) []byte {
	c := bytes.Clone(sealed)
	binary.BigEndian.PutUint32(c[1:headerSize], id)
	return c
}

// dataKey is one row of the data_keys table.
type dataKey struct {
	id        int64
	wrapped   []byte
	createdAt int64
	state     string
}

// dataKeys returns every row of the data_keys table, in the order of their
// IDs.
func dataKeys(t testing.TB, s *store.Store) []dataKey {
	t.Helper()
	rows, err := s.DB().QueryContext(t.Context(), `SELECT id, wrapped, created_at, state FROM data_keys ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	var all []dataKey
	for rows.Next() {
		var k dataKey
		if err := rows.Scan(&k.id, &k.wrapped, &k.createdAt, &k.state); err != nil {
			t.Fatal(errors.Join(err, rows.Close()))
		}
		all = append(all, k)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	return all
}

// sameKeys reports whether a and b hold the same rows.
func sameKeys(a, b []dataKey) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].id != b[i].id || !bytes.Equal(a[i].wrapped, b[i].wrapped) ||
			a[i].createdAt != b[i].createdAt || a[i].state != b[i].state {
			return false
		}
	}
	return true
}

// job is the part of a row of the jobs table the tests look at.
type job struct {
	kind, key, state string
	payload          []byte
	attempts         int64
}

// jobRows returns every row of the jobs table, in the order they were
// added.
func jobRows(t testing.TB, s *store.Store) []job {
	t.Helper()
	rows, err := s.DB().QueryContext(t.Context(),
		`SELECT kind, coalesce(idempotency_key, ''), state, payload, attempts FROM jobs ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	var all []job
	for rows.Next() {
		var j job
		if err := rows.Scan(&j.kind, &j.key, &j.state, &j.payload, &j.attempts); err != nil {
			t.Fatal(errors.Join(err, rows.Close()))
		}
		all = append(all, j)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	return all
}

// logRecord is one record a recorder kept: its level, message and
// attributes, each value as slog resolves it.
type logRecord struct {
	level slog.Level
	msg   string
	attrs map[string]string
}

// recorder is a slog.Handler that keeps every record, for a test to read.
type recorder struct {
	mu      sync.Mutex
	records []logRecord
}

// newLogger returns a logger that writes to a new recorder.
func newLogger() (*slog.Logger, *recorder) {
	r := &recorder{}
	return slog.New(r), r
}

func (*recorder) Enabled(context.Context, slog.Level) bool { return true }

func (r *recorder) Handle(_ context.Context, rec slog.Record) error {
	attrs := map[string]string{}
	rec.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.Resolve().String()
		return true
	})
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, logRecord{level: rec.Level, msg: rec.Message, attrs: attrs})
	return nil
}

// LoadMaster logs through its logger as given, so these need do nothing
// more than satisfy slog.Handler.
func (r *recorder) WithAttrs([]slog.Attr) slog.Handler { return r }
func (r *recorder) WithGroup(string) slog.Handler      { return r }

// all returns every record kept.
func (r *recorder) all() []logRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]logRecord(nil), r.records...)
}

// mentions reports whether any record's message or attribute value
// contains text.
func (r *recorder) mentions(text string) bool {
	for _, rec := range r.all() {
		if strings.Contains(rec.msg, text) {
			return true
		}
		for _, v := range rec.attrs {
			if strings.Contains(v, text) {
				return true
			}
		}
	}
	return false
}

// constReader is a random source every byte of which is the same: two
// data keys drawn from it are the same key, and two nonces the same
// nonce.
type constReader byte

func (c constReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(c)
	}
	return len(p), nil
}

// failingReader is a random source that cannot be read.
type failingReader struct{}

func (failingReader) Read([]byte) (int, error) {
	return 0, errors.New("no randomness to be had")
}

// The random sources the tests pass satisfy the interface OpenKeyring
// takes.
var (
	_ io.Reader = constReader(0)
	_ io.Reader = failingReader{}
)
