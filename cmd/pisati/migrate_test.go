// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package main

import (
	"path/filepath"
	"testing"
)

// lockedLine is the line a command that needs the state directory's lock
// ends with while another Pisati holds it.
func lockedLine(state string) string {
	return "pisati: another Pisati process is using the state directory " + state +
		". Only one Pisati process can use it at a time. Stop the other process, then run this command again."
}

func TestMigrateCommand(t *testing.T) {
	withUmask(t, 0o022)
	s := newSite(t)
	for _, want := range []string{"schema 0 -> 1\n", "schema 1 -> 1\n"} {
		if r := s.run(t, "migrate"); r.code != 0 || r.stdout != want || r.stderr != "" {
			t.Errorf("pisati migrate = %d, %q, %q; want 0, %q and nothing on stderr", r.code, r.stdout, r.stderr, want)
		}
	}
	mustOwnerOnly(t, s.state, "pisati.db", "pisati.lock")

	v := s.serve(t)
	r := s.run(t, "migrate")
	if want := lockedLine(s.state); r.code != 1 || r.line(t) != want || r.stdout != "" {
		t.Errorf("pisati migrate while serving = %d, %q, %q; want 1, %q", r.code, r.stdout, r.stderr, want)
	}
	if code := v.stop(); code != 0 {
		t.Errorf("serve exited %d", code)
	}
}

func TestMigrateRefusesMissingState(t *testing.T) {
	// The commands but serve check the state directory as serve does.
	s := newSite(t)
	missing := filepath.Join(t.TempDir(), "absent")
	s.env["PISATI_STORAGE_STATE_DIR"] = missing
	r := s.run(t, "migrate")
	want := "pisati: the state directory " + missing + " does not exist. Create it, and give the user that runs Pisati " +
		"permission to write to it. Or set storage.state_dir (PISATI_STORAGE_STATE_DIR) to another directory."
	if r.code != 1 || r.line(t) != want || r.stdout != "" {
		t.Errorf("pisati migrate = %d, %q, %q; want 1, %q", r.code, r.stdout, r.stderr, want)
	}
}
