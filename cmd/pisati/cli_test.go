// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package main

import (
	"strings"
	"testing"
)

func TestVersion(t *testing.T) {
	r := newSite(t).run(t, "version")
	if r.code != 0 || r.stdout != "Pisati dev\n" || r.stderr != "" {
		t.Errorf("pisati version = %d, %q, %q; want 0, %q and nothing on stderr", r.code, r.stdout, r.stderr, "Pisati dev\n")
	}
}

func TestUnknownCommand(t *testing.T) {
	for _, tc := range []struct {
		args         []string
		error, usage string
	}{
		{[]string{"bogus"}, "pisati: unexpected argument bogus", "Usage: pisati <command>"},
		{nil, `pisati: expected one of "serve", "migrate"`, "Usage: pisati <command>"},
		{[]string{"backup"}, `pisati: expected "<dest>"`, "Usage: pisati backup <dest>"},
		{[]string{"keys", "rewrap"}, "pisati: missing flags: --new-main-key-file=PATH", "Usage: pisati keys rewrap --new-main-key-file=PATH"},
		{[]string{"version", "extra"}, "pisati: unexpected argument extra", "Usage: pisati version"},
	} {
		r := newSite(t).run(t, tc.args...)
		if r.code != 2 {
			t.Errorf("pisati %q exited %d, want 2", tc.args, r.code)
		}
		if r.stdout != "" {
			t.Errorf("pisati %q wrote to stdout: %q", tc.args, r.stdout)
		}
		first, rest, _ := strings.Cut(r.stderr, "\n")
		if !strings.HasPrefix(first, tc.error) || !strings.Contains(rest, tc.usage) {
			t.Errorf("pisati %q wrote %q to stderr, want %q and then the usage %q", tc.args, r.stderr, tc.error, tc.usage)
		}
	}
}

func TestHelp(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"-h"}} {
		r := newSite(t).run(t, args...)
		if r.code != 0 || r.stderr != "" {
			t.Errorf("pisati %q = %d, stderr %q; want 0 and nothing on stderr", args, r.code, r.stderr)
		}
		for _, want := range []string{
			"Usage: pisati <command>", "serve", "migrate", "backup <dest>", "restore <src>",
			"keys rewrap --new-main-key-file=PATH", "keys rotate", "health", "version",
		} {
			if !strings.Contains(r.stdout, want) {
				t.Errorf("pisati %q: the help does not name %q:\n%s", args, want, r.stdout)
			}
		}
	}
}

func TestUnknownEnvRefused(t *testing.T) {
	for _, tc := range []struct {
		command []string
		prefix  string
	}{
		{[]string{"serve"}, "pisati: the configuration has these problems:\n"},
		{[]string{"migrate"}, "pisati: the configuration has these problems:\n"},
		{[]string{"backup", "/nowhere/copy.db"}, "pisati: the configuration has these problems:\n"},
		{[]string{"health"}, "config: the configuration has these problems: "},
	} {
		s := newSite(t)
		s.env["PISATI_BOGUS"] = "1"
		r := s.run(t, tc.command...)
		if r.code != 1 {
			t.Errorf("pisati %q with PISATI_BOGUS set exited %d, want 1", tc.command, r.code)
		}
		if !strings.HasPrefix(r.stderr, tc.prefix) || !strings.Contains(r.stderr, "PISATI_BOGUS") {
			t.Errorf("pisati %q wrote %q to stderr, want %q naming PISATI_BOGUS", tc.command, r.stderr, tc.prefix)
		}
		if r.stdout != "" {
			t.Errorf("pisati %q wrote to stdout: %q", tc.command, r.stdout)
		}
	}
}

func TestDirectoryVariablesRefused(t *testing.T) {
	for _, name := range []string{"CONFIGURATION_DIRECTORY", "CREDENTIALS_DIRECTORY"} {
		for _, value := range []string{"etc/pisati", "/etc/pisati/", "/etc//pisati", "/etc/./pisati", "/a:/b"} {
			s := newSite(t)
			s.env[name] = value
			r := s.run(t, "migrate")
			want := "  " + name + ` is "` + value + `": it must be one absolute path`
			if r.code != 1 || !strings.Contains(r.stderr, want) {
				t.Errorf("%s=%q: pisati migrate = %d, %q; want 1, naming the variable and its value", name, value, r.code, r.stderr)
			}
		}
	}
	s := newSite(t)
	s.env["CREDENTIALS_DIRECTORY"] = ""
	s.mustRun(t, "migrate")
}
