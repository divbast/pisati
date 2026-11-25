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
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ncruces/go-sqlite3"
	"github.com/ncruces/go-sqlite3/driver"

	"github.com/divbast/pisati/internal/config"
	"github.com/divbast/pisati/internal/explain"
	"github.com/divbast/pisati/internal/store/storedb"
)

// databaseFile is the name of Pisati's database in its state directory.
const databaseFile = "pisati.db"

// DatabasePath returns where Pisati keeps its database in the state
// directory stateDir: stateDir/pisati.db. It is the file to Open, and the
// one Migrate and Restore act on under the lock AcquireLock takes on
// stateDir.
func DatabasePath(stateDir config.StateDir) string {
	return filepath.Join(stateDir.String(), databaseFile)
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
	// dir is the directory that holds the database, open, dirPath its
	// path, and name the database's file in it: Check looks there for the
	// file at its path, which the connections, holding it open, cannot.
	dir     *os.Root
	dirPath string
	name    string
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
	if err := absolute(path, "store.Open"); err != nil {
		return nil, explain.Wrap(err, "Pisati cannot open the database %s", path)
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
		return nil, cannotOpen(ctx, err, path)
	}
	// Idle connections stay at database/sql's default of two: each one
	// kept keeps an instance of SQLite, with its cache.
	db.SetMaxOpenConns(maxConns)
	// sql.Open connects to nothing; a connection opens the file.
	if err := db.PingContext(ctx); err != nil {
		return nil, errors.Join(cannotOpen(ctx, err, path), db.Close())
	}
	dirPath, name := filepath.Split(path)
	dir, err := os.OpenRoot(dirPath)
	if err != nil {
		return nil, errors.Join(explain.New(err, "Pisati cannot open the directory %s, which holds the database %s: %s",
			filepath.Clean(dirPath), path, pathless(err)), db.Close())
	}
	return &Store{
		db:         db,
		queries:    storedb.New(db),
		migrations: o.migrations,
		path:       path,
		dir:        dir,
		dirPath:    filepath.Clean(dirPath),
		name:       name,
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
		return errors.Join(explain.Wrap(err, "Pisati cannot close the database %s", s.path), s.dir.Close())
	}
	// The directory, opened read-only, holds nothing to flush.
	return s.dir.Close()
}

// DB returns the pool of connections, for what the generated queries do
// not cover. It is nil for the zero Store.
func (s *Store) DB() *sql.DB {
	if s == nil {
		return nil
	}
	return s.db
}

// Check checks, within the time within gives it, that the database is
// there and reads, as a health check needs. The connections hold the
// database's file open, and read it as it is in their cache, or in the
// write-ahead log: a file moved, removed, replaced or overwritten from
// outside SQLite would go unseen through them. So Check, in turn:
//   - checks that the directory the Store opened, which holds the
//     database, is still the one at that directory's path;
//   - checks that the database's file is there in it, as a regular file,
//     not a symbolic link;
//   - reads the file's first 100 bytes, its header, from the file itself,
//     and checks that they start as every SQLite database's do;
//   - empties its connection's cache of pages, and counts the tables and
//     indexes in the schema, sqlite_schema, which SQLite reads from the
//     database's first pages, in the file or in its write-ahead log.
//
// It returns nil once all pass, and otherwise an error that names the
// directory or the database and says what is wrong and what to do; once
// ctx has ended, or within has passed, an error wrapping ctx's. A within
// that is not positive is a bug, and gets an error, as does the zero
// Store.
func (s *Store) Check(ctx context.Context, within time.Duration) (err error) {
	if err := s.open(); err != nil {
		return err
	}
	if within <= 0 {
		return fmt.Errorf("store.Store.Check was given %s to check the database %s in, which is a bug in Pisati: it "+
			"needs a time above zero", within, s.path)
	}
	ctx, cancel := context.WithTimeout(ctx, within)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return cannotRead(ctx, err, s.path, within)
	}
	if err := s.sameDirectory(ctx); err != nil {
		return err
	}
	if err := s.atItsPath(ctx); err != nil {
		return err
	}
	if err := s.header(ctx); err != nil {
		return err
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return cannotRead(ctx, err, s.path, within)
	}
	defer func() {
		err = errors.Join(err, conn.Close())
	}()
	// sqlc cannot parse a PRAGMA, so both statements are written here.
	if _, err := conn.ExecContext(ctx, "PRAGMA shrink_memory"); err != nil {
		return cannotRead(ctx, err, s.path, within)
	}
	var entries int64
	if err := conn.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema").Scan(&entries); err != nil {
		return cannotRead(ctx, err, s.path, within)
	}
	return nil
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
		return cannotChange(ctx, err, "Pisati cannot start a change to the database %s", s.path)
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
		return cannotChange(ctx, err, "Pisati cannot save a change to the database %s", s.path)
	}
	return nil
}

// rollback rolls tx back. A transaction whose context has ended is one
// database/sql has rolled back already, so that is no error.
func (s *Store) rollback(tx *sql.Tx) error {
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		return explain.Wrap(err, "Pisati cannot undo a change to the database %s that did not finish", s.path)
	}
	return nil
}

// open returns an error for a Store that Open did not return.
func (s *Store) open() error {
	if s == nil || s.db == nil {
		return errors.New("a store.Store was used that store.Open did not return, which is a bug in Pisati")
	}
	return nil
}

// absolute returns an error unless path, which the function fn was
// given, is absolute: only a bug in Pisati gives the store another.
func absolute(path, fn string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("%s was given the path %q, which is not a full path, which is a bug in Pisati: Pisati "+
			"gives the store full paths only", fn, path)
	}
	return nil
}

// splitPath returns the directory and the name of the file at path, which
// the function fn was given, and which must be absolute and name a file
// below the root.
func splitPath(path, fn string) (dir, name string, err error) {
	if err := absolute(path, fn); err != nil {
		return "", "", err
	}
	dir, name = filepath.Split(filepath.Clean(path))
	if name == "" {
		return "", "", fmt.Errorf("%s names no file: give the path of a file", path)
	}
	return dir, name, nil
}

// removeFiles removes each file names, in root, that exists. Each error
// names the file, by its path, and says why it is still there. Once ctx
// has ended, it removes nothing, and returns ctx's error: a removal that
// must run whatever stopped its caller is given a context that never ends.
func removeFiles(ctx context.Context, root *os.Root, names ...string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var errs []error
	for _, name := range names {
		if err := root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, explain.New(err, "Pisati cannot remove the file %s: %s", filepath.Join(root.Name(), name),
				pathless(err)))
		}
	}
	return errors.Join(errs...)
}

// stoppedBy returns ctx's error, in the words format and args give, when
// ctx has ended and err is what that leaves of a call into SQLite: ctx's
// own error; SQLITE_BUSY, "database is locked", which a wait for a lock
// another connection holds returns once ctx cuts the wait short; or
// SQLITE_INTERRUPT, which a statement that ctx stopped returns. The caller
// was stopped, and nothing failed, so the error wraps ctx's error alone:
// errors.Is finds context.Canceled or context.DeadlineExceeded through it,
// and no code of SQLite's. Otherwise stoppedBy returns nil.
func stoppedBy(ctx context.Context, err error, format string, args ...any) error {
	cause := ctx.Err()
	if cause == nil || err == nil {
		return nil
	}
	if !errors.Is(err, cause) && !errors.Is(err, sqlite3.BUSY) && !errors.Is(err, sqlite3.INTERRUPT) {
		return nil
	}
	return explain.New(cause, format, args...)
}

// cannotOpen is the error for the database at path, which err says SQLite
// cannot open: SQLite's reason, and what to do when the reason says, or,
// once ctx has ended, that Pisati stopped (stoppedBy).
func cannotOpen(ctx context.Context, err error, path string) error {
	if stop := stoppedBy(ctx, err, "Pisati stopped before it had opened the database %s", path); stop != nil {
		return stop
	}
	switch {
	case errors.Is(err, sqlite3.NOTADB), errors.Is(err, sqlite3.CORRUPT):
		return explain.New(err, "Pisati cannot open the database %s: %s. It is not a SQLite database, or it is "+
			"damaged: restore a backup of it with pisati restore", path, err)
	case errors.Is(err, sqlite3.CANTOPEN):
		return explain.New(err, "Pisati cannot open the database %s: %s. Check that the user that runs Pisati "+
			"can read and write the file, and the directory that holds it", path, err)
	case errors.Is(err, sqlite3.BUSY):
		return explain.New(err, "Pisati cannot open the database %s: %s. Another program that has the database "+
			"open, such as pisati serve or the sqlite3 tool, kept it locked for more than %d seconds: run the command "+
			"again once that program is done with it", path, err, busyTimeoutMillis/1000)
	}
	return explain.New(err, "Pisati cannot open the database %s: %s. Check that the user that runs Pisati can read "+
		"and write the file and the directory that holds it, and that the file is a database that Pisati made", path, err)
}

// stopAndRestore is what an error that finds the database lost says to
// do.
const stopAndRestore = "stop Pisati, then restore your last backup with pisati restore"

// sameDirectory returns an error unless the directory the Store opened,
// which holds the database, is still the one at that directory's path.
// Once ctx has ended, it checks nothing, and returns ctx's error.
func (s *Store) sameDirectory(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	opened, err := s.dir.Stat(".")
	if err != nil {
		return explain.New(err, "Pisati cannot check the directory %s, which holds the database, through the handle it "+
			"has open on it: %s. That points to a fault of the disk or of the file system that holds it: the system "+
			"log may say more", s.dirPath, pathless(err))
	}
	now, err := os.Stat(s.dirPath)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return explain.New(err, "the directory %s, which holds the database, is gone: it was moved or removed while "+
			"Pisati ran. Pisati still reads and writes the database in the directory it has open, but would not find "+
			"it when it next starts. If the directory was moved, stop Pisati and move it back to %s. If it was "+
			"removed, the database is lost once Pisati stops: %s", s.dirPath, s.dirPath, stopAndRestore)
	case errors.Is(err, fs.ErrPermission):
		return explain.New(err, "Pisati cannot check the directory %s, which holds the database, because the user "+
			"that runs Pisati no longer has permission to reach it. Give that user back permission to enter %s and "+
			"every directory above it", s.dirPath, s.dirPath)
	case err != nil:
		return explain.New(err, "Pisati cannot check the directory %s, which holds the database: %s", s.dirPath,
			pathless(err))
	case !os.SameFile(opened, now):
		return fmt.Errorf("the directory %s is not the directory that held the database when Pisati opened it: the "+
			"directory Pisati has open was moved, and another put at its path, while Pisati ran. Stop Pisati, then "+
			"move the directory that holds the database back to %s", s.dirPath, s.dirPath)
	}
	return nil
}

// atItsPath returns an error unless the database's file is at its path, in
// the directory the Store opened, as a regular file: what Check finds
// otherwise, and what to do about it. Once ctx has ended, it checks
// nothing, and returns ctx's error.
func (s *Store) atItsPath(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := s.dir.Lstat(s.name)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return explain.New(err, "Pisati cannot find its database file %s: it was moved or removed while Pisati ran. "+
			"Pisati still reads and writes the file it has open, but would not find it when it next starts. If the "+
			"file was moved, stop Pisati and move it back to %s. If it was removed, it is lost once Pisati stops: %s",
			s.path, s.path, stopAndRestore)
	case errors.Is(err, fs.ErrPermission):
		return explain.New(err, "Pisati cannot check its database file %s, because the user that runs Pisati no longer "+
			"has permission to look in the directory that holds it. Give that user back permission to read and enter "+
			"%s", s.path, s.dirPath)
	case errors.Is(err, syscall.EIO):
		return explain.New(err, "Pisati cannot check its database file %s, because the disk or the file system that "+
			"holds it reported a fault: the system log may say more", s.path)
	case err != nil:
		return explain.New(err, "Pisati cannot check its database file %s: %s", s.path, pathless(err))
	case !info.Mode().IsRegular():
		return fmt.Errorf("the database file %s is not a regular file any more: %s is there in its place, put there "+
			"while Pisati ran. Stop Pisati, move what is there away, and move the database file back to %s, or "+
			"restore your last backup with pisati restore", s.path, kindOf(info.Mode()), s.path)
	}
	return nil
}

// sqliteMagic starts every SQLite database file: the first 16 of the 100
// bytes of its header.
const sqliteMagic = "SQLite format 3\x00"

// header returns an error unless the database's file, read through the
// directory the Store opened, starts with a whole header, 100 bytes, that
// starts as every SQLite database's does. The driver locks the file with
// locks of its own open file, which a descriptor opened and closed here
// leaves alone. Once ctx has ended, it reads nothing, and returns ctx's
// error.
func (s *Store) header(ctx context.Context) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	f, err := s.dir.Open(s.name)
	if err != nil {
		return explain.New(err, "Pisati cannot open its database file %s to check it: %s", s.path, pathless(err))
	}
	defer func() {
		err = errors.Join(err, f.Close())
	}()
	var head [100]byte
	switch _, err := io.ReadFull(f, head[:]); {
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return fmt.Errorf("the database file %s is shorter than the 100 bytes that start every SQLite database: it "+
			"was cut short while Pisati ran. %s", s.path, upper(stopAndRestore))
	case err != nil:
		return explain.New(err, "Pisati cannot read its database file %s to check it: %s", s.path, pathless(err))
	case string(head[:len(sqliteMagic)]) != sqliteMagic:
		return fmt.Errorf("the database file %s does not start as a SQLite database does: something wrote over it "+
			"while Pisati ran. %s", s.path, upper(stopAndRestore))
	}
	return nil
}

// upper returns s with its first letter in capitals, to start a sentence.
func upper(s string) string {
	return strings.ToUpper(s[:1]) + s[1:]
}

// kindOf names what a file of mode m is, other than a regular file.
func kindOf(m fs.FileMode) string {
	switch {
	case m.IsDir():
		return "a directory"
	case m&fs.ModeSymlink != 0:
		return "a symbolic link"
	}
	return "a file that is not a regular file"
}

// cannotRead is the error for the database at path, which err says Check
// cannot read: SQLite's reason, and what to do when the reason says. Once
// ctx has ended, it is ctx's error: the read ran out of the time within
// gave it, or was stopped.
func cannotRead(ctx context.Context, err error, path string, within time.Duration) error {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return explain.New(ctx.Err(), "Pisati did not finish reading the database %s within %s, the time the check "+
			"allows", path, within)
	case ctx.Err() != nil:
		return explain.New(ctx.Err(), "Pisati stopped reading the database %s before it had finished", path)
	case errors.Is(err, sqlite3.NOTADB), errors.Is(err, sqlite3.CORRUPT):
		return explain.New(err, "Pisati cannot read the database %s: %s. It is not a SQLite database any more, or it is "+
			"damaged: %s", path, err, stopAndRestore)
	case errors.Is(err, sqlite3.IOERR):
		return explain.New(err, "Pisati cannot read the database %s: %s. The disk or the file system that holds it "+
			"reported a fault: the system log may say more", path, err)
	}
	return explain.Wrap(err, "Pisati cannot read the database %s", path)
}

// cannotChange is the error, worded as format and args say, for a change
// to the database that err says SQLite refused: SQLite's reason, and what
// to do when the reason says, or, once ctx has ended, that Pisati stopped
// (stoppedBy).
func cannotChange(ctx context.Context, err error, format string, args ...any) error {
	words := fmt.Sprintf(format, args...)
	if stop := stoppedBy(ctx, err, "%s, because Pisati stopped first", words); stop != nil {
		return stop
	}
	switch {
	case errors.Is(err, sqlite3.FULL):
		return explain.New(err, "%s: %s. The disk that holds it is full: free some space on it", words, err)
	case errors.Is(err, sqlite3.READONLY):
		return explain.New(err, "%s: %s. Pisati may only read it: let the user that runs Pisati write to the "+
			"file and to the directory that holds it", words, err)
	case errors.Is(err, sqlite3.BUSY):
		return explain.New(err, "%s: %s. Another change kept the database locked for more than %d seconds", words,
			err, busyTimeoutMillis/1000)
	}
	return explain.Wrap(err, "%s", words)
}

// pathless returns the error inside err when err is an *fs.PathError,
// whose path the message around it names already, or err.
func pathless(err error) error {
	if pe, ok := errors.AsType[*fs.PathError](err); ok {
		return pe.Err
	}
	return err
}

// sidecars returns the files SQLite may keep beside the database name: its
// write-ahead log, the log's shared-memory index, and a rollback journal.
func sidecars(name string) []string {
	return []string{name + "-wal", name + "-shm", name + "-journal"}
}

// createFile creates the empty file name in root, mode 0600, refusing one
// that exists with an error wrapping fs.ErrExist. Once ctx has ended, it
// creates nothing, and returns ctx's error.
func createFile(ctx context.Context, root *os.Root, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	return f.Close()
}

// syncFile flushes the file name in root to stable storage. Once ctx has
// ended, it flushes nothing, and returns ctx's error.
func syncFile(ctx context.Context, root *os.Root, name string) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
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
// removed in it, to stable storage. Once ctx has ended, it flushes
// nothing, and returns ctx's error.
func syncDir(ctx context.Context, root *os.Root) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	d, err := root.Open(".")
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, d.Close())
	}()
	return d.Sync()
}
