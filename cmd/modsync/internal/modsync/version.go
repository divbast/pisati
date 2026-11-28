// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package modsync

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
)

// CompareVersions compares two module versions by semantic versioning's
// precedence, as the go command orders them: it returns -1 when a comes
// before b, 0 when neither does, and +1 when a comes after b. Build
// metadata, such as +incompatible, does not count. Either one not a
// canonical semantic version with a leading v, as go.mod writes them, is
// an error.
//
// It makes each a modVersion first, and compares those with
// compareVersions, which modsync itself calls.
func CompareVersions(a, b string) (int, error) {
	va, err := newModVersion(a)
	if err != nil {
		return 0, err
	}
	vb, err := newModVersion(b)
	if err != nil {
		return 0, err
	}
	return compareVersions(va, vb), nil
}

// compareVersions compares a and b as CompareVersions does. Each holds
// the parts that order it, which newModVersion parsed, so the comparison
// cannot fail.
func compareVersions(a, b modVersion) int {
	for i := range a.order.release {
		if c := compareNumbers(a.order.release[i], b.order.release[i]); c != 0 {
			return c
		}
	}
	return comparePre(a.order.pre, b.order.pre)
}

// semver is what orders a module version: its major, minor and patch
// numbers, as digits, and its pre-release identifiers, separated by dots,
// empty for a release. It is a comparable value, so a modVersion holding
// one can key a map.
type semver struct {
	release [3]string
	pre     string
}

// parseSemver returns v's parts that order it, or an error unless v is
// a v, three numbers separated by dots, then optionally a hyphen and
// pre-release identifiers, then optionally a plus and build identifiers,
// each part as semantic versioning 2.0.0 spells it.
func parseSemver(v string) (semver, error) {
	bad := fmt.Errorf("%q is not a module version", v)
	rest, ok := strings.CutPrefix(v, "v")
	if !ok {
		return semver{}, bad
	}
	rest, build, hasBuild := strings.Cut(rest, "+")
	if hasBuild && !buildIdentifiers(build) {
		return semver{}, bad
	}
	rest, pre, hasPre := strings.Cut(rest, "-")
	if hasPre && !preIdentifiers(pre) {
		return semver{}, bad
	}
	numbers := strings.Split(rest, ".")
	if len(numbers) != 3 {
		return semver{}, bad
	}
	var s semver
	for i, n := range numbers {
		if !isNumber(n) {
			return semver{}, bad
		}
		s.release[i] = n
	}
	s.pre = pre
	return s, nil
}

// buildIdentifiers reports whether s is build identifiers: separated by
// dots, each of letters, digits and hyphens, and none empty.
func buildIdentifiers(s string) bool {
	for id := range strings.SplitSeq(s, ".") {
		if !isIdentifier(id) {
			return false
		}
	}
	return true
}

// preIdentifiers reports whether s is pre-release identifiers: as build
// identifiers are, with no leading zero on one of digits alone.
func preIdentifiers(s string) bool {
	for id := range strings.SplitSeq(s, ".") {
		if !isIdentifier(id) || isDigits(id) && !isNumber(id) {
			return false
		}
	}
	return true
}

// isIdentifier reports whether id is letters, digits and hyphens, at
// least one.
func isIdentifier(id string) bool {
	return id != "" && strings.Trim(id, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-") == ""
}

// isDigits reports whether s is digits alone, at least one.
func isDigits(s string) bool {
	return s != "" && strings.Trim(s, "0123456789") == ""
}

// isNumber reports whether s is a number as semantic versioning writes
// one: 0, or digits that do not start with 0.
func isNumber(s string) bool {
	return isDigits(s) && (s == "0" || s[0] != '0')
}

// compareNumbers compares two numbers isNumber accepts, as numbers:
// with no leading zeros, the longer is the larger, and digits of one
// length compare as text does.
func compareNumbers(a, b string) int {
	return cmp.Or(cmp.Compare(len(a), len(b)), strings.Compare(a, b))
}

// comparePre compares two versions' pre-release identifiers: none, a
// release, comes after any; then identifier by identifier, numbers as
// numbers, before any other, and others as text; then the one with more
// identifiers comes after.
func comparePre(a, b string) int {
	if a == "" || b == "" {
		return -cmp.Compare(len(a), len(b))
	}
	return slices.CompareFunc(strings.Split(a, "."), strings.Split(b, "."), compareIdentifiers)
}

// compareIdentifiers compares two pre-release identifiers.
func compareIdentifiers(a, b string) int {
	numA, numB := isDigits(a), isDigits(b)
	switch {
	case numA && numB:
		return compareNumbers(a, b)
	case numA:
		return -1
	case numB:
		return 1
	}
	return strings.Compare(a, b)
}
