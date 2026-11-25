// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/divbast/pisati/internal/jobs"
	"github.com/divbast/pisati/internal/store"
)

func TestServeAndShutdown(t *testing.T) {
	// A umask that would leave the database readable by everyone: serve
	// must make every file it creates its own user's alone all the same.
	withUmask(t, 0o022)

	// A collector that counts the traces serve sends it, which serve
	// holds in a batch until it flushes them as it stops.
	var exports atomic.Int32
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/traces" {
			exports.Add(1)
		}
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Errorf("read the export: %v", err)
		}
	}))
	t.Cleanup(collector.Close)

	s := newSite(t)
	s.env["PISATI_TELEMETRY_OTLP_ENDPOINT"] = collector.URL
	s.mustRun(t, "migrate")
	session := expiredSession(t, s.state)
	orphan := orphanJob(t, s.state, "test.orphan")

	// The commands before set the umask too: set it back, so that what
	// serve creates is under the umask it was started with.
	withUmask(t, 0o022)
	v := s.serve(t)
	public := fmt.Sprintf("http://127.0.0.1:%d", s.public)
	internal := fmt.Sprintf("http://127.0.0.1:%d", s.internal)
	for _, tc := range []struct {
		method, url string
		status      int
		body        string
		anyBody     bool
	}{
		{http.MethodGet, internal + "/healthz", http.StatusOK, "ok", false},
		{http.MethodHead, internal + "/healthz", http.StatusOK, "", false},
		{http.MethodGet, internal + "/metrics", http.StatusOK, "", true},
		{http.MethodGet, internal + "/", http.StatusNotFound, "", false},
		{http.MethodGet, internal + "/login", http.StatusNotFound, "", false},
		{http.MethodPost, internal + "/healthz", http.StatusNotFound, "", false},
		{http.MethodGet, internal + "/debug/pprof/", http.StatusNotFound, "", false},
		{http.MethodGet, public + "/healthz", http.StatusNotFound, "", true},
		{http.MethodGet, public + "/login", http.StatusOK, "", true},
	} {
		status, body := get(t, tc.method, tc.url)
		if status != tc.status || (!tc.anyBody && body != tc.body) {
			t.Errorf("%s %s = %d %q, want %d %q", tc.method, tc.url, status, body, tc.status, tc.body)
		}
	}

	// The sweep at start deletes the session that had expired, and the
	// job the stopped run left leased is released at once: claimed again,
	// its cut-off try counted, and dead-lettered, as nothing handles its
	// kind.
	within(t, "the expired session is deleted", func() bool {
		n := 0
		withStore(t, s.state, func(db *store.Store) {
			n = count(t, db, `SELECT count(*) FROM sessions WHERE id_hash = ?`, session)
		})
		return n == 0
	})
	within(t, "the orphaned job is claimed again", func() bool {
		var state string
		withStore(t, s.state, func(db *store.Store) {
			state, _, _ = jobRow(t, db, orphan)
		})
		return state == "dead"
	})
	withStore(t, s.state, func(db *store.Store) {
		if state, attempts, lastError := jobRow(t, db, orphan); state != "dead" || attempts != 2 ||
			lastError != "no handler for job kind test.orphan" {
			t.Errorf("the orphaned job is %s after %d tries (%q), want dead after 2, for its kind", state, attempts, lastError)
		}
	})
	mustOwnerOnly(t, s.state, "pisati.db", "pisati.db-wal", "pisati.db-shm", "pisati.lock", "keys", filepath.Join("keys", "main.key"))
	if n := len(v.stdout.logged(t, "serving started")); n != 1 {
		t.Errorf("serve logged %d records that it started serving, want 1:\n%s", n, v.stdout.String())
	}

	stopping := time.Now()
	if code := v.stop(); code != 0 {
		t.Errorf("serve exited %d once stopped, want 0:\n%s", code, v.stderr.String())
	}
	if took := time.Since(stopping); took >= 9*time.Second {
		t.Errorf("serve took %v to stop, want under 9 s", took)
	}
	if v.stderr.String() != "" {
		t.Errorf("serve wrote to stderr: %q", v.stderr.String())
	}
	records := v.stdout.records(t)
	if last := records[len(records)-1]["msg"]; last != "stopped" {
		t.Errorf("serve's last record is %q, want %q", last, "stopped")
	}

	// Once serve has returned, the store is closed, which removes the
	// write-ahead log, the lock is free, and the traces are sent.
	if _, err := os.Lstat(filepath.Join(s.state, "pisati.db-wal")); !os.IsNotExist(err) {
		t.Errorf("pisati.db-wal is still there once serve stopped (%v): the store was not closed", err)
	}
	lock, err := store.AcquireLock(t.Context(), s.state)
	if err != nil {
		t.Errorf("the state directory's lock is still held once serve stopped: %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Error(err)
	}
	if exports.Load() == 0 {
		t.Error("serve sent no traces to the collector before it stopped")
	}
	mustOwnerOnly(t, s.state, "pisati.db", "pisati.lock", filepath.Join("keys", "main.key"))
}

// stalledRequest opens a connection to s's public listener and sends a
// request whose body never comes in full, so that its handler keeps
// reading it. It returns the connection, which the test closes before it
// settles.
func stalledRequest(t *testing.T, s *site) net.Conn {
	t.Helper()
	var d net.Dialer
	conn, err := d.DialContext(t.Context(), "tcp", fmt.Sprintf("127.0.0.1:%d", s.public))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(conn, "POST /_/theme HTTP/1.1\r\nHost: localhost:%d\r\n"+
		"Content-Type: application/x-www-form-urlencoded\r\nContent-Length: 100\r\n\r\ntheme=", s.public); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	return conn
}

// closeAndSettle closes conn, then waits for every goroutine of the run to
// end (settle).
func closeAndSettle(t *testing.T, conn net.Conn) {
	t.Helper()
	if err := conn.Close(); err != nil {
		t.Error(err)
	}
	settle(t)
}

// haltRecord is the one record a halt logs.
const haltRecord = "stop cut short: a second SIGTERM or SIGINT signal arrived, so Pisati stops at once, " +
	"without waiting for the work it was doing to finish"

// mustHaveHalted fails the test unless serve, run as v, ended as a halt
// ends it: exit 1, took after the halt, which is at once; the halt's one
// record, last, after "stopping" when a first signal had begun the stop,
// and with no "stopping" when none had; nothing on stderr; and the state
// directory's lock free.
func mustHaveHalted(t *testing.T, s *site, v *serving, code int, took time.Duration, stoppedFirst bool) {
	t.Helper()
	if code != 1 {
		t.Errorf("serve stopped by a second signal exited %d, want 1", code)
	}
	if took >= time.Second {
		t.Errorf("serve took %v to stop after the second signal, want it at once", took)
	}
	if v.stderr.String() != "" {
		t.Errorf("serve wrote to stderr: %q; the log holds why it stopped", v.stderr.String())
	}
	var msgs []string
	for _, rec := range v.stdout.records(t) {
		msgs = append(msgs, fmt.Sprint(rec["msg"]))
	}
	stopping := slices.Index(msgs, "stopping")
	if n := len(msgs); n == 0 || msgs[n-1] != haltRecord {
		t.Errorf("serve logged %q; want the halt's record last", msgs)
	}
	switch {
	case stoppedFirst && (stopping < 0 || stopping > len(msgs)-2):
		t.Errorf("serve logged %q; want \"stopping\", from the first signal, before the halt's record", msgs)
	case !stoppedFirst && stopping >= 0:
		t.Errorf("serve logged %q; want no \"stopping\": the halt logs only its own record", msgs)
	}
	if cut := v.stdout.logged(t, haltRecord); len(cut) != 1 || cut[0]["level"] != "ERROR" {
		t.Errorf("serve logged %v, want one error record of the halt", cut)
	}
	lock, err := store.AcquireLock(t.Context(), s.state)
	if err != nil {
		t.Errorf("the state directory's lock is still held once serve stopped: %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Error(err)
	}
}

// mustLogNothingAfterHalt fails the test unless the halt's record is still
// serve's last, once everything serve left running has ended: the log
// takes no record after the halt's.
func mustLogNothingAfterHalt(t *testing.T, v *serving) {
	t.Helper()
	records := v.stdout.records(t)
	if last := records[len(records)-1]["msg"]; last != haltRecord {
		t.Errorf("serve logged %q after the halt's record; want nothing after it", last)
	}
}

func TestServeDrainsWithinDeadline(t *testing.T) {
	s := newSite(t)
	v := s.serve(t)

	// A request whose body never comes in full keeps its handler reading
	// past the stop deadline.
	conn := stalledRequest(t, s)
	stopping := time.Now()
	code := v.stop()
	took := time.Since(stopping)
	if code != 0 {
		t.Errorf("serve exited %d when the deadline cut a request off, want 0:\n%s", code, v.stderr.String())
	}
	if took < drain || took >= drain+time.Second {
		t.Errorf("serve took %v to stop with a request running, want at least the drain, %v, and under %v", took, drain, drain+time.Second)
	}
	if cut := v.stdout.logged(t, "connections still open at the stop deadline were closed"); len(cut) != 1 ||
		cut[0]["listener"] != "server.listen (PISATI_SERVER_LISTEN)" || cut[0]["waited"] != "8s" {
		t.Errorf("serve logged %v, want one record of the public listener's connections closed after 8 s", cut)
	}
	closeAndSettle(t, conn)
}

func TestServeStoppedDuringStart(t *testing.T) {
	s := newSite(t)
	h, stdout, stderr := s.host("serve")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if code := run(ctx, h); code != 0 {
		t.Errorf("serve stopped before it started exited %d, want 0:\n%s", code, stderr.String())
	}
	if stdout.String() != "" || stderr.String() != "" {
		t.Errorf("serve stopped before it started wrote %q and %q, want nothing", stdout.String(), stderr.String())
	}
	entries, err := os.ReadDir(s.state)
	if err != nil || len(entries) != 0 {
		t.Errorf("the state directory holds %v (%v), want nothing", entries, err)
	}
}

// The state directory's refusals, as serve's one line names them.
const (
	stateDirAdvice = "storage.state_dir (PISATI_STORAGE_STATE_DIR)"
)

func TestServeRefusesUnwritableState(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		s := newSite(t)
		missing := filepath.Join(t.TempDir(), "absent")
		s.env["PISATI_STORAGE_STATE_DIR"] = missing
		holdPorts(t, s)
		r := s.run(t, "serve")
		want := "pisati: the state directory " + missing + " does not exist. Create it, and give the user that runs Pisati " +
			"permission to write to it. Or set " + stateDirAdvice + " to another directory."
		if r.code != 1 || r.line(t) != want {
			t.Errorf("serve = %d, %q; want 1, %q", r.code, r.stderr, want)
		}
		if _, err := os.Lstat(missing); !os.IsNotExist(err) {
			t.Errorf("serve made the missing state directory: %v", err)
		}
		mustNotHaveStarted(t, r)
	})
	t.Run("not a directory", func(t *testing.T) {
		s := newSite(t)
		file := filepath.Join(t.TempDir(), "state")
		if err := os.WriteFile(file, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		s.env["PISATI_STORAGE_STATE_DIR"] = file
		holdPorts(t, s)
		r := s.run(t, "serve")
		want := "pisati: the state directory " + file + " is not a directory. Set " + stateDirAdvice + " to a directory."
		if r.code != 1 || r.line(t) != want {
			t.Errorf("serve = %d, %q; want 1, %q", r.code, r.stderr, want)
		}
		mustNotHaveStarted(t, r)
	})
	t.Run("not writable", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root writes to any directory, whatever its mode")
		}
		s := newSite(t)
		// No write and no search: either refuses the test file.
		if err := os.Chmod(s.state, 0o400); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := os.Chmod(s.state, 0o600); err != nil {
				t.Error(err)
			}
		})
		holdPorts(t, s)
		r := s.run(t, "serve")
		want := "pisati: Pisati cannot write to the state directory " + s.state + ", because the user that runs Pisati " +
			"does not have permission to. Give that user permission to write to " + s.state + ", or set " + stateDirAdvice +
			" to a directory it can write to."
		if r.code != 1 || r.line(t) != want {
			t.Errorf("serve = %d, %q; want 1, %q", r.code, r.stderr, want)
		}
		if entries, err := os.ReadDir(s.state); err != nil || len(entries) != 0 {
			t.Errorf("the state directory holds %v (%v), want nothing", entries, err)
		}
		mustNotHaveStarted(t, r)
	})
}

// mustNotHaveStarted fails the test unless serve, which ended with r,
// logged no start. A test that calls it held both ports during the serve
// (holdPorts), so a listener opened before the refusal would have made
// the refusal one about a port.
func mustNotHaveStarted(t *testing.T, r result) {
	t.Helper()
	if strings.Contains(r.stdout, "serving started") {
		t.Errorf("serve started serving before it refused:\n%s", r.stdout)
	}
}

func TestServeRefusesDatabaseWithoutKey(t *testing.T) {
	keys := t.TempDir()
	const advice = "This database needs the main key it was last used with. A backup made by pisati backup does not " +
		"hold that key. "
	for _, tc := range []struct {
		name string
		// key, when set, gives the old site's main key and the new
		// site's its own, each from where it puts it in env.
		key func(t *testing.T, s *site, name string)
		// before has the new site make its own key before the restore.
		before bool
		want   func(s *site) string
	}{
		{
			name: "a key generated on this start",
			want: func(s *site) string {
				path := filepath.Join(s.state, "keys", "main.key")
				return "pisati: there was no main key in " + path + ", so Pisati created one on this start. " +
					"That new key cannot decrypt the data keys in this database. " + advice + "Replace the file " + path +
					" with that key" +
					", or set keys.main_key_file or PISATI_KEYS_MAIN_KEY_FILE to the file that holds it, " +
					"or put it in the credential pisati-main-key."
			},
		},
		{
			name:   "a key generated before",
			before: true,
			want: func(s *site) string {
				path := filepath.Join(s.state, "keys", "main.key")
				return "pisati: the main key in " + path + " cannot decrypt the data keys in this database. " +
					advice + "Replace the file " + path + " with that key" +
					", or set keys.main_key_file or PISATI_KEYS_MAIN_KEY_FILE to the file that holds it, " +
					"or put it in the credential pisati-main-key."
			},
		},
		{
			name: "a key file",
			key: func(t *testing.T, s *site, name string) {
				s.env["PISATI_KEYS_MAIN_KEY_FILE"] = writeKeyFile(t, keys, name)
			},
			want: func(s *site) string {
				path := s.env["PISATI_KEYS_MAIN_KEY_FILE"]
				return "pisati: the main key in " + path + " cannot decrypt the data keys in this database. " +
					"keys.main_key_file or PISATI_KEYS_MAIN_KEY_FILE names that file. " + advice + "Replace the file " + path +
					" with that key" +
					", or set keys.main_key_file or PISATI_KEYS_MAIN_KEY_FILE to the file that holds it."
			},
		},
		{
			name: "a credential",
			key: func(t *testing.T, s *site, _ string) {
				creds := t.TempDir()
				writeKeyFile(t, creds, "pisati-main-key")
				s.env["CREDENTIALS_DIRECTORY"] = creds
			},
			want: func(*site) string {
				return "pisati: the main key in the credential pisati-main-key cannot decrypt the data keys in this database. " +
					advice + "Put that key in the credential pisati-main-key, " +
					"or set keys.main_key_file or PISATI_KEYS_MAIN_KEY_FILE to the file that holds it."
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The old site makes its first data key under its main key,
			// and is backed up.
			old := newSite(t)
			if tc.key != nil {
				tc.key(t, old, "old.key")
			}
			if code := old.serve(t).stop(); code != 0 {
				t.Fatalf("the old site's serve exited %d", code)
			}
			backup := filepath.Join(t.TempDir(), "copy.db")
			old.mustRun(t, "backup", backup)

			// The new site restores the database alone, under a main key
			// of its own.
			s := newSite(t)
			if tc.key != nil {
				tc.key(t, s, "new.key")
			}
			if tc.before {
				if code := s.serve(t).stop(); code != 0 {
					t.Fatalf("the new site's first serve exited %d", code)
				}
			}
			s.mustRun(t, "restore", backup)
			holdPorts(t, s)
			r := s.run(t, "serve")
			if want := tc.want(s); r.code != 1 || r.line(t) != want {
				t.Errorf("serve = %d, %q; want 1, %q", r.code, r.stderr, want)
			}
			mustNotHaveStarted(t, r)
		})
	}
}

func TestServeReportsEveryPortRefused(t *testing.T) {
	s := newSite(t)
	holdPorts(t, s)
	r := s.run(t, "serve")
	want := []string{
		fmt.Sprintf("pisati: Pisati cannot listen on 127.0.0.1:%d, the address that server.listen (PISATI_SERVER_LISTEN) "+
			"sets, because another process already uses it. Stop that process, or set server.listen (PISATI_SERVER_LISTEN) "+
			"to another address.", s.public),
		fmt.Sprintf("pisati: Pisati cannot listen on 127.0.0.1:%d, the address that server.internal_listen "+
			"(PISATI_SERVER_INTERNAL_LISTEN) sets, because another process already uses it. Stop that process, or set "+
			"server.internal_listen (PISATI_SERVER_INTERNAL_LISTEN) to another address.", s.internal),
	}
	if got := r.lines(t); r.code != 1 || !slices.Equal(got, want) {
		t.Errorf("serve with both ports taken = %d, %q; want 1 and one line for each port:\n%s", r.code, got, strings.Join(want, "\n"))
	}
	mustNotHaveStarted(t, r)
}

func TestSecondSignalCutsStopShort(t *testing.T) {
	t.Run("during the drain", func(t *testing.T) {
		s := newSite(t)
		v := s.serve(t)
		// A request whose body never comes in full would keep the stop
		// waiting for the whole deadline.
		conn := stalledRequest(t, s)
		v.cancel()
		time.Sleep(200 * time.Millisecond)
		halting := time.Now()
		v.halt()
		code := v.wait()
		mustHaveHalted(t, s, v, code, time.Since(halting), true)
		closeAndSettle(t, conn)
		mustLogNothingAfterHalt(t, v)
	})

	t.Run("during the telemetry flush", func(t *testing.T) {
		// A collector that holds every export until the test lets it go,
		// so that the stop waits in the flush for the whole deadline.
		hold := make(chan struct{})
		collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, err := io.Copy(io.Discard, r.Body); err != nil {
				t.Errorf("read the export: %v", err)
			}
			<-hold
		}))
		s := newSite(t)
		s.env["PISATI_TELEMETRY_OTLP_ENDPOINT"] = collector.URL
		v := s.serve(t)
		get(t, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/login", s.public)) // a span to send
		v.cancel()
		time.Sleep(500 * time.Millisecond)
		halting := time.Now()
		v.halt()
		code := v.wait()
		mustHaveHalted(t, s, v, code, time.Since(halting), true)
		close(hold)
		collector.Close()
		settle(t)
		mustLogNothingAfterHalt(t, v)
	})

	t.Run("during the start", func(t *testing.T) {
		s := newSite(t)
		s.mustRun(t, "migrate")
		release := holdWriteLock(t, s.state)
		v := s.serveStarting(t)
		time.Sleep(500 * time.Millisecond) // serve waits for the write lock, in its start
		halting := time.Now()
		v.halt()
		code := v.wait()
		mustHaveHalted(t, s, v, code, time.Since(halting), false)
		if strings.Contains(v.stdout.String(), "serving started") {
			t.Errorf("serve started serving before the halt:\n%s", v.stdout.String())
		}
		release()
		settle(t)
		mustLogNothingAfterHalt(t, v)
	})

	t.Run("with a job running", func(t *testing.T) {
		// A job whose handler runs until the test lets it go: the halt
		// does not wait for the job worker.
		kind, err := jobs.NewKind("test.held")
		if err != nil {
			t.Fatal(err)
		}
		running, letGo := make(chan struct{}), make(chan struct{})
		s := newSite(t)
		s.jobs = map[jobs.Kind]jobs.Handler{kind: func(context.Context, []byte) error {
			close(running)
			<-letGo
			return nil
		}}
		s.mustRun(t, "migrate")
		withStore(t, s.state, func(db *store.Store) {
			if _, err := jobs.Enqueue(t.Context(), db.Queries(), jobs.Job{Kind: kind}); err != nil {
				t.Fatal(err)
			}
		})
		v := s.serve(t)
		select {
		case <-running:
		case <-time.After(10 * time.Second):
			t.Fatal("the job did not start within 10 s")
		}
		halting := time.Now()
		v.halt()
		code := v.wait()
		mustHaveHalted(t, s, v, code, time.Since(halting), false)
		close(letGo)
		settle(t)
		mustLogNothingAfterHalt(t, v)
	})
}

func TestServeWithPprof(t *testing.T) {
	s := newSite(t)
	s.env["PISATI_TELEMETRY_PPROF"] = "on"
	v := s.serve(t)
	public := fmt.Sprintf("http://127.0.0.1:%d", s.public)
	internal := fmt.Sprintf("http://127.0.0.1:%d", s.internal)
	for _, tc := range []struct {
		method, url string
		status      int
	}{
		{http.MethodGet, internal + "/debug/pprof/", http.StatusOK},
		{http.MethodGet, internal + "/debug/pprof/cmdline", http.StatusOK},
		{http.MethodPost, internal + "/debug/pprof/symbol", http.StatusOK},
		{http.MethodGet, internal + "/healthz", http.StatusOK},
		{http.MethodGet, public + "/debug/pprof/", http.StatusNotFound},
	} {
		if status, _ := get(t, tc.method, tc.url); status != tc.status {
			t.Errorf("%s %s = %d, want %d", tc.method, tc.url, status, tc.status)
		}
	}
	// The catch-all still answers everything else, with nothing.
	if status, body := get(t, http.MethodGet, internal+"/nothing"); status != http.StatusNotFound || body != "" {
		t.Errorf("GET /nothing on the internal listener = %d %q, want an empty 404", status, body)
	}
	if code := v.stop(); code != 0 {
		t.Errorf("serve exited %d", code)
	}
}

func TestServeRefusesSameAddress(t *testing.T) {
	const both = "server.listen (PISATI_SERVER_LISTEN) is %s and server.internal_listen (PISATI_SERVER_INTERNAL_LISTEN) " +
		"is %s, both on port %d. "
	for _, tc := range []struct {
		name, public, internal string
		// why is what the line says after the settings, with %[1]d for
		// the port.
		why string
	}{
		{"the same address", "127.0.0.1:%d", "127.0.0.1:%d", ""},
		{
			"every address and one of them", ":%d", "127.0.0.1:%d",
			":%[1]d stands for every address of this machine, so Pisati needs the two on different ports.",
		},
		{
			"one address and every address", "127.0.0.1:%d", "[::]:%d",
			"[::]:%[1]d stands for every address of this machine, so Pisati needs the two on different ports.",
		},
		{
			"every IPv4 address and every address", "0.0.0.0:%d", "[::]:%d",
			"0.0.0.0:%[1]d stands for every IPv4 address of this machine, and [::]:%[1]d for every address of this " +
				"machine, so Pisati needs the two on different ports.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSite(t)
			public, internal := fmt.Sprintf(tc.public, s.public), fmt.Sprintf(tc.internal, s.public)
			s.env["PISATI_SERVER_LISTEN"] = public
			s.env["PISATI_SERVER_INTERNAL_LISTEN"] = internal
			holdPorts(t, s)
			r := s.run(t, "serve")
			want := fmt.Sprintf("pisati: server.listen (PISATI_SERVER_LISTEN) and server.internal_listen "+
				"(PISATI_SERVER_INTERNAL_LISTEN) are both %s. Pisati needs a different address for each. Set one of them "+
				"to another port.", public)
			if tc.why != "" {
				want = "pisati: " + fmt.Sprintf(both, public, internal, s.public) + fmt.Sprintf(tc.why, s.public) +
					" Set one of them to another port."
			}
			if r.code != 1 || r.line(t) != want {
				t.Errorf("serve = %d, %q; want 1, %q", r.code, r.stderr, want)
			}
			if entries, err := os.ReadDir(s.state); err != nil || len(entries) != 0 {
				t.Errorf("the state directory holds %v (%v), want nothing: serve refuses before anything else", entries, err)
			}
			mustNotHaveStarted(t, r)
		})
	}
}

func TestServeNamesItsOwnListener(t *testing.T) {
	// Two names that Pisati does not compare, as it looks no name up:
	// localhost listens on 127.0.0.1 here, so the second listener finds
	// the address in use by the first, whichever binds first.
	s := newSite(t)
	s.env["PISATI_SERVER_LISTEN"] = fmt.Sprintf("localhost:%d", s.public)
	s.env["PISATI_SERVER_INTERNAL_LISTEN"] = fmt.Sprintf("127.0.0.1:%d", s.public)
	r := s.run(t, "serve")
	line := func(addr, setting, other, otherSetting, host, otherHost string) string {
		return fmt.Sprintf("pisati: Pisati cannot listen on %[1]s, the address that %[2]s sets, because that address is "+
			"in use already. %[4]s is %[3]s, on the same port. If %[6]s and %[5]s name the same address, Pisati itself "+
			"uses it, for %[4]s: set one of the two settings to another port. If they do not, another process uses it: "+
			"stop that process, or set %[2]s to another address.", addr, setting, other, otherSetting, host, otherHost)
	}
	public, internal := s.env["PISATI_SERVER_LISTEN"], s.env["PISATI_SERVER_INTERNAL_LISTEN"]
	const publicSetting = "server.listen (PISATI_SERVER_LISTEN)"
	const internalSetting = "server.internal_listen (PISATI_SERVER_INTERNAL_LISTEN)"
	wants := []string{
		line(internal, internalSetting, public, publicSetting, "127.0.0.1", "localhost"),
		line(public, publicSetting, internal, internalSetting, "localhost", "127.0.0.1"),
	}
	if lines := r.lines(t); r.code != 1 || len(lines) != 1 || !slices.Contains(wants, lines[0]) {
		t.Errorf("serve = %d, %q; want 1, and one of:\n%s", r.code, r.stderr, strings.Join(wants, "\n"))
	}
	mustNotHaveStarted(t, r)
}
