// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package main

import (
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/divbast/pisati/internal/store"
)

// The lines the keys commands print once they have done their work.
const (
	rewrapped = "The data keys are now encrypted with the new main key. Before you start pisati serve again, " +
		"set keys.main_key_file or PISATI_KEYS_MAIN_KEY_FILE to the new key file, " +
		"or put the new key in the credential pisati-main-key.\n"
	rotated = "A new data key is now in use.\n"
)

// keyStates returns the state of each data key in the database at state,
// in the order they were made.
func keyStates(t *testing.T, state string) []string {
	t.Helper()
	var states []string
	withStore(t, state, func(db *store.Store) {
		rows, err := db.DB().QueryContext(t.Context(), `SELECT state FROM data_keys ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			states = append(states, s)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
	})
	return states
}

func TestKeysRewrapAndRotate(t *testing.T) {
	s := newSite(t)
	if code := s.serve(t).stop(); code != 0 {
		t.Fatalf("serve exited %d", code)
	}

	// Rewrap from the generated key to a key file, named relative to the
	// working directory.
	dir := t.TempDir()
	t.Chdir(dir)
	newKey := writeKeyFile(t, dir, "new.key")
	if r := s.mustRun(t, "keys", "rewrap", "--new-main-key-file", "new.key"); r.stdout != rewrapped || r.stderr != "" {
		t.Errorf("pisati keys rewrap wrote %q and %q, want %q and nothing on stderr", r.stdout, r.stderr, rewrapped)
	}
	generated := filepath.Join(s.state, "keys", "main.key")
	if r := s.run(t, "serve"); r.code != 1 || !strings.HasPrefix(r.line(t), "pisati: the main key in "+generated+
		" cannot decrypt") {
		t.Errorf("serve with the old key = %d, %q; want 1, saying the key in %s cannot decrypt the data keys", r.code, r.stderr, generated)
	}

	// The new key serves, and neither command runs beside serve.
	s.env["PISATI_KEYS_MAIN_KEY_FILE"] = newKey
	v := s.serve(t)
	for _, args := range [][]string{{"keys", "rotate"}, {"keys", "rewrap", "--new-main-key-file", newKey}} {
		if r := s.run(t, args...); r.code != 1 || r.line(t) != lockedLine(s.state) {
			t.Errorf("pisati %q while serving = %d, %q; want 1, %q", args, r.code, r.stderr, lockedLine(s.state))
		}
	}
	if code := v.stop(); code != 0 {
		t.Fatalf("serve exited %d", code)
	}

	if r := s.mustRun(t, "keys", "rotate"); r.stdout != rotated || r.stderr != "" {
		t.Errorf("pisati keys rotate wrote %q and %q, want %q and nothing on stderr", r.stdout, r.stderr, rotated)
	}
	if states := keyStates(t, s.state); strings.Join(states, " ") != "retired current" {
		t.Errorf("the data keys are %v after the rotation, want the first retired and the second current", states)
	}
	reencrypt := func() (state string) {
		withStore(t, s.state, func(db *store.Store) {
			if n := count(t, db, `SELECT count(*) FROM jobs WHERE kind = 'crypto.reencrypt'`); n != 1 {
				t.Errorf("%d re-encryption jobs, want 1", n)
			}
			if err := db.DB().QueryRowContext(t.Context(), `SELECT state FROM jobs WHERE kind = 'crypto.reencrypt'`).
				Scan(&state); err != nil {
				t.Fatal(err)
			}
		})
		return state
	}
	if state := reencrypt(); state != "ready" {
		t.Errorf("the re-encryption job is %s after the rotation, want ready", state)
	}

	// The next serve runs the re-encryption.
	v = s.serve(t)
	within(t, "the re-encryption job is done", func() bool { return reencrypt() == "done" })
	if code := v.stop(); code != 0 {
		t.Errorf("serve exited %d", code)
	}
}

func TestKeysMigrateFirst(t *testing.T) {
	// On a state directory nothing has used yet, rotate migrates the new
	// database, generates the main key, and makes the first data key and
	// then the second. Its log goes to stderr, its result to stdout.
	s := newSite(t)
	h, stdout, stderr := s.host("keys", "rotate")
	if code := run(t.Context(), h); code != 0 {
		t.Fatalf("pisati keys rotate exited %d:\n%s", code, stderr.String())
	}
	if stdout.String() != rotated {
		t.Errorf("pisati keys rotate wrote %q to stdout, want %q", stdout.String(), rotated)
	}
	if n := len(stderr.logged(t, "main key generated on the data volume")); n != 1 {
		t.Errorf("pisati keys rotate logged %d records of the generated key to stderr, want 1:\n%s", n, stderr.String())
	}
	if states := keyStates(t, s.state); strings.Join(states, " ") != "retired current" {
		t.Errorf("the data keys are %v, want the first retired and the second current", states)
	}
}

func TestKeysRewrapNeverShowsAKeyGivenAsItsFile(t *testing.T) {
	// A key pasted where the path of its file belongs. One that starts
	// with "/" reads as a full path already: this one does not, so that
	// the command would resolve it against the working directory.
	key := "/"
	for strings.HasPrefix(key, "/") {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			t.Fatal(err)
		}
		key = base64.StdEncoding.EncodeToString(raw)
	}
	s := newSite(t)
	t.Chdir(t.TempDir())
	r := s.run(t, "keys", "rewrap", "--new-main-key-file", key)
	want := "pisati: the value of --new-main-key-file reads as a key, not as the path of a file. Give the path of the " +
		"file that holds the new main key. Pisati does not show the value, because it may be the key itself."
	if r.code != 1 || r.line(t) != want || strings.Contains(r.stdout+r.stderr, key) {
		t.Errorf("pisati keys rewrap --new-main-key-file <a key> = %d, %q, %q; want 1, %q, and the key nowhere", r.code,
			r.stdout, r.stderr, want)
	}
	if entries, err := os.ReadDir(s.state); err != nil || len(entries) != 0 {
		t.Errorf("the state directory holds %v (%v), want nothing: the command refuses before anything else", entries, err)
	}
}

func TestKeyGivenAsAnArgumentNeverShown(t *testing.T) {
	// A key pasted with no flag before it, in place of
	// --new-main-key-file's value, which kong refuses as an argument it
	// did not expect; a key among the arguments of a command that takes
	// none; and a command line refused for another reason, which holds a
	// key as a flag's value.
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	key := base64.StdEncoding.EncodeToString(raw)
	for _, args := range [][]string{
		{"keys", "rewrap", key},
		{"keys", "rewrap", "--new-main-key-file", "/run/secrets/new.key", key},
		{"keys", "rotate", key + "\n"},
		// A flag's value after "=", which the parser's error may quote.
		{"keys", "rewrap", "--new-main-key-file=" + key, "and-more"},
	} {
		s := newSite(t)
		r := s.run(t, args...)
		const want = "pisati: the command line holds a value that reads as a key, where Pisati does not take one, so " +
			"Pisati does not show the command line's error. Give the path of a key file, never the key itself, as in: " +
			"pisati keys rewrap --new-main-key-file /path/to/new.key."
		if lines := strings.Split(r.stderr, "\n"); r.code != 2 || lines[0] != want || strings.Contains(r.stdout+r.stderr, strings.TrimSpace(key)) {
			t.Errorf("pisati %d arguments with a key among them = %d, %q, %q; want 2, %q first, and the key nowhere",
				len(args), r.code, r.stdout, r.stderr, want)
		}
	}
}
