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
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

func TestServeDrainsWithinDeadline(t *testing.T) {
	s := newSite(t)
	v := s.serve(t)

	// A request whose body never comes in full keeps its handler reading
	// past the stop deadline.
	var d net.Dialer
	conn, err := d.DialContext(t.Context(), "tcp", fmt.Sprintf("127.0.0.1:%d", s.public))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := conn.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err := fmt.Fprintf(conn, "POST /_/theme HTTP/1.1\r\nHost: localhost:%d\r\n"+
		"Content-Type: application/x-www-form-urlencoded\r\nContent-Length: 100\r\n\r\ntheme=", s.public); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)

	stopping := time.Now()
	code := v.stop()
	took := time.Since(stopping)
	if code != 0 {
		t.Errorf("serve exited %d when the deadline cut a request off, want 0:\n%s", code, v.stderr.String())
	}
	if took < drain || took >= drain+time.Second {
		t.Errorf("serve took %v to stop with a request running, want at least the drain, %v, and under %v", took, drain, drain+time.Second)
	}
	if cut := v.stdout.logged(t, "connections closed at the stop deadline"); len(cut) != 1 || cut[0]["listener"] != "server.listen" {
		t.Errorf("serve logged %v, want one record of the public listener's connections closed at the deadline", cut)
	}
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

func TestServeRefusesUnwritableState(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes to any directory, whatever its mode")
	}
	t.Run("missing", func(t *testing.T) {
		s := newSite(t)
		missing := filepath.Join(t.TempDir(), "absent")
		s.env["PISATI_STORAGE_STATE_DIR"] = missing
		r := s.run(t, "serve")
		want := "pisati: the state directory " + missing + " does not exist. Create it and let the user that runs Pisati write to it, " +
			"or set storage.state_dir to another directory."
		if r.code != 1 || r.line(t) != want {
			t.Errorf("serve = %d, %q; want 1, %q", r.code, r.stderr, want)
		}
		if _, err := os.Lstat(missing); !os.IsNotExist(err) {
			t.Errorf("serve made the missing state directory: %v", err)
		}
		mustNotHaveStarted(t, s, r)
	})
	t.Run("read-only", func(t *testing.T) {
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
		r := s.run(t, "serve")
		want := "pisati: cannot write to the state directory " + s.state + ": permission denied. " +
			"Let the user that runs Pisati write to it."
		if r.code != 1 || r.line(t) != want {
			t.Errorf("serve = %d, %q; want 1, %q", r.code, r.stderr, want)
		}
		if entries, err := os.ReadDir(s.state); err != nil || len(entries) != 0 {
			t.Errorf("the state directory holds %v (%v), want nothing", entries, err)
		}
		mustNotHaveStarted(t, s, r)
	})
}

// mustNotHaveStarted fails the test unless serve, which ended with r,
// logged no start and left both of s's ports free.
func mustNotHaveStarted(t *testing.T, s *site, r result) {
	t.Helper()
	if strings.Contains(r.stdout, "serving started") {
		t.Errorf("serve started serving before it refused:\n%s", r.stdout)
	}
	var lc net.ListenConfig
	for _, port := range []int{s.public, s.internal} {
		ln, err := lc.Listen(t.Context(), "tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			t.Errorf("port %d is taken after serve refused: %v", port, err)
			continue
		}
		if err := ln.Close(); err != nil {
			t.Error(err)
		}
	}
}

func TestServeRefusesDatabaseWithoutKey(t *testing.T) {
	keys := t.TempDir()
	for _, tc := range []struct {
		name string
		// key, when set, gives the old site's main key and the new
		// site's its own, each from where it puts it in env.
		key  func(t *testing.T, s *site, name string)
		want func(s *site) string
	}{
		{
			name: "a generated key",
			want: func(s *site) string {
				path := filepath.Join(s.state, "keys", "main.key")
				return "pisati: the main key in " + path + " cannot decrypt the data keys in this database. " +
					"Put the main key that was backed up with this database at " + path + ". " +
					"Or, if you keep that key in another file, set keys.main_key_file or PISATI_KEYS_MAIN_KEY_FILE to that file, " +
					"or put the key in the credential pisati-main-key."
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
					"Put the main key that was backed up with this database in " + path + ", " +
					"or set keys.main_key_file or PISATI_KEYS_MAIN_KEY_FILE to the file that holds it."
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
					"Put the main key that was backed up with this database in the credential pisati-main-key, " +
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
			s.mustRun(t, "restore", backup)
			r := s.run(t, "serve")
			if want := tc.want(s); r.code != 1 || r.line(t) != want {
				t.Errorf("serve = %d, %q; want 1, %q", r.code, r.stderr, want)
			}
			mustNotHaveStarted(t, s, r)
		})
	}
}
