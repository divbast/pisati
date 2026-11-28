// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package modsync

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
)

// modPath is a module's path, as go.mod and go.sum name it. Only
// newModPath makes one; the zero modPath is none.
type modPath struct {
	s string
}

// newModPath returns s as a modPath, or an error unless isPath accepts it.
func newModPath(s string) (modPath, error) {
	if !isPath(s) {
		return modPath{}, fmt.Errorf("%q is not a module path", s)
	}
	return modPath{s: s}, nil
}

// String returns the path as go.mod and go.sum write it.
func (p modPath) String() string {
	return p.s
}

// compare orders p before, with or after q, as the go command orders the
// paths of a go.sum: byte by byte.
func (p modPath) compare(q modPath) int {
	return strings.Compare(p.s, q.s)
}

// modVersion is a module's version, as go.mod and go.sum write it: a
// canonical semantic version with a leading v, and the parts that order
// it. Only newModVersion makes one; the zero modVersion is none.
type modVersion struct {
	s     string
	order semver
}

// newModVersion returns s as a modVersion, or an error unless s is a
// canonical semantic version with a leading v, as parseSemver reads one.
func newModVersion(s string) (modVersion, error) {
	order, err := parseSemver(s)
	if err != nil {
		return modVersion{}, err
	}
	return modVersion{s: s, order: order}, nil
}

// String returns the version as go.mod and go.sum write it.
func (v modVersion) String() string {
	return v.s
}

// module is a module at one version.
type module struct {
	path    modPath
	version modVersion
}

// SumLine is one line of a go.sum file: a module's path, a version of it,
// and a hash of either the module's files at that version or its go.mod
// alone. The zero SumLine is none: ParseSumLine never returns it.
type SumLine struct {
	path    modPath
	version modVersion
	covers  coverage
	hash    string
}

// coverage is what a go.sum line's hash is of.
type coverage int

const (
	// coversFiles is the hash of every file of the module, written after
	// the version alone.
	coversFiles coverage = iota + 1
	// coversGoMod is the hash of the module's go.mod alone, written after
	// the version and /go.mod.
	coversGoMod
)

// goModSuffix follows the version on a line whose hash is of the
// module's go.mod alone.
const goModSuffix = "/go.mod"

// ParseSumLine returns line, one line of a go.sum file without its
// newline, as a SumLine. The line holds three fields, separated by white
// space, as the go command reads them: a module path; a module version,
// followed by /go.mod when the hash is of the module's go.mod alone; and
// the hash, such as h1:, then base64. Anything else is an error, which
// says why.
func ParseSumLine(line string) (SumLine, error) {
	fields := strings.Fields(line)
	if len(fields) != 3 {
		return SumLine{}, fmt.Errorf("%d fields, but a go.sum line has 3", len(fields))
	}
	path, version, hash := fields[0], fields[1], fields[2]
	covers := coversFiles
	if v, ok := strings.CutSuffix(version, goModSuffix); ok {
		version, covers = v, coversGoMod
	}
	p, err := newModPath(path)
	if err != nil {
		return SumLine{}, err
	}
	v, err := newModVersion(version)
	if err != nil {
		// The field as the line spells it, with /go.mod, says more than
		// the version alone.
		return SumLine{}, fmt.Errorf("%q is not a module version", fields[1])
	}
	if !isHash(hash) {
		return SumLine{}, fmt.Errorf("%q is not a go.sum hash", hash)
	}
	return SumLine{path: p, version: v, covers: covers, hash: hash}, nil
}

// String returns the line as the go command writes it: its three fields,
// separated by one space.
func (l SumLine) String() string {
	version := l.version.String()
	if l.covers == coversGoMod {
		version += goModSuffix
	}
	return l.path.String() + " " + version + " " + l.hash
}

// isPath reports whether s can be a module path: the letters, digits and
// punctuation the go command allows in one, in elements separated by
// single slashes, none of them empty. It is stricter than nothing and
// looser than the go command: modsync only needs to tell a path from a
// mistake, and never to resolve one.
func isPath(s string) bool {
	for elem := range strings.SplitSeq(s, "/") {
		if elem == "" || strings.Trim(elem, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._~") != "" {
			return false
		}
	}
	return true
}

// isHash reports whether s can be a go.sum hash: h and a number, naming
// the algorithm, then a colon and the hash in base64.
func isHash(s string) bool {
	alg, sum, ok := strings.Cut(s, ":")
	number, isH := strings.CutPrefix(alg, "h")
	return ok && isH && number != "" && strings.Trim(number, "0123456789") == "" &&
		sum != "" && strings.Trim(sum, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789+/=") == ""
}

// parseSum returns the lines of data, the go.sum file name, skipping
// blank lines as the go command does. A line ParseSumLine refuses is an
// error naming the file and the line's number; every such line is
// reported.
func parseSum(name string, data []byte) ([]SumLine, error) {
	var lines []SumLine
	var errs []error
	n := 0
	for text := range strings.Lines(string(data)) {
		n++
		if strings.TrimSpace(text) == "" {
			continue
		}
		line, err := ParseSumLine(text)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s:%d: %w", name, n, err))
			continue
		}
		lines = append(lines, line)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return lines, nil
}

// formatSum returns lines as a go.sum file holds them, one to a line.
func formatSum(lines []SumLine) []byte {
	var b bytes.Buffer
	for _, l := range lines {
		b.WriteString(l.String())
		b.WriteByte('\n')
	}
	return b.Bytes()
}
