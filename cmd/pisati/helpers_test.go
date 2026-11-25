// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/divbast/pisati/internal/store"
	"github.com/divbast/pisati/internal/store/storedb"
)

// The tests drive Pisati only through run, as its command line does: with
// arguments, an environment, the clock, a random source and two streams.

// stream collects what a command writes to one of its streams. It is safe
// for concurrent use, as serve's goroutines all log to one.
type stream struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *stream) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

// String returns everything written so far.
func (s *stream) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// records returns the JSON log records written so far, failing the test
// on a line that is not one.
func (s *stream) records(t *testing.T) []map[string]any {
	t.Helper()
	var all []map[string]any
	for line := range strings.Lines(s.String()) {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("a log line is not a JSON record: %q: %v", line, err)
		}
		all = append(all, rec)
	}
	return all
}

// logged returns the records with the message msg.
func (s *stream) logged(t *testing.T, msg string) []map[string]any {
	t.Helper()
	var found []map[string]any
	for _, rec := range s.records(t) {
		if rec["msg"] == msg {
			found = append(found, rec)
		}
	}
	return found
}

// result is what one run of a command left: its exit code and streams.
type result struct {
	code           int
	stdout, stderr string
}

// line returns the result's stderr as its one line, failing the test
// unless it is exactly one line.
func (r result) line(t *testing.T) string {
	t.Helper()
	line, ok := strings.CutSuffix(r.stderr, "\n")
	if !ok || strings.Contains(line, "\n") {
		t.Fatalf("stderr is not one line: %q", r.stderr)
	}
	return line
}

// lines returns the result's stderr as its lines, failing the test unless
// it ends with a newline.
func (r result) lines(t *testing.T) []string {
	t.Helper()
	text, ok := strings.CutSuffix(r.stderr, "\n")
	if !ok {
		t.Fatalf("stderr does not end its last line: %q", r.stderr)
	}
	return strings.Split(text, "\n")
}

// site is one installation of Pisati: its environment, with its own state
// and configuration directories, and the free loopback ports its two
// listeners take.
type site struct {
	state            string
	public, internal int
	env              map[string]string
}

// newSite returns a site whose environment sets what every command needs:
// the base URL, both listeners on loopback, the state directory, and an
// empty configuration directory, so that no test reads /etc/pisati.
func newSite(t *testing.T) *site {
	t.Helper()
	s := &site{state: t.TempDir(), public: freePort(t), internal: freePort(t)}
	s.env = map[string]string{
		"CONFIGURATION_DIRECTORY":       t.TempDir(),
		"PISATI_SERVER_BASE_URL":        fmt.Sprintf("http://localhost:%d", s.public),
		"PISATI_SERVER_LISTEN":          fmt.Sprintf("127.0.0.1:%d", s.public),
		"PISATI_SERVER_INTERNAL_LISTEN": fmt.Sprintf("127.0.0.1:%d", s.internal),
		"PISATI_STORAGE_STATE_DIR":      s.state,
	}
	return s
}

// freePort returns a loopback port that was free a moment ago. The
// configuration takes no port 0, so a test picks its own.
func freePort(t *testing.T) int {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := portOf(t, ln)
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

// portOf returns the TCP port ln listens on.
func portOf(t *testing.T, ln net.Listener) int {
	t.Helper()
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("%v is not a TCP address", ln.Addr())
	}
	return addr.Port
}

// host returns the Host a command of s runs with, and its two streams.
func (s *site) host(args ...string) (Host, *stream, *stream) {
	env := maps.Clone(s.env)
	stdout, stderr := &stream{}, &stream{}
	return Host{
		Args: args,
		Lookup: func(name string) (string, bool) {
			v, ok := env[name]
			return v, ok
		},
		EnvNames: slices.Sorted(maps.Keys(env)),
		Clock:    time.Now,
		Rand:     rand.Reader,
		Stdout:   stdout,
		Stderr:   stderr,
	}, stdout, stderr
}

// runWithin bounds a command a test runs to its end: a serve that should
// have refused to start is stopped then, and the test fails on its exit
// code rather than hang.
const runWithin = 30 * time.Second

// run runs a command of s to its end, or for runWithin at most.
func (s *site) run(t *testing.T, args ...string) result {
	t.Helper()
	h, stdout, stderr := s.host(args...)
	ctx, cancel := context.WithTimeout(t.Context(), runWithin)
	defer cancel()
	code := run(ctx, h)
	return result{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

// halting starts a command of s with its own Host.Halt, and returns a
// function that closes it, as a second SIGTERM or SIGINT does, and one
// that waits for the command's result. Its context is the test's: no first
// signal ends it.
func (s *site) halting(t *testing.T, args ...string) (halt func(), wait func() result) {
	t.Helper()
	h, stdout, stderr := s.host(args...)
	halted := make(chan struct{})
	h.Halt = halted
	done := make(chan int, 1)
	go func() { done <- run(t.Context(), h) }()
	return func() { close(halted) }, func() result {
		select {
		case code := <-done:
			return result{code: code, stdout: stdout.String(), stderr: stderr.String()}
		case <-time.After(20 * time.Second):
			t.Fatal("the command did not return within 20 s")
			return result{}
		}
	}
}

// settle waits, for at most 5 s, until no goroutine is left but the
// test's own, failing the test if one still runs then. net/http keeps a
// connection's goroutine 500 ms after its handler returns with the
// request's body unread, as a cut-off request's always is: longer than
// goleak's own retries at the end of the package wait. It changes nothing
// goleak checks; it only waits for that goroutine to end.
func settle(t *testing.T) {
	t.Helper()
	var err error
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if err = goleak.Find(); err == nil {
			return
		}
	}
	t.Fatalf("goroutines still running 5 s after the test's work: %v", err)
}

// mustRun runs a command of s, failing the test unless it exits 0.
func (s *site) mustRun(t *testing.T, args ...string) result {
	t.Helper()
	r := s.run(t, args...)
	if r.code != 0 {
		t.Fatalf("pisati %s exited %d:\n%s", strings.Join(args, " "), r.code, r.stderr)
	}
	return r
}

// serving is a pisati serve that a test started.
type serving struct {
	t      *testing.T
	cancel context.CancelFunc
	// halted is closed by halt, as main closes Host.Halt on a second
	// SIGTERM or SIGINT.
	halted chan struct{}
	// done is closed once run has returned code.
	done           chan struct{}
	code           int
	stdout, stderr *stream
}

// serve starts pisati serve for s, and returns once its internal listener
// answers /healthz with 200. It fails the test if serve exits first. The
// test's end stops it, if the test has not.
func (s *site) serve(t *testing.T) *serving {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	h, stdout, stderr := s.host("serve")
	v := &serving{t: t, cancel: cancel, halted: make(chan struct{}), done: make(chan struct{}), stdout: stdout, stderr: stderr}
	h.Halt = v.halted
	go func() {
		defer close(v.done)
		v.code = run(ctx, h)
	}()
	t.Cleanup(func() { v.stop() })

	healthz := fmt.Sprintf("http://127.0.0.1:%d/healthz", s.internal)
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		select {
		case <-v.done:
			t.Fatalf("pisati serve exited %d before it served:\n%s", v.code, stderr.String())
		default:
		}
		if status, _, err := tryGet(t, healthz); err == nil && status == http.StatusOK {
			return v
		}
	}
	t.Fatalf("pisati serve did not answer %s within 10 s:\n%s", healthz, stderr.String())
	return nil
}

// serveStarting starts pisati serve for s, as serve does, but returns at
// once, without waiting for it to answer: for a test that holds serve in
// its start.
func (s *site) serveStarting(t *testing.T) *serving {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	h, stdout, stderr := s.host("serve")
	v := &serving{t: t, cancel: cancel, halted: make(chan struct{}), done: make(chan struct{}), stdout: stdout, stderr: stderr}
	h.Halt = v.halted
	go func() {
		defer close(v.done)
		v.code = run(ctx, h)
	}()
	t.Cleanup(func() { v.stop() })
	return v
}

// stop cancels serve's context, as SIGTERM does, and returns its exit
// code. Calling it again returns the same.
func (v *serving) stop() int {
	v.cancel()
	return v.wait()
}

// halt closes Host.Halt, as a second SIGTERM or SIGINT does.
func (v *serving) halt() {
	close(v.halted)
}

// wait waits for serve to return, and returns its exit code.
func (v *serving) wait() int {
	select {
	case <-v.done:
		return v.code
	case <-time.After(20 * time.Second):
		v.t.Errorf("pisati serve did not stop within 20 s")
		return -1
	}
}

// holdPorts listens on both of s's ports itself until the test ends, so
// that a serve which opened a listener before it refused to start would
// fail on the port instead, with another message.
func holdPorts(t *testing.T, s *site) {
	t.Helper()
	var lc net.ListenConfig
	for _, port := range []int{s.public, s.internal} {
		ln, err := lc.Listen(t.Context(), "tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := ln.Close(); err != nil {
				t.Error(err)
			}
		})
	}
}

// client is the client the tests send their requests with. It keeps no
// connection open between requests, which goleak would see.
var client = &http.Client{
	Transport: &http.Transport{DisableKeepAlives: true},
	Timeout:   5 * time.Second,
}

// tryGet sends GET url, and returns the status and body, or an error when
// there was no answer.
func tryGet(t *testing.T, url string) (int, string, error) {
	t.Helper()
	return send(t, http.MethodGet, url)
}

// send sends a request of method to url, and returns the status and body,
// or an error when there was no answer.
func send(t *testing.T, method, url string) (int, string, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err := errors.Join(err, resp.Body.Close()); err != nil {
		return 0, "", err
	}
	return resp.StatusCode, string(body), nil
}

// get sends a request of method to url, failing the test when there was
// no answer, and returns the status and body.
func get(t *testing.T, method, url string) (int, string) {
	t.Helper()
	status, body, err := send(t, method, url)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return status, body
}

// withStore opens the database in the state directory state, beside any
// Pisati that has it open, runs f on it, and closes it again, so that a
// Pisati that stops after is the last to close it.
func withStore(t *testing.T, state string, f func(db *store.Store)) {
	t.Helper()
	db, err := store.Open(t.Context(), store.DatabasePath(state))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	f(db)
}

// count returns the one integer query returns from db.
func count(t *testing.T, db *store.Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.DB().QueryRowContext(t.Context(), query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// jobRow returns the state, attempts and last error of the job id.
func jobRow(t *testing.T, db *store.Store, id int64) (string, int64, string) {
	t.Helper()
	var state string
	var attempts int64
	var lastError sql.NullString
	if err := db.DB().QueryRowContext(t.Context(), `SELECT state, attempts, last_error FROM jobs WHERE id = ?`, id).
		Scan(&state, &attempts, &lastError); err != nil {
		t.Fatalf("job %d: %v", id, err)
	}
	return state, attempts, lastError.String
}

// within polls cond every 20 ms until it holds, failing the test with what
// if it does not within 10 s.
func within(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("%s: not within 10 s", what)
}

// writeKeyFile writes a new main key, 32 bytes from crypto/rand in
// standard base64, to a file named name in dir, mode 0600, and returns its
// path. Every key is made as the test runs, so none is in the source.
func writeKeyFile(t *testing.T, dir, name string) string {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(key)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// withUmask sets the process's umask to mask until the test ends.
func withUmask(t *testing.T, mask int) {
	t.Helper()
	old := syscall.Umask(mask)
	t.Cleanup(func() { syscall.Umask(old) })
}

// mustOwnerOnly fails the test unless every file and directory under the
// state directory state, but the directory itself, is for its owner
// alone, and the files Pisati keeps there are all present.
func mustOwnerOnly(t *testing.T, state string, want ...string) {
	t.Helper()
	seen := map[string]bool{}
	if err := filepath.WalkDir(state, func(path string, d os.DirEntry, err error) error {
		if err != nil || path == state {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(state, path)
		if err != nil {
			return err
		}
		seen[rel] = true
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			t.Errorf("%s has mode %04o, want no access for its group or others", rel, perm)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, name := range want {
		if !seen[name] {
			t.Errorf("%s is not in the state directory, which holds %v", name, slices.Sorted(maps.Keys(seen)))
		}
	}
}

// holdWriteLock takes the database's write lock at state, in a
// transaction of its own, and keeps it until the function it returns is
// called, which waits for the transaction to end. A Pisati command that
// writes waits for it, up to SQLite's busy timeout.
func holdWriteLock(t *testing.T, state string) (release func()) {
	t.Helper()
	db, err := store.Open(t.Context(), store.DatabasePath(state))
	if err != nil {
		t.Fatal(err)
	}
	held, unhold, ended := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		ended <- db.InTx(context.WithoutCancel(t.Context()), func(*storedb.Queries) error {
			close(held)
			<-unhold
			return nil
		})
	}()
	<-held
	release = sync.OnceFunc(func() {
		close(unhold)
		if err := errors.Join(<-ended, db.Close()); err != nil {
			t.Error(err)
		}
	})
	// Released at the test's end too, should the test stop before it
	// releases it itself.
	t.Cleanup(release)
	return release
}

// orphanJob leaves a job of kind in the database at state as a run of
// Pisati that stopped mid-job would: claimed, its try counted, and leased
// until long after the test. It returns the job's ID.
func orphanJob(t *testing.T, state, kind string) int64 {
	t.Helper()
	ctx := t.Context()
	var id int64
	withStore(t, state, func(db *store.Store) {
		if err := db.InTx(ctx, func(q *storedb.Queries) error {
			if _, err := q.EnqueueJob(ctx, storedb.EnqueueJobParams{Kind: kind, Payload: []byte{}}); err != nil {
				return err
			}
			now := time.Now()
			j, err := q.ClaimJob(ctx, storedb.ClaimJobParams{
				Now:        now.UnixMilli(),
				LeaseUntil: sql.NullInt64{Int64: now.Add(time.Hour).UnixMilli(), Valid: true},
			})
			id = j.ID
			return err
		}); err != nil {
			t.Fatal(err)
		}
	})
	return id
}

// expiredSession stores a session in the database at state that expired a
// minute ago, and returns its ID's hash.
func expiredSession(t *testing.T, state string) []byte {
	t.Helper()
	id := make([]byte, 32)
	if _, err := rand.Read(id); err != nil {
		t.Fatal(err)
	}
	then := time.Now().Add(-time.Hour).UnixMilli()
	withStore(t, state, func(db *store.Store) {
		if err := db.Queries().InsertSession(t.Context(), storedb.InsertSessionParams{
			IDHash:     id,
			UserID:     sql.NullInt64{Int64: 1, Valid: true},
			CreatedAt:  then,
			LastSeenAt: then,
			ExpiresAt:  time.Now().Add(-time.Minute).UnixMilli(),
		}); err != nil {
			t.Fatal(err)
		}
	})
	return id
}
