// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

// Package store keeps Pisati's state in one SQLite database,
// <state>/pisati.db. It opens the database with the settings every
// connection needs, migrates its schema with the goose migrations built
// into Pisati, runs write transactions, and backs the database up and
// restores it. The lock that keeps one Pisati to a state directory lives
// here too, since migrating and restoring are safe only while it is held.
//
// Every path the package is given must be absolute: it never resolves one
// against the working directory.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"

	"github.com/ncruces/go-sqlite3/driver"

	"github.com/divbast/pisati/internal/store/storedb"
)

// databaseFile is the name of Pisati's database in its state directory.
const databaseFile = "pisati.db"

// DatabasePath returns where Pisati keeps its database in the state
// directory stateDir: stateDir/pisati.db. It is the file to Open, and the
// one Migrate and Restore act on under the lock AcquireLock takes on
// stateDir.
func DatabasePath(stateDir string) string {
	return filepath.Join(stateDir, databaseFile)
}

const (
	// busyTimeoutMillis is how long a connection waits for a lock another
	// holds before it fails with SQLITE_BUSY.
	busyTimeoutMillis = 5000

	// maxConns bounds the connections the pool opens. Unbounded,
	// database/sql would open one for every caller at once, however many
	// requests arrive, and each runs its own instance of SQLite, whose
	// memory grows with use. Past a point more connections add nothing:
	// SQLite runs one writer at a time, and WAL lets the readers share
	// the file.
	maxConns = 64
)

// Store is Pisati's database: a pool of connections to one SQLite file,
// each set up as Open describes. Its methods are safe for concurrent use.
// The zero Store holds no database: InTx, Migrate and Backup return an
// error, and DB and Queries return nil.
type Store struct {
	db         *sql.DB
	queries    *storedb.Queries
	migrations fs.FS
	path       string
}

// Option changes how a Store that Open returns behaves.
type Option func(*options)

type options struct {
	migrations fs.FS
}

// WithMigrations has Migrate apply the goose migrations at the root of
// fsys in place of the ones Pisati is built with. It serves a tool or test
// that needs a schema of its own; Pisati itself never passes it.
func WithMigrations(fsys fs.FS) Option {
	return func(o *options) {
		o.migrations = fsys
	}
}

// Open opens the SQLite database at path, creating it when it does not
// exist, and returns it once a first connection has opened. On every
// connection it opens:
//   - busy_timeout is 5 s, set first so that the settings after it wait
//     out a lock rather than fail on one;
//   - the journal is a write-ahead log (WAL), so that readers and the one
//     writer do not block each other;
//   - foreign keys are enforced, which SQLite leaves off by default;
//   - synchronous is NORMAL: in WAL mode a commit survives the process
//     dying, and the database is never corrupted, though a power cut may
//     lose the last commits;
//   - write transactions begin IMMEDIATE, as InTx describes.
//
// path must be absolute. The error names path when the database cannot be
// opened.
func Open(ctx context.Context, path string, opts ...Option) (*Store, error) {
	if err := absolute(path); err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	ours, err := pisatiMigrations()
	if err != nil {
		return nil, err
	}
	o := options{migrations: ours}
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}

	db, err := driver.Open(dataSourceName(path))
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	// Idle connections stay at database/sql's default of two: each one
	// kept keeps an instance of SQLite, with its cache.
	db.SetMaxOpenConns(maxConns)
	// sql.Open connects to nothing; a connection opens the file.
	if err := db.PingContext(ctx); err != nil {
		return nil, errors.Join(fmt.Errorf("store: open %s: %w", path, err), db.Close())
	}
	return &Store{
		db:         db,
		queries:    storedb.New(db),
		migrations: o.migrations,
		path:       path,
	}, nil
}

// dataSourceName returns the URI that has the driver open path with the
// settings Open describes. They go in the URI so that the driver applies
// them to every connection the pool opens, not to the first alone.
func dataSourceName(path string) string {
	q := url.Values{}
	for _, pragma := range []string{
		fmt.Sprintf("busy_timeout(%d)", busyTimeoutMillis),
		"journal_mode(wal)",
		"foreign_keys(1)",
		"synchronous(normal)",
	} {
		q.Add("_pragma", pragma)
	}
	// A DEFERRED transaction that reads and then writes must turn its read
	// into a write. If another connection has committed since the read
	// began, SQLite refuses with SQLITE_BUSY at once and calls no busy
	// handler, since waiting could never help: busy_timeout cannot save it.
	// IMMEDIATE takes the write lock at BEGIN, so that concurrent writers
	// queue on busy_timeout instead. It is the driver's default, set here
	// so that a change of default cannot undo it.
	q.Set("_txlock", "immediate")
	u := url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}
	return u.String()
}

// Close stops new work: once it is called, every call through the Store
// that needs a connection, whether a query, a transaction, InTx, Migrate
// or Backup, fails with an error. Work already running, a query or a
// transaction such as an InTx in progress, keeps its connection and runs
// to its end, and the connection then closes. database/sql speaks of
// Close waiting for the queries a server has begun; SQLite has no server,
// as its queries run in this process, and Close may return while they
// still run. So the caller stops every user of the Store first, the
// servers and the workers, and lets their calls return.
//
// Closing the last connection checkpoints the write-ahead log into the
// database file and removes the log. With every user stopped, Close does
// that before it returns, a wait bounded by the size of the log, which
// SQLite keeps to about 1000 pages (4 MiB), and by no context. Closing a
// Store again, or the zero Store, does nothing.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("store: close %s: %w", s.path, err)
	}
	return nil
}

// DB returns the pool of connections, for what the generated queries do
// not cover. It is nil for the zero Store.
func (s *Store) DB() *sql.DB {
	if s == nil {
		return nil
	}
	return s.db
}

// Queries returns the generated queries, each run outside any transaction
// on a connection from the pool. It is nil for the zero Store.
func (s *Store) Queries() *storedb.Queries {
	if s == nil {
		return nil
	}
	return s.queries
}

// InTx runs fn in one write transaction, which it commits when fn returns
// nil. When fn returns an error, or panics, InTx rolls the transaction
// back, and returns fn's error joined with any error from the rollback, or
// lets the panic go on. fn does its work through the Queries it is given,
// which run in the transaction.
//
// The transaction takes the database's write lock as it begins, so
// concurrent calls queue for the lock for up to the busy timeout, 5 s,
// rather than fail with SQLITE_BUSY when fn reads before it writes.
func (s *Store) InTx(ctx context.Context, fn func(*storedb.Queries) error) (err error) {
	if err := s.open(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin a transaction on %s: %w", s.path, err)
	}
	// Rolled back on every way out but the commit, a panic in fn included:
	// otherwise the connection would never return to the pool, and would
	// hold the write lock until the process ended.
	committing := false
	defer func() {
		if !committing {
			err = errors.Join(err, s.rollback(tx))
		}
	}()
	if err := fn(s.queries.WithTx(tx)); err != nil {
		return err
	}
	committing = true
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit to %s: %w", s.path, err)
	}
	return nil
}

// rollback rolls tx back. A transaction whose context has ended is one
// database/sql has rolled back already, so that is no error.
func (s *Store) rollback(tx *sql.Tx) error {
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		return fmt.Errorf("store: roll back on %s: %w", s.path, err)
	}
	return nil
}

// open returns an error for a Store that Open did not return.
func (s *Store) open() error {
	if s == nil || s.db == nil {
		return errors.New("store: the Store was not opened with Open")
	}
	return nil
}

// absolute returns an error unless path is absolute.
func absolute(path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("not an absolute path")
	}
	return nil
}

// splitPath returns the directory and the name of the file at path, which
// must be absolute and name a file below the root.
func splitPath(path string) (dir, name string, err error) {
	if err := absolute(path); err != nil {
		return "", "", err
	}
	dir, name = filepath.Split(filepath.Clean(path))
	if name == "" {
		return "", "", errors.New("it names no file")
	}
	return dir, name, nil
}

// removeFiles removes each file names, in root, that exists.
func removeFiles(root *os.Root, names ...string) error {
	var errs []error
	for _, name := range names {
		if err := root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// sidecars returns the files SQLite may keep beside the database name: its
// write-ahead log, the log's shared-memory index, and a rollback journal.
func sidecars(name string) []string {
	return []string{name + "-wal", name + "-shm", name + "-journal"}
}

// createFile creates the empty file name in root, mode 0600, refusing one
// that exists with an error wrapping fs.ErrExist.
func createFile(root *os.Root, name string) error {
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	return f.Close()
}

// syncFile flushes the file name in root to stable storage.
func syncFile(root *os.Root, name string) (err error) {
	f, err := root.OpenFile(name, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, f.Close())
	}()
	return f.Sync()
}

// syncDir flushes root's directory, and so the files created, renamed and
// removed in it, to stable storage.
func syncDir(root *os.Root) (err error) {
	d, err := root.Open(".")
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, d.Close())
	}()
	return d.Sync()
}
