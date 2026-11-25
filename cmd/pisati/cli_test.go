// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package main

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
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
		line    string
	}{
		{[]string{"serve"}, "pisati: PISATI_BOGUS: unknown setting."},
		{[]string{"migrate"}, "pisati: PISATI_BOGUS: unknown setting."},
		{[]string{"backup", "/nowhere/copy.db"}, "pisati: PISATI_BOGUS: unknown setting."},
		{[]string{"health"}, "config: PISATI_BOGUS: unknown setting."},
	} {
		s := newSite(t)
		s.env["PISATI_BOGUS"] = "1"
		r := s.run(t, tc.command...)
		if r.code != 1 || r.line(t) != tc.line || r.stdout != "" {
			t.Errorf("pisati %q with PISATI_BOGUS set = %d, %q, %q; want 1 and %q", tc.command, r.code, r.stdout, r.stderr, tc.line)
		}
	}
}

func TestConfigProblemsOneLineEach(t *testing.T) {
	for _, tc := range []struct {
		command []string
		prefix  string
	}{
		{[]string{"migrate"}, "pisati: "},
		{[]string{"health"}, "config: "},
	} {
		s := newSite(t)
		s.env["PISATI_BOGUS"] = "1"
		delete(s.env, "PISATI_SERVER_BASE_URL")
		r := s.run(t, tc.command...)
		lines := r.lines(t)
		if r.code != 1 || len(lines) != 2 {
			t.Fatalf("pisati %q with two problems = %d, %q; want 1 and two lines", tc.command, r.code, r.stderr)
		}
		for i, want := range []string{"PISATI_BOGUS: unknown setting.", "server.base_url"} {
			if !strings.HasPrefix(lines[i], tc.prefix) || !strings.Contains(lines[i], want) || !strings.HasSuffix(lines[i], ".") {
				t.Errorf("pisati %q: line %d is %q, want it to start %q, name %q and end with a full stop",
					tc.command, i+1, lines[i], tc.prefix, want)
			}
		}
	}
}

func TestDirectoryVariablesRefused(t *testing.T) {
	for _, name := range []string{"CONFIGURATION_DIRECTORY", "CREDENTIALS_DIRECTORY"} {
		for _, tc := range []struct{ value, rule string }{
			{"/a:/b", "which names more than one directory. Set it to one directory."},
			{"etc/pisati", `which is not a full path. Set it to a full path that starts with "/".`},
			{"/etc//pisati", `which has "//" in it. Write one "/" there instead.`},
			{"/etc/pisati/", `which ends with "/". Remove the "/" at the end.`},
			{"/etc/./pisati", `which has "." or ".." as one of its directory names. Write the path without them.`},
			{"/etc/pisati/..", `which has "." or ".." as one of its directory names. Write the path without them.`},
		} {
			s := newSite(t)
			s.env[name] = tc.value
			r := s.run(t, "migrate")
			want := "pisati: " + name + ` is set to "` + tc.value + `", ` + tc.rule
			if r.code != 1 || r.line(t) != want {
				t.Errorf("%s=%q: pisati migrate = %d, %q; want 1, %q", name, tc.value, r.code, r.stderr, want)
			}
		}
	}
	s := newSite(t)
	s.env["CREDENTIALS_DIRECTORY"] = ""
	s.mustRun(t, "migrate")
}

func TestSecondSignalStopsOtherCommands(t *testing.T) {
	const line = "pisati: the command stopped at once, before it finished, because a second SIGTERM or SIGINT signal arrived."
	t.Run("keys rotate, waiting for the write lock", func(t *testing.T) {
		s := newSite(t)
		s.mustRun(t, "migrate")
		release := holdWriteLock(t, s.state)
		halt, wait := s.halting(t, "keys", "rotate")
		time.Sleep(500 * time.Millisecond)
		halting := time.Now()
		halt()
		r := wait()
		// Its log, on stderr too, holds the warning of the key it
		// generated: the halt's line comes last.
		lines := r.lines(t)
		if took := time.Since(halting); r.code != 1 || lines[len(lines)-1] != line || took >= time.Second {
			t.Errorf("pisati keys rotate halted = %d, %q after %v; want 1, %q last, at once", r.code, r.stderr, took, line)
		}
		release()
		settle(t)
	})
	t.Run("health, waiting for an answer", func(t *testing.T) {
		var lc net.ListenConfig
		ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		accepted := make(chan net.Conn, 1)
		go func() {
			conn, err := ln.Accept()
			if err == nil {
				accepted <- conn
			}
			close(accepted)
		}()
		s := newSite(t)
		s.env["PISATI_SERVER_INTERNAL_LISTEN"] = fmt.Sprintf("127.0.0.1:%d", portOf(t, ln))
		halt, wait := s.halting(t, "health")
		time.Sleep(500 * time.Millisecond)
		halting := time.Now()
		halt()
		r := wait()
		if took := time.Since(halting); r.code != 1 || r.line(t) != line || took >= time.Second {
			t.Errorf("pisati health halted = %d, %q after %v; want 1, %q at once", r.code, r.stderr, took, line)
		}
		if err := ln.Close(); err != nil {
			t.Error(err)
		}
		for conn := range accepted {
			if err := conn.Close(); err != nil {
				t.Error(err)
			}
		}
		settle(t)
	})
}

func TestDirectoryVariablesAmongOtherProblems(t *testing.T) {
	// A bad directory variable is reported with every other problem. With
	// no configuration directory read, no setting is reported missing, as
	// its files may hold it; with the credentials alone unreadable, the
	// configuration's own problems are all there.
	for _, tc := range []struct {
		name    string
		missing bool
	}{
		{"CONFIGURATION_DIRECTORY", false},
		{"CREDENTIALS_DIRECTORY", true},
	} {
		s := newSite(t)
		s.env[tc.name] = "relative"
		s.env["PISATI_BOGUS"] = "1"
		delete(s.env, "PISATI_SERVER_BASE_URL")
		r := s.run(t, "migrate")
		lines := r.lines(t)
		want := 2
		if tc.missing {
			want = 3
		}
		if r.code != 1 || len(lines) != want {
			t.Fatalf("%s bad, PISATI_BOGUS set, no base URL: pisati migrate = %d, %q; want 1 and %d lines", tc.name, r.code, r.stderr, want)
		}
		text := r.stderr
		if !strings.Contains(text, "pisati: "+tc.name+` is set to "relative"`) || !strings.Contains(text, "pisati: PISATI_BOGUS: unknown setting.") {
			t.Errorf("%s bad: the lines are %q, want the variable and PISATI_BOGUS each named", tc.name, lines)
		}
		if named := strings.Contains(text, "server.base_url"); named != tc.missing {
			t.Errorf("%s bad: server.base_url reported missing: %t, want %t:\n%s", tc.name, named, tc.missing, text)
		}
	}
}

func TestCommandStoppedBySignal(t *testing.T) {
	s := newSite(t)
	h, stdout, stderr := s.host("migrate")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	code := run(ctx, h)
	r := result{code: code, stdout: stdout.String(), stderr: stderr.String()}
	want := "pisati: the command stopped before it finished, because a SIGTERM or SIGINT signal arrived."
	if r.code != 1 || r.line(t) != want || r.stdout != "" {
		t.Errorf("pisati migrate stopped by its context = %d, %q, %q; want 1, %q", r.code, r.stdout, r.stderr, want)
	}
}
