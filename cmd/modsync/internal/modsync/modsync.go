// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

// Package modsync keeps test/acceptance, the acceptance suite's module, in
// step with the repository's root module. test/acceptance replaces
// github.com/divbast/ci with the checkout just setup makes, which
// Renovate's clone lacks, so Renovate leaves the module alone: after each
// Go update of the root module it runs cmd/modsync instead, which runs
// this package.
//
// For every module both go.mod files require, directly or not, modsync
// gives test/acceptance the root's version. In test/acceptance's go.sum it
// drops the module's lines for the version test/acceptance required, and
// copies in the root go.sum's lines for the root's version: the hash of
// the module's files and the hash of its go.mod. It reads the two go.mod
// files, through go mod edit, and the two go.sum files, and nothing else:
// no network, and no checkout of ci.
//
// A module either go.mod replaces with a directory, at the version that
// go.mod requires or at every version, is left alone, as
// github.com/divbast/ci is. So is a module test/acceptance alone requires
// indirectly: those come through ci, and move with ci's pin. These are
// errors: a module test/acceptance alone requires directly, which has no
// version to follow; one either go.mod replaces with another module; one
// whose root version test/acceptance excludes, since go would then refuse
// the version modsync wrote; one whose root version test/acceptance
// replaces, since go would then build test/acceptance from the
// replacement rather than from the module the root requires; and one
// test/acceptance requires at a newer version than the root, which comes
// through ci and must not move back: the root module moves up instead.
package modsync

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"slices"
	"strings"
)

// The files modsync reads, from the repository's root. It writes the
// acceptance module's alone, each first to its temporary beside it, then
// renamed over it.
const (
	rootGoMod           = "go.mod"
	rootGoSum           = "go.sum"
	acceptanceGoMod     = "test/acceptance/go.mod"
	acceptanceGoSum     = "test/acceptance/go.sum"
	acceptanceGoModTemp = "test/acceptance/go.mod.modsync"
	acceptanceGoSumTemp = "test/acceptance/go.sum.modsync"
)

// The exit codes Run returns.
const (
	exitOK     = 0
	exitFailed = 1
	exitUsage  = 2
)

// Run runs modsync's command line, args, given without the command's
// name, in repo, the repository opened at its root, with goCmd, the go
// command that reads and edits the go.mod files, which NewGo makes.
//
// With no arguments it brings test/acceptance in step with the root
// module, writing test/acceptance's go.mod and go.sum only when they must
// change. With -check it writes nothing, and names each difference it
// finds, or names each temporary that a stopped run left. Each difference
// and each error is one line on stderr, starting "modsync: ". Nothing is
// written when there is nothing to report.
//
// It returns the exit code: 0 when test/acceptance is in step, or has been
// brought in step; 1 when it is not, under -check, or when modsync cannot
// do its work; and 2 when the command line is wrong.
func Run(ctx context.Context, args []string, repo *os.Root, goCmd Go, stderr io.Writer) int {
	flags := flag.NewFlagSet("modsync", flag.ContinueOnError)
	flags.SetOutput(stderr)
	check := flags.Bool("check", false, "write nothing, and exit 1 naming each difference, unless test/acceptance is in step")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if flags.NArg() > 0 {
		say(stderr, fmt.Sprintf("it takes no arguments, but was given %q", flags.Arg(0)))
		flags.Usage()
		return exitUsage
	}
	if goCmd.path == "" {
		say(stderr, "no go command was given")
		return exitFailed
	}
	s, err := inspect(ctx, repo, goCmd)
	if err != nil {
		say(stderr, strings.Split(err.Error(), "\n")...)
		return exitFailed
	}
	if *check {
		if err := leftovers(repo); err != nil {
			say(stderr, strings.Split(err.Error(), "\n")...)
			return exitFailed
		}
		if len(s.differences) == 0 {
			return exitOK
		}
		report := make([]string, 0, len(s.differences)+1)
		for _, d := range s.differences {
			report = append(report, d.String())
		}
		say(stderr, append(report, "test/acceptance is not in step with the root module: go run ./cmd/modsync brings it in step")...)
		return exitFailed
	}
	if err := apply(ctx, repo, goCmd, s); err != nil {
		say(stderr, strings.Split(err.Error(), "\n")...)
		return exitFailed
	}
	return exitOK
}

// say writes each of lines to w, each after "modsync: ".
func say(w io.Writer, lines ...string) {
	var b strings.Builder
	for _, line := range lines {
		b.WriteString("modsync: " + line + "\n")
	}
	io.WriteString(w, b.String()) //nolint:errcheck // w is stderr, and when it fails there is nowhere left to say so
}

// inspect reads the repository's two modules, and returns what bringing
// test/acceptance in step takes. Each file it cannot read is reported.
func inspect(ctx context.Context, repo *os.Root, goCmd Go) (steps, error) {
	root, rootErr := readMod(ctx, repo, goCmd, rootGoMod)
	acceptance, acceptanceErr := readMod(ctx, repo, goCmd, acceptanceGoMod)
	rootSum, rootSumErr := readSum(ctx, repo, rootGoSum)
	acceptanceSum, acceptanceSumErr := readSum(ctx, repo, acceptanceGoSum)
	if err := errors.Join(rootErr, acceptanceErr, rootSumErr, acceptanceSumErr); err != nil {
		return steps{}, err
	}
	return plan(root, acceptance, rootSum, acceptanceSum)
}

// readSum returns the lines of the go.sum name, relative to the
// repository's root.
func readSum(ctx context.Context, repo *os.Root, name string) ([]SumLine, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	f, err := repo.Open(name)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, maxOutput+1))
	if err := errors.Join(err, f.Close()); err != nil {
		return nil, err
	}
	if len(data) > maxOutput {
		return nil, fmt.Errorf("%s: larger than %d bytes", name, maxOutput)
	}
	return parseSum(name, data)
}

// apply brings test/acceptance in step as s says, and writes nothing when
// s holds no difference. go mod edit -print gives go.mod with each version
// s holds set; apply writes it and go.sum's lines each to its temporary,
// syncs and closes both, and only then renames them over the two files:
// go.sum first, then go.mod. A run stopped between the renames leaves
// go.mod naming the old versions, so the next run finds them and brings
// test/acceptance in step; one stopped sooner changes neither file. Any
// failure removes the temporaries apply made.
func apply(ctx context.Context, repo *os.Root, goCmd Go, s steps) error {
	if len(s.differences) == 0 {
		return nil
	}
	goMod, err := edited(ctx, repo, goCmd, acceptanceGoMod, s.versions)
	if err != nil {
		return err
	}
	if made, err := writeTemp(repo, acceptanceGoModTemp, acceptanceGoMod, goMod); err != nil {
		if made {
			return errors.Join(err, remove(repo, acceptanceGoModTemp))
		}
		return err
	}
	if made, err := writeTemp(repo, acceptanceGoSumTemp, acceptanceGoSum, formatSum(s.sum)); err != nil {
		if made {
			return errors.Join(err, remove(repo, acceptanceGoModTemp, acceptanceGoSumTemp))
		}
		return errors.Join(err, remove(repo, acceptanceGoModTemp))
	}
	if err := repo.Rename(acceptanceGoSumTemp, acceptanceGoSum); err != nil {
		return errors.Join(err, remove(repo, acceptanceGoModTemp, acceptanceGoSumTemp))
	}
	if err := repo.Rename(acceptanceGoModTemp, acceptanceGoMod); err != nil {
		return errors.Join(err, remove(repo, acceptanceGoModTemp))
	}
	return nil
}

// writeTemp writes data to the temporary temp, which it makes new, with
// the mode of final, the file it will be renamed over, then syncs and
// closes it. It reports whether it made temp, which then holds no more
// than part of data when the error is not nil. A temp already there is an
// error that names it and says what to do, and is left as it is.
func writeTemp(repo *os.Root, temp, final string, data []byte) (bool, error) {
	info, err := repo.Stat(final)
	if err != nil {
		return false, err
	}
	f, err := repo.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if errors.Is(err, fs.ErrExist) {
		return false, leftover(temp)
	}
	if err != nil {
		return false, err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	return true, errors.Join(err, f.Close())
}

// leftovers returns an error naming each of apply's temporaries that is
// there already: a run that was stopped can leave one, and -check, which
// writes nothing, says so as a run that writes would.
func leftovers(repo *os.Root) error {
	var errs []error
	for _, temp := range []string{acceptanceGoModTemp, acceptanceGoSumTemp} {
		_, err := repo.Lstat(temp)
		switch {
		case err == nil:
			errs = append(errs, leftover(temp))
		case !errors.Is(err, fs.ErrNotExist):
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// leftover is the error for temp, one of apply's temporaries, which is
// there already.
func leftover(temp string) error {
	return fmt.Errorf("%s is already there, perhaps left by a run of modsync that was stopped: delete it, then run modsync again", temp)
}

// remove removes each of the temporaries names, which apply made, and
// reports each it could not.
func remove(repo *os.Root, names ...string) error {
	errs := make([]error, 0, len(names))
	for _, name := range names {
		errs = append(errs, repo.Remove(name))
	}
	return errors.Join(errs...)
}

// steps is what bringing test/acceptance in step takes: each difference,
// in the order of the modules' paths; the version to set for each module
// whose version differs, by path; and test/acceptance's go.sum lines as
// they must be.
type steps struct {
	differences []difference
	versions    map[modPath]modVersion
	sum         []SumLine
}

// plan returns what bringing acceptance, test/acceptance's go.mod, and
// acceptanceSum, its go.sum, in step with root and rootSum takes. Each
// module modsync cannot keep in step is an error, and all are reported.
func plan(root, acceptance modFile, rootSum, acceptanceSum []SumLine) (steps, error) {
	s := steps{versions: map[modPath]modVersion{}, sum: slices.Clone(acceptanceSum)}
	var errs []error
	for _, path := range slices.SortedFunc(maps.Keys(acceptance.requires), modPath.compare) {
		req := acceptance.requires[path]
		rootReq, shared := root.requires[path]
		with, replaced := acceptance.replacedWith(path, req.version)
		rootWith, rootReplaced := root.replacedWith(path, rootReq.version)
		rootReplaced = shared && rootReplaced
		_, replacesRoot := acceptance.replaces[module{path: path, version: rootReq.version}]
		switch {
		case replaced && with == byDirectory, rootReplaced && rootWith == byDirectory:
			continue
		case !shared && req.indirect:
			continue
		case !shared:
			errs = append(errs, fmt.Errorf("%s: %s requires it directly, but the root module does not require it, so there is no version to copy", path, acceptanceGoMod))
			continue
		case replaced:
			errs = append(errs, replacedByModule(path, acceptanceGoMod))
			continue
		case rootReplaced:
			errs = append(errs, replacedByModule(path, rootGoMod))
			continue
		case acceptance.excludes[module{path: path, version: rootReq.version}]:
			errs = append(errs, fmt.Errorf("%s %s: %s excludes the version the root module requires, so test/acceptance cannot follow it", path, rootReq.version, acceptanceGoMod))
			continue
		case replacesRoot:
			errs = append(errs, fmt.Errorf("%s %s: %s replaces the version the root module requires, so test/acceptance cannot follow it", path, rootReq.version, acceptanceGoMod))
			continue
		}
		if err := notAhead(path, req.version, rootReq.version); err != nil {
			errs = append(errs, err)
			continue
		}
		lines, err := linesAt(rootSum, path, rootReq.version)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if req.version != rootReq.version {
			s.differences = append(s.differences, versionDiff{path: path, acceptance: req.version, root: rootReq.version})
			s.versions[path] = rootReq.version
		}
		next := swap(s.sum, path, []modVersion{req.version, rootReq.version}, lines)
		if !slices.Equal(linesOf(s.sum, path), linesOf(next, path)) {
			s.differences = append(s.differences, sumDiff{path: path, version: rootReq.version})
		}
		s.sum = next
	}
	if err := errors.Join(errs...); err != nil {
		return steps{}, err
	}
	return s, nil
}

// replacedByModule is the error for path, which the go.mod name replaces
// with another module.
func replacedByModule(path modPath, name string) error {
	return fmt.Errorf("%s: %s replaces it with another module, so its version cannot follow the root module's", path, name)
}

// notAhead returns an error unless test/acceptance's version of path,
// acceptance, is the root's or older. A newer one comes through ci, and
// moving it back would give ci less than it requires, so the root module
// must move up instead.
func notAhead(path modPath, acceptance, root modVersion) error {
	if compareVersions(acceptance, root) <= 0 {
		return nil
	}
	return fmt.Errorf("%s: %s requires %s, which is newer than the root module's %s, and modsync never moves a module back. Bump the root module instead: run go get %s@%s in the repository's root", path, acceptanceGoMod, acceptance, root, path, acceptance)
}

// linesAt returns sum's lines for path at version, each once, which must
// include the hash of the module's files and the hash of its go.mod.
func linesAt(sum []SumLine, path modPath, version modVersion) ([]SumLine, error) {
	var lines []SumLine
	var files, goMod bool
	for _, l := range sum {
		if l.path != path || l.version != version || slices.Contains(lines, l) {
			continue
		}
		lines = append(lines, l)
		switch l.covers {
		case coversFiles:
			files = true
		case coversGoMod:
			goMod = true
		}
	}
	switch {
	case !files:
		return nil, fmt.Errorf("%s %s: the root go.sum has no hash of the module's files to copy", path, version)
	case !goMod:
		return nil, fmt.Errorf("%s %s: the root go.sum has no hash of the module's go.mod to copy", path, version)
	}
	return lines, nil
}

// swap returns a copy of sum without path's lines for any of versions,
// and with lines, which are path's, after path's other lines. The go
// command orders a go.sum by path, then by version. A tidy go.sum keeps
// lines for other versions of a path only where another module of the
// graph requires them, and the version a go.mod requires is the one
// minimal version selection chose from them all, the greatest. So the
// root's version goes after the others, and no version need be compared
// here: a root version below one of them would leave test/acceptance
// requiring less than its graph needs, which go mod tidy -diff reports.
func swap(sum []SumLine, path modPath, versions []modVersion, lines []SumLine) []SumLine {
	out := make([]SumLine, 0, len(sum)+len(lines))
	placed := false
	for _, l := range sum {
		if l.path == path && slices.Contains(versions, l.version) {
			continue
		}
		if !placed && l.path.compare(path) > 0 {
			out = append(out, lines...)
			placed = true
		}
		out = append(out, l)
	}
	if !placed {
		out = append(out, lines...)
	}
	return out
}

// linesOf returns sum's lines for path, in order.
func linesOf(sum []SumLine, path modPath) []SumLine {
	var lines []SumLine
	for _, l := range sum {
		if l.path == path {
			lines = append(lines, l)
		}
	}
	return lines
}

// difference is one way test/acceptance is out of step with the root
// module, as -check names it.
//
//sumtype:decl
type difference interface {
	fmt.Stringer
	difference()
}

// versionDiff is a module test/acceptance requires at another version than
// the root module does.
type versionDiff struct {
	path       modPath
	acceptance modVersion
	root       modVersion
}

func (versionDiff) difference() {}

func (d versionDiff) String() string {
	return fmt.Sprintf("%s: %s requires %s, and the root module requires %s", d.path, acceptanceGoMod, d.acceptance, d.root)
}

// sumDiff is a module whose lines in test/acceptance's go.sum are not the
// root go.sum's for the root's version.
type sumDiff struct {
	path    modPath
	version modVersion
}

func (sumDiff) difference() {}

func (d sumDiff) String() string {
	return fmt.Sprintf("%s: %s's lines for %s are not the root go.sum's", d.path, acceptanceGoSum, d.version)
}
