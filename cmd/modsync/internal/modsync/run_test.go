// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package modsync_test

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/divbast/pisati/cmd/modsync/internal/modsync"
)

// The four files modsync reads, from the repository's root. It writes
// only the last two.
var files = []string{"go.mod", "go.sum", "test/acceptance/go.mod", "test/acceptance/go.sum"}

// outOfStep is the line -check ends its report of differences with.
const outOfStep = "modsync: test/acceptance is not in step with the root module: go run ./cmd/modsync brings it in step\n"

// TestRun runs modsync over each repository under testdata, first with
// -check, then without, then with -check again. Each case's in holds the
// repository; its want, when there is one, holds test/acceptance as
// modsync must leave it. -check writes nothing, whatever it finds. A
// case with a want must be brought in step, and then be found in step,
// and a second run must change nothing. A case without one must be left
// as it was, and every run must report the same.
func TestRun(t *testing.T) {
	tests := []struct {
		name string
		// check is -check's exit code.
		check int
		// report is what -check writes to stderr, and the run without
		// -check too when the case has no want.
		report string
	}{
		{
			name: "in-step",
		},
		{
			name:  "out-of-step",
			check: 1,
			report: "modsync: go.uber.org/goleak: test/acceptance/go.mod requires v1.3.0, and the root module requires v1.3.1\n" +
				"modsync: go.uber.org/goleak: test/acceptance/go.sum's lines for v1.3.1 are not the root go.sum's\n" +
				"modsync: golang.org/x/sys: test/acceptance/go.mod requires v0.49.0, and the root module requires v0.50.0\n" +
				"modsync: golang.org/x/sys: test/acceptance/go.sum's lines for v0.50.0 are not the root go.sum's\n" +
				outOfStep,
		},
		{
			name:  "sums-only",
			check: 1,
			report: "modsync: go.uber.org/goleak: test/acceptance/go.sum's lines for v1.3.0 are not the root go.sum's\n" +
				"modsync: golang.org/x/sys: test/acceptance/go.sum's lines for v0.50.0 are not the root go.sum's\n" +
				outOfStep,
		},
		{
			name:  "direct-not-in-root",
			check: 1,
			report: "modsync: example.com/extra: test/acceptance/go.mod requires it directly, but the root module does not require it, so there is no version to copy\n" +
				"modsync: example.com/more: test/acceptance/go.mod requires it directly, but the root module does not require it, so there is no version to copy\n",
		},
		{
			name:  "root-sum-missing",
			check: 1,
			report: "modsync: go.uber.org/goleak v1.3.1: the root go.sum has no hash of the module's files to copy\n" +
				"modsync: golang.org/x/sys v0.50.0: the root go.sum has no hash of the module's go.mod to copy\n",
		},
		{
			name:  "replaced",
			check: 1,
			report: "modsync: go.uber.org/goleak: test/acceptance/go.mod replaces it with another module, so its version cannot follow the root module's\n" +
				"modsync: golang.org/x/sys: go.mod replaces it with another module, so its version cannot follow the root module's\n",
		},
		{
			name:  "malformed-sum",
			check: 1,
			report: "modsync: test/acceptance/go.sum:2: 2 fields, but a go.sum line has 3\n" +
				"modsync: test/acceptance/go.sum:4: \"1.3.0/go.mod\" is not a module version\n",
		},
		{
			// goleak's replace covers a version test/acceptance does not
			// require, so goleak follows the root; x/sys's covers the one
			// it requires, so x/sys is left alone.
			name:  "replace-version",
			check: 1,
			report: "modsync: go.uber.org/goleak: test/acceptance/go.mod requires v1.3.0, and the root module requires v1.3.1\n" +
				"modsync: go.uber.org/goleak: test/acceptance/go.sum's lines for v1.3.1 are not the root go.sum's\n" +
				outOfStep,
		},
		{
			// test/acceptance excludes goleak's root version, and
			// replaces x/sys's root version, though not the version it
			// requires: modsync would write a version go never uses.
			name:  "root-version-blocked",
			check: 1,
			report: "modsync: go.uber.org/goleak v1.3.1: test/acceptance/go.mod excludes the version the root module requires, so test/acceptance cannot follow it\n" +
				"modsync: golang.org/x/sys v0.50.0: test/acceptance/go.mod replaces the version the root module requires, so test/acceptance cannot follow it\n",
		},
		{
			// test/acceptance requires goleak, and x/sys at a
			// pre-release, newer than the root does: modsync refuses to
			// move either back.
			name:  "ahead-of-root",
			check: 1,
			report: "modsync: go.uber.org/goleak: test/acceptance/go.mod requires v1.3.1, which is newer than the root module's v1.3.0, and modsync never moves a module back. Bump the root module instead: run go get go.uber.org/goleak@v1.3.1 in the repository's root\n" +
				"modsync: golang.org/x/sys: test/acceptance/go.mod requires v0.51.0-0.20261101000000-abcdefabcdef, which is newer than the root module's v0.50.0, and modsync never moves a module back. Bump the root module instead: run go get golang.org/x/sys@v0.51.0-0.20261101000000-abcdefabcdef in the repository's root\n",
		},
		{
			// The root replaces goleak with a directory at every
			// version, so test/acceptance's goleak is left alone.
			name: "root-replaces-all",
		},
		{
			// The root replaces x/sys with a directory at the version
			// it requires, so x/sys is left alone, and x/text at another
			// version, so x/text follows the root.
			name:  "root-replaces-one",
			check: 1,
			report: "modsync: golang.org/x/text: test/acceptance/go.mod requires v0.43.0, and the root module requires v0.44.0\n" +
				"modsync: golang.org/x/text: test/acceptance/go.sum's lines for v0.44.0 are not the root go.sum's\n" +
				outOfStep,
		},
		{
			// The root's go.sum holds each goleak line twice;
			// test/acceptance gets each once.
			name:  "root-sum-duplicates",
			check: 1,
			report: "modsync: go.uber.org/goleak: test/acceptance/go.mod requires v1.3.0, and the root module requires v1.3.1\n" +
				"modsync: go.uber.org/goleak: test/acceptance/go.sum's lines for v1.3.1 are not the root go.sum's\n" +
				outOfStep,
		},
		{
			// What a run stopped between apply's two renames leaves:
			// go.sum is new, go.mod still names the old version.
			name:   "stopped-between-renames",
			check:  1,
			report: "modsync: go.uber.org/goleak: test/acceptance/go.mod requires v1.3.0, and the root module requires v1.3.1\n" + outOfStep,
		},
		{
			name:  "no-acceptance",
			check: 1,
			report: "modsync: go mod edit -json test/acceptance/go.mod: exit status 1: go: open test/acceptance/go.mod: no such file or directory\n" +
				"modsync: openat test/acceptance/go.sum: no such file or directory\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := filepath.Join("testdata", tt.name, "in")
			want := filepath.Join("testdata", tt.name, "want")
			_, err := os.Stat(want)
			synced := err == nil
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				t.Fatal(err)
			}
			repo, dir := copyRepo(t, in)

			code, report := run(t, repo, "-check")
			if code != tt.check || report != tt.report {
				t.Errorf("-check: exit %d, and wrote:\n%s\nwant exit %d, and:\n%s", code, report, tt.check, tt.report)
			}
			sameFiles(t, "-check", dir, in, files)

			code, report = run(t, repo)
			if !synced {
				if code != tt.check || report != tt.report {
					t.Errorf("without -check: exit %d, and wrote:\n%s\nwant exit %d, and:\n%s", code, report, tt.check, tt.report)
				}
				sameFiles(t, "without -check", dir, in, files)
				return
			}
			if code != 0 || report != "" {
				t.Fatalf("without -check: exit %d, and wrote:\n%s\nwant exit 0, and nothing", code, report)
			}
			sameFiles(t, "without -check", dir, in, files[:2])
			sameFiles(t, "without -check", dir, want, files[2:])

			if code, report := run(t, repo, "-check"); code != 0 || report != "" {
				t.Errorf("-check after modsync: exit %d, and wrote:\n%s\nwant exit 0, and nothing", code, report)
			}
			if code, report := run(t, repo); code != 0 || report != "" {
				t.Errorf("modsync again: exit %d, and wrote:\n%s\nwant exit 0, and nothing", code, report)
			}
			sameFiles(t, "modsync again", dir, want, files[2:])
		})
	}
}

// TestRunCommandLine checks that a command line modsync does not take
// exits 2, naming what is wrong, and changes nothing, and that -h exits 0.
func TestRunCommandLine(t *testing.T) {
	tests := []struct {
		name string
		args []string
		code int
		// report is the start of what modsync writes.
		report string
	}{
		{name: "a flag modsync does not have", args: []string{"-write"}, code: 2, report: "flag provided but not defined: -write\nUsage of modsync:\n"},
		{name: "an argument", args: []string{"test/acceptance"}, code: 2, report: "modsync: it takes no arguments, but was given \"test/acceptance\"\nUsage of modsync:\n"},
		{name: "-check with a value", args: []string{"-check=maybe"}, code: 2, report: "invalid boolean value \"maybe\" for -check: parse error\nUsage of modsync:\n"},
		{name: "help", args: []string{"-h"}, code: 0, report: "Usage of modsync:\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := filepath.Join("testdata", "out-of-step", "in")
			repo, dir := copyRepo(t, in)
			code, report := run(t, repo, tt.args...)
			if code != tt.code || !strings.HasPrefix(report, tt.report) {
				t.Errorf("modsync %s: exit %d, and wrote:\n%s\nwant exit %d, and a report that starts:\n%s", strings.Join(tt.args, " "), code, report, tt.code, tt.report)
			}
			if !strings.Contains(report, "-check") {
				t.Errorf("modsync %s wrote no usage naming -check:\n%s", strings.Join(tt.args, " "), report)
			}
			sameFiles(t, strings.Join(tt.args, " "), dir, in, files)
		})
	}
}

// TestRunReadsGoModStrictly runs modsync with a go command that prints,
// for go mod edit -json, what the real one never does, and checks that
// modsync refuses it, for each go.mod, naming why. The repository's
// go.sum files are empty, so they are not the cause.
func TestRunReadsGoModStrictly(t *testing.T) {
	tests := []struct {
		name   string
		json   string
		report string
	}{
		{name: "not JSON", json: `{"Require": [`, report: "unexpected EOF"},
		{name: "two values", json: `{} {}`, report: "more than one JSON value"},
		{name: "a requirement without a path", json: `{"Require": [{"Version": "v1.0.0"}]}`, report: `"" is not a module path`},
		{name: "a requirement on a branch", json: `{"Require": [{"Path": "example.com/a", "Version": "main"}]}`, report: `example.com/a: "main" is not a module version`},
		{name: "a module required twice", json: `{"Require": [{"Path": "example.com/a", "Version": "v1.0.0"}, {"Path": "example.com/a", "Version": "v1.1.0"}]}`, report: "example.com/a: required twice"},
		{name: "a replace without a path", json: `{"Replace": [{"Old": {}, "New": {"Path": "../a"}}]}`, report: `a replace: "" is not a module path`},
		{name: "a replace with nothing to replace it with", json: `{"Replace": [{"Old": {"Path": "example.com/a"}, "New": {}}]}`, report: "example.com/a: replaced with nothing"},
		{name: "a replace of a version that is not one", json: `{"Replace": [{"Old": {"Path": "example.com/a", "Version": "main"}, "New": {"Path": "../a"}}]}`, report: `example.com/a: "main" is not a module version, in a replace`},
		{name: "a replacement module at a version that is not one", json: `{"Replace": [{"Old": {"Path": "example.com/a"}, "New": {"Path": "example.com/b", "Version": "main"}}]}`, report: `example.com/a: "main" is not a module version, in a replace`},
		{name: "an exclude without a path", json: `{"Exclude": [{"Version": "v1.0.0"}]}`, report: `an exclude: "" is not a module path`},
		{name: "an exclude without a version", json: `{"Exclude": [{"Path": "example.com/a"}]}`, report: `example.com/a: "" is not a module version, in an exclude`},
		{name: "a version cut short", json: `{"Require": [{"Path": "example.com/a", "Version": "v1.0"}]}`, report: `example.com/a: "v1.0" is not a module version`},
		{name: "a value of the wrong type", json: `{"Require": {"Path": "example.com/a"}}`, report: "cannot unmarshal object into Go struct field"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, name := range []string{"go.sum", "test/acceptance/go.sum"} {
				if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(name)), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			repo := openRepo(t, dir)
			goCmd := goCommand(t, fakeGo(t, "cat <<'EOF'\n"+tt.json+"\nEOF\n"), time.Minute)
			var stderr bytes.Buffer
			code := modsync.Run(t.Context(), []string{"-check"}, repo, goCmd, &stderr)
			want := []string{
				"modsync: go.mod: go mod edit -json printed what modsync cannot read: ",
				"modsync: test/acceptance/go.mod: go mod edit -json printed what modsync cannot read: ",
			}
			lines := strings.Split(strings.TrimSuffix(stderr.String(), "\n"), "\n")
			if code != 1 || len(lines) != len(want) {
				t.Fatalf("exit %d, and wrote:\n%s\nwant exit 1, and one line for each go.mod", code, stderr.String())
			}
			for i, line := range lines {
				if !strings.HasPrefix(line, want[i]) || !strings.Contains(line, tt.report) {
					t.Errorf("line %d is %q, want it to start %q and say %q", i+1, line, want[i], tt.report)
				}
			}
		})
	}
}

// TestRunBoundsGoOutput checks that modsync stops reading what go mod edit
// prints at 1 MiB, and fails then rather than read a part of it.
func TestRunBoundsGoOutput(t *testing.T) {
	dir := t.TempDir()
	repo := openRepo(t, dir)
	goCmd := goCommand(t, fakeGo(t, "head -c 1048577 /dev/zero\n"), time.Minute)
	var stderr bytes.Buffer
	code := modsync.Run(t.Context(), []string{"-check"}, repo, goCmd, &stderr)
	want := "modsync: go mod edit -json go.mod: printed more than 1048576 bytes\n"
	if code != 1 || !strings.HasPrefix(stderr.String(), want) {
		t.Fatalf("exit %d, and wrote:\n%s\nwant exit 1, and a report that starts:\n%s", code, stderr.String(), want)
	}
}

// TestRunBoundsGoSum checks that modsync reads a go.sum of 1 MiB at most,
// and fails on a larger one rather than read a part of it.
func TestRunBoundsGoSum(t *testing.T) {
	in := filepath.Join("testdata", "in-step", "in")
	repo, dir := copyRepo(t, in)
	if err := os.WriteFile(filepath.Join(dir, "go.sum"), bytes.Repeat([]byte("\n"), 1<<20+1), 0o600); err != nil {
		t.Fatal(err)
	}
	want := "modsync: go.sum: larger than 1048576 bytes\n"
	if code, report := run(t, repo, "-check"); code != 1 || report != want {
		t.Fatalf("exit %d, and wrote:\n%s\nwant exit 1, and:\n%s", code, report, want)
	}
}

// TestRunEditFails runs modsync where go mod edit can read each go.mod
// but cannot edit test/acceptance's, and checks that modsync fails, with
// go's reason, and leaves test/acceptance's go.sum as it was, in step
// with its go.mod still.
func TestRunEditFails(t *testing.T) {
	in := filepath.Join("testdata", "out-of-step", "in")
	repo, dir := copyRepo(t, in)
	realGo, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	goCmd := goCommand(t, fakeGo(t, `case "$*" in
*-json*) exec '`+realGo+`' "$@" ;;
*) echo "go: test/acceptance/go.mod: permission denied" >&2; exit 1 ;;
esac
`), time.Minute)
	var stderr bytes.Buffer
	code := modsync.Run(t.Context(), nil, repo, goCmd, &stderr)
	want := "modsync: go mod edit -print -require=go.uber.org/goleak@v1.3.1 -require=golang.org/x/sys@v0.50.0 test/acceptance/go.mod: exit status 1: go: test/acceptance/go.mod: permission denied\n"
	if code != 1 || stderr.String() != want {
		t.Errorf("exit %d, and wrote:\n%s\nwant exit 1, and:\n%s", code, stderr.String(), want)
	}
	sameFiles(t, "a failed edit", dir, in, files)
}

// TestRunCancelled checks that modsync, given a context already
// cancelled, reads nothing, writes nothing, and says why for each file.
func TestRunCancelled(t *testing.T) {
	in := filepath.Join("testdata", "out-of-step", "in")
	repo, dir := copyRepo(t, in)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, args := range [][]string{{"-check"}, nil} {
		var stderr bytes.Buffer
		code := modsync.Run(ctx, args, repo, goOnPath(t), &stderr)
		want := "modsync: go mod edit -json go.mod: context canceled\n" +
			"modsync: go mod edit -json test/acceptance/go.mod: context canceled\n" +
			"modsync: go.sum: context canceled\n" +
			"modsync: test/acceptance/go.sum: context canceled\n"
		if code != 1 || stderr.String() != want {
			t.Errorf("modsync %s: exit %d, and wrote:\n%s\nwant exit 1, and:\n%s", strings.Join(args, " "), code, stderr.String(), want)
		}
		sameFiles(t, "a cancelled run", dir, in, files)
	}
}

// TestRunTimesOut runs modsync with a go command that takes longer than
// its timeout, and checks that modsync stops it and fails, naming the
// timeout, for each go.mod, and writes nothing.
func TestRunTimesOut(t *testing.T) {
	in := filepath.Join("testdata", "out-of-step", "in")
	repo, dir := copyRepo(t, in)
	goCmd := goCommand(t, fakeGo(t, "exec sleep 5\n"), 100*time.Millisecond)
	for _, args := range [][]string{{"-check"}, nil} {
		var stderr bytes.Buffer
		code := modsync.Run(t.Context(), args, repo, goCmd, &stderr)
		want := "modsync: go mod edit -json go.mod: go did not finish within 100ms\n" +
			"modsync: go mod edit -json test/acceptance/go.mod: go did not finish within 100ms\n"
		if code != 1 || stderr.String() != want {
			t.Errorf("modsync %s: exit %d, and wrote:\n%s\nwant exit 1, and:\n%s", strings.Join(args, " "), code, stderr.String(), want)
		}
		sameFiles(t, "a run that timed out", dir, in, files)
	}
}

// TestRunGoFailsSilently checks that a go command that fails and prints
// nothing is reported by its exit status alone, for each go.mod.
func TestRunGoFailsSilently(t *testing.T) {
	in := filepath.Join("testdata", "out-of-step", "in")
	repo, _ := copyRepo(t, in)
	var stderr bytes.Buffer
	code := modsync.Run(t.Context(), []string{"-check"}, repo, goCommand(t, fakeGo(t, "exit 3\n"), time.Minute), &stderr)
	want := "modsync: go mod edit -json go.mod: exit status 3\n" +
		"modsync: go mod edit -json test/acceptance/go.mod: exit status 3\n"
	if code != 1 || stderr.String() != want {
		t.Errorf("exit %d, and wrote:\n%s\nwant exit 1, and:\n%s", code, stderr.String(), want)
	}
}

// TestNewGo checks that NewGo refuses a go command it could not run: one
// with no path, or a timeout that is not positive.
func TestNewGo(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		timeout time.Duration
		err     string
	}{
		{name: "an absolute path", path: "/usr/local/go/bin/go", timeout: time.Nanosecond},
		{name: "a name left for PATH", path: "go", timeout: time.Minute, err: `the go command's path must be absolute, not "go"`},
		{name: "a relative path", path: "bin/go", timeout: time.Minute, err: `the go command's path must be absolute, not "bin/go"`},
		{name: "no path", timeout: time.Minute, err: "no go command is named"},
		{name: "no timeout", path: "/usr/local/go/bin/go", err: "a go command's timeout must be positive, not 0s"},
		{name: "a negative timeout", path: "/usr/local/go/bin/go", timeout: -time.Second, err: "a go command's timeout must be positive, not -1s"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			goCmd, err := modsync.NewGo(tt.path, tt.timeout)
			switch {
			case tt.err == "" && err != nil:
				t.Fatalf("NewGo(%q, %s): %v", tt.path, tt.timeout, err)
			case tt.err == "" && goCmd == (modsync.Go{}):
				t.Fatalf("NewGo(%q, %s) returned the zero Go", tt.path, tt.timeout)
			case tt.err != "" && (err == nil || err.Error() != tt.err):
				t.Fatalf("NewGo(%q, %s): error %v, want %q", tt.path, tt.timeout, err, tt.err)
			case tt.err != "" && goCmd != (modsync.Go{}):
				t.Fatalf("NewGo(%q, %s) returned a Go beside its error, want the zero Go", tt.path, tt.timeout)
			}
		})
	}
}

// TestRunRefusesNoGo checks that Run, given the zero Go, fails, saying so,
// and writes nothing.
func TestRunRefusesNoGo(t *testing.T) {
	in := filepath.Join("testdata", "out-of-step", "in")
	repo, dir := copyRepo(t, in)
	var stderr bytes.Buffer
	code := modsync.Run(t.Context(), nil, repo, modsync.Go{}, &stderr)
	if want := "modsync: no go command was given\n"; code != 1 || stderr.String() != want {
		t.Errorf("exit %d, and wrote:\n%s\nwant exit 1, and:\n%s", code, stderr.String(), want)
	}
	sameFiles(t, "a run with no go command", dir, in, files)
}

// TestRunRecoversFromAFailedWrite makes test/acceptance a directory
// modsync cannot write in, and checks that modsync fails, leaves go.mod
// and go.sum as they were, and leaves no file of its own behind; then,
// with the directory writable again, that a second run brings
// test/acceptance in step and -check finds nothing.
func TestRunRecoversFromAFailedWrite(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes in a directory whatever its mode, so a read-only test/acceptance cannot make modsync's write fail")
	}
	in := filepath.Join("testdata", "out-of-step", "in")
	want := filepath.Join("testdata", "out-of-step", "want")
	repo, dir := copyRepo(t, in)
	acceptance := filepath.Join(dir, "test", "acceptance")
	//nolint:gosec // G302: a directory its owner may list and enter but not write in, which is the point of the test
	if err := os.Chmod(acceptance, 0o500); err != nil {
		t.Fatal(err)
	}
	// TempDir's removal needs the directory writable again.
	t.Cleanup(func() {
		//nolint:gosec // G302: a directory, which its owner must be able to list and enter
		if err := os.Chmod(acceptance, 0o700); err != nil {
			t.Error(err)
		}
	})
	if code, _ := run(t, repo); code == 0 {
		t.Error("modsync exited 0 in a directory it cannot write in")
	}
	sameFiles(t, "a failed write", dir, in, files)
	noTemps(t, dir)
	//nolint:gosec // G302: a directory, which its owner must be able to list and enter
	if err := os.Chmod(acceptance, 0o700); err != nil {
		t.Fatal(err)
	}
	if code, report := run(t, repo); code != 0 || report != "" {
		t.Errorf("the second run: exit %d, and wrote:\n%s\nwant exit 0, and nothing", code, report)
	}
	sameFiles(t, "a failed write and a second run", dir, want, files[2:])
	noTemps(t, dir)
	if code, report := run(t, repo, "-check"); code != 0 || report != "" {
		t.Errorf("-check after the second run: exit %d, and wrote:\n%s\nwant exit 0, and nothing", code, report)
	}
}

// noTemps fails the test when a file modsync writes before renaming it is
// in dir's test/acceptance.
func noTemps(t *testing.T, dir string) {
	t.Helper()
	left, err := filepath.Glob(filepath.Join(dir, "test", "acceptance", "*.modsync"))
	if err != nil {
		t.Fatal(err)
	}
	if len(left) > 0 {
		t.Errorf("modsync left %q", left)
	}
}

// TestRunCheckRefusesLeftovers checks that -check, which writes nothing,
// fails all the same on a file a stopped run of modsync could leave,
// naming it and how to go on, even with test/acceptance in step.
func TestRunCheckRefusesLeftovers(t *testing.T) {
	for _, leftover := range []string{"go.mod.modsync", "go.sum.modsync"} {
		t.Run(leftover, func(t *testing.T) {
			repo, dir := copyRepo(t, filepath.Join("testdata", "in-step", "in"))
			if err := os.WriteFile(filepath.Join(dir, "test", "acceptance", leftover), []byte("left over\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			code, report := run(t, repo, "-check")
			want := "modsync: test/acceptance/" + leftover + " is already there, perhaps left by a run of modsync that was stopped: delete it, then run modsync again\n"
			if code != 1 || report != want {
				t.Errorf("-check: exit %d, and wrote:\n%s\nwant exit 1, and:\n%s", code, report, want)
			}
		})
	}
}

// TestRunRefusesLeftovers puts in place a file modsync writes before it
// renames it, as a stopped run could leave one, and checks that modsync
// fails, naming the file and how to go on, changes neither go.mod nor
// go.sum, leaves the file as it was, and removes any file it made itself.
// Deleting the file then lets modsync finish.
func TestRunRefusesLeftovers(t *testing.T) {
	for _, leftover := range []string{"go.mod.modsync", "go.sum.modsync"} {
		t.Run(leftover, func(t *testing.T) {
			in := filepath.Join("testdata", "out-of-step", "in")
			repo, dir := copyRepo(t, in)
			path := filepath.Join(dir, "test", "acceptance", leftover)
			if err := os.WriteFile(path, []byte("left over\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			code, report := run(t, repo)
			want := "modsync: test/acceptance/" + leftover + " is already there, perhaps left by a run of modsync that was stopped: delete it, then run modsync again\n"
			if code != 1 || report != want {
				t.Errorf("exit %d, and wrote:\n%s\nwant exit 1, and:\n%s", code, report, want)
			}
			sameFiles(t, "a refused run", dir, in, files)
			if got, err := fs.ReadFile(os.DirFS(dir), "test/acceptance/"+leftover); err != nil || string(got) != "left over\n" {
				t.Errorf("the leftover is %q, %v, want it as it was", got, err)
			}
			for _, made := range []string{"go.mod.modsync", "go.sum.modsync"} {
				if made == leftover {
					continue
				}
				if _, err := os.Stat(filepath.Join(dir, "test", "acceptance", made)); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("test/acceptance/%s is there after the refused run: %v", made, err)
				}
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if code, report := run(t, repo); code != 0 || report != "" {
				t.Errorf("after the leftover is deleted: exit %d, and wrote:\n%s\nwant exit 0, and nothing", code, report)
			}
			sameFiles(t, "a run after the leftover is deleted", dir, filepath.Join("testdata", "out-of-step", "want"), files[2:])
		})
	}
}

// copyRepo copies the repository in to a new directory, and returns it
// opened as Run takes it, and its path.
func copyRepo(t *testing.T, in string) (*os.Root, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS(in)); err != nil {
		t.Fatal(err)
	}
	return openRepo(t, dir), dir
}

// openRepo opens dir as Run takes it, until the test ends.
func openRepo(t *testing.T, dir string) *os.Root {
	t.Helper()
	repo, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repo.Close(); err != nil {
			t.Error(err)
		}
	})
	return repo
}

// fakeGo writes a go command that runs script, whatever its arguments,
// and returns its path.
func fakeGo(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "go")
	//nolint:gosec // G306: the fake is a go command, which Run executes, so its owner must be able to run it
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// run runs modsync with args in repo, with the go command on PATH, and
// returns its exit code and what it wrote.
func run(t *testing.T, repo *os.Root, args ...string) (int, string) {
	t.Helper()
	var stderr bytes.Buffer
	code := modsync.Run(t.Context(), args, repo, goOnPath(t), &stderr)
	return code, stderr.String()
}

// goOnPath returns the go command on PATH, which go test puts first, as
// main finds it.
func goOnPath(t *testing.T) modsync.Go {
	t.Helper()
	path, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	return goCommand(t, path, time.Minute)
}

// goCommand returns the go command path, each run of which may take
// timeout.
func goCommand(t *testing.T, path string, timeout time.Duration) modsync.Go {
	t.Helper()
	goCmd, err := modsync.NewGo(path, timeout)
	if err != nil {
		t.Fatal(err)
	}
	return goCmd
}

// sameFiles fails the test, saying after what, unless each of names is the
// same in dir as in want, or missing from both.
func sameFiles(t *testing.T, after, dir, want string, names []string) {
	t.Helper()
	for _, name := range names {
		got, gotErr := fs.ReadFile(os.DirFS(dir), name)
		exp, wantErr := fs.ReadFile(os.DirFS(want), name)
		switch {
		case errors.Is(gotErr, fs.ErrNotExist) && errors.Is(wantErr, fs.ErrNotExist):
		case gotErr != nil || wantErr != nil:
			t.Errorf("after %s: %s: %v", after, name, errors.Join(gotErr, wantErr))
		case !bytes.Equal(got, exp):
			t.Errorf("after %s: %s is:\n%s\nwant:\n%s", after, name, got, exp)
		}
	}
}
