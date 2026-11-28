// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package modsync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	// maxOutput bounds what modsync reads of a go command's output, and
	// of a go.sum: 1 MiB, where Pisati's largest go.sum is under 12 KiB.
	maxOutput = 1 << 20
	// waitDelay bounds how long modsync waits, once a go command has been
	// stopped or has exited, for whatever it started to close its output.
	waitDelay = time.Second
)

// Go is the go command modsync runs to read and edit go.mod files, and
// how long each run of it may take. The zero Go is none, and Run refuses
// it.
type Go struct {
	path    string
	timeout time.Duration
}

// NewGo returns the go command at path, which must be absolute, as
// exec.LookPath finds it in main, so that nothing here reads PATH; each
// run of it may take timeout at most. An empty or relative path, or a
// timeout that is not positive, is an error.
func NewGo(path string, timeout time.Duration) (Go, error) {
	switch {
	case path == "":
		return Go{}, errors.New("no go command is named")
	case !filepath.IsAbs(path):
		return Go{}, fmt.Errorf("the go command's path must be absolute, not %q", path)
	case timeout <= 0:
		return Go{}, fmt.Errorf("a go command's timeout must be positive, not %s", timeout)
	}
	return Go{path: path, timeout: timeout}, nil
}

// modFile is what modsync needs of a go.mod: the modules it requires, by
// path; the replace directives that name a version, by module, and those
// that name none, which cover every version of their path, by path; and
// the modules it excludes.
type modFile struct {
	requires    map[modPath]requirement
	replaces    map[module]replacement
	replacesAll map[modPath]replacement
	excludes    map[module]bool
}

// requirement is one module a go.mod requires: the version, and whether
// it is marked // indirect.
type requirement struct {
	version  modVersion
	indirect bool
}

// replacement is what a replace directive puts in a module's place.
type replacement int

const (
	// byDirectory is a directory, as test/acceptance replaces
	// github.com/divbast/ci with the checkout just setup makes.
	byDirectory replacement = iota + 1
	// byModule is another module, at a version.
	byModule
)

// replacedWith returns what the go.mod puts in place of path at version,
// as the go command reads its replace directives: one naming that version
// first, then one naming none.
func (m modFile) replacedWith(path modPath, version modVersion) (replacement, bool) {
	if r, ok := m.replaces[module{path: path, version: version}]; ok {
		return r, true
	}
	r, ok := m.replacesAll[path]
	return r, ok
}

// modJSON is what go mod edit -json prints, as far as modsync reads it.
// go mod edit prints more, such as the go line, and adds fields as Go
// grows, so fields modsync does not read are let through; every value it
// does read is checked by parseMod.
type modJSON struct {
	Require []requireJSON `json:"Require"`
	Replace []replaceJSON `json:"Replace"`
	Exclude []moduleJSON  `json:"Exclude"`
}

// requireJSON is one require directive, as go mod edit -json prints it.
type requireJSON struct {
	Path     string `json:"Path"`
	Version  string `json:"Version"`
	Indirect bool   `json:"Indirect"`
}

// replaceJSON is one replace directive, as go mod edit -json prints it.
type replaceJSON struct {
	Old moduleJSON `json:"Old"`
	New moduleJSON `json:"New"`
}

// moduleJSON is a module path, with a version or none, as go mod edit
// -json prints it. A replace's old module with no version covers every
// version; its new one with no version is a directory.
type moduleJSON struct {
	Path    string `json:"Path"`
	Version string `json:"Version"`
}

// readMod returns the go.mod name, relative to the repository's root, as
// go mod edit -json reads it.
func readMod(ctx context.Context, repo *os.Root, goCmd Go, name string) (modFile, error) {
	out, err := runGo(ctx, repo, goCmd, "mod", "edit", "-json", name)
	if err != nil {
		return modFile{}, err
	}
	mod, err := parseMod(out)
	if err != nil {
		return modFile{}, fmt.Errorf("%s: go mod edit -json printed what modsync cannot read: %w", name, err)
	}
	return mod, nil
}

// parseMod returns data, one JSON value as go mod edit -json prints it,
// as a modFile. A path or a version that cannot be one, a module required
// twice, and a replacement with no path are errors.
func parseMod(data []byte) (modFile, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	var raw modJSON
	if err := dec.Decode(&raw); err != nil {
		return modFile{}, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return modFile{}, errors.New("more than one JSON value")
	}
	mod := modFile{
		requires:    map[modPath]requirement{},
		replaces:    map[module]replacement{},
		replacesAll: map[modPath]replacement{},
		excludes:    map[module]bool{},
	}
	for _, r := range raw.Require {
		path, err := newModPath(r.Path)
		if err != nil {
			return modFile{}, fmt.Errorf("a requirement: %w", err)
		}
		version, err := newModVersion(r.Version)
		if err != nil {
			return modFile{}, fmt.Errorf("%s: %w", path, err)
		}
		if _, twice := mod.requires[path]; twice {
			return modFile{}, fmt.Errorf("%s: required twice", path)
		}
		mod.requires[path] = requirement{version: version, indirect: r.Indirect}
	}
	for _, r := range raw.Replace {
		if err := addReplace(mod, r); err != nil {
			return modFile{}, err
		}
	}
	for _, e := range raw.Exclude {
		path, err := newModPath(e.Path)
		if err != nil {
			return modFile{}, fmt.Errorf("an exclude: %w", err)
		}
		version, err := newModVersion(e.Version)
		if err != nil {
			return modFile{}, fmt.Errorf("%s: %w, in an exclude", path, err)
		}
		mod.excludes[module{path: path, version: version}] = true
	}
	return mod, nil
}

// addReplace adds r, one replace directive, to mod: by module when it
// names the version it covers, and by path when it covers every version.
// A path or a version that cannot be one, and a replacement with no path,
// are errors.
func addReplace(mod modFile, r replaceJSON) error {
	path, err := newModPath(r.Old.Path)
	if err != nil {
		return fmt.Errorf("a replace: %w", err)
	}
	var version modVersion
	if r.Old.Version != "" {
		if version, err = newModVersion(r.Old.Version); err != nil {
			return fmt.Errorf("%s: %w, in a replace", path, err)
		}
	}
	if r.New.Path == "" {
		return fmt.Errorf("%s: replaced with nothing", path)
	}
	with := byDirectory
	if r.New.Version != "" {
		if _, err := newModVersion(r.New.Version); err != nil {
			return fmt.Errorf("%s: %w, in a replace", path, err)
		}
		with = byModule
	}
	if r.Old.Version == "" {
		mod.replacesAll[path] = with
	} else {
		mod.replaces[module{path: path, version: version}] = with
	}
	return nil
}

// edited returns the go.mod name, relative to the repository's root, with
// the version of each module in versions set, as go mod edit -print gives
// it: every other line as it was, and nothing written.
func edited(ctx context.Context, repo *os.Root, goCmd Go, name string, versions map[modPath]modVersion) ([]byte, error) {
	args := []string{"mod", "edit", "-print"}
	for _, path := range slices.SortedFunc(maps.Keys(versions), modPath.compare) {
		args = append(args, "-require="+path.String()+"@"+versions[path].String())
	}
	return runGo(ctx, repo, goCmd, append(args, name)...)
}

// runGo runs goCmd with args in the repository's root, stopping it once
// its timeout passes, and returns what it printed to stdout. A failure
// says what go printed to stderr.
func runGo(ctx context.Context, repo *os.Root, goCmd Go, args ...string) ([]byte, error) {
	runCtx, cancel := context.WithTimeout(ctx, goCmd.timeout)
	defer cancel()
	//nolint:gosec // G204: goCmd is the go command main names, and args are go mod edit's, with paths modsync names itself
	cmd := exec.CommandContext(runCtx, goCmd.path, args...)
	cmd.Dir = repo.Name()
	cmd.WaitDelay = waitDelay
	var stdout, stderr capped
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	what := "go " + strings.Join(args, " ")
	switch {
	case err == nil && !stdout.over:
		return stdout.buf.Bytes(), nil
	case ctx.Err() != nil:
		return nil, fmt.Errorf("%s: %w", what, ctx.Err())
	case runCtx.Err() != nil:
		return nil, fmt.Errorf("%s: go did not finish within %s", what, goCmd.timeout)
	case stdout.over:
		return nil, fmt.Errorf("%s: printed more than %d bytes", what, maxOutput)
	}
	if said := strings.TrimSpace(stderr.buf.String()); said != "" {
		return nil, fmt.Errorf("%s: %w: %s", what, err, said)
	}
	return nil, fmt.Errorf("%s: %w", what, err)
}

// capped keeps the first maxOutput bytes written to it, and drops the
// rest, noting that it did. It never fails a write, so a go command that
// prints too much runs to its end, rather than stop at a broken pipe at a
// moment no test could predict.
type capped struct {
	buf  bytes.Buffer
	over bool
}

func (c *capped) Write(p []byte) (int, error) {
	n := len(p)
	if room := maxOutput - c.buf.Len(); n > room {
		c.over = true
		p = p[:room]
	}
	c.buf.Write(p)
	return n, nil
}
