// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package modsync

import (
	"fmt"
	"maps"
)

// ParseMod parses data as parseMod does, for FuzzParseMod: it returns
// parseMod's error, and nil when data parses.
func ParseMod(data []byte) error {
	_, err := parseMod(data)
	return err
}

// Resync plans bringing the acceptance module in step with the root,
// each given as go mod edit -json prints its go.mod and as its go.sum,
// applies the plan to the acceptance module in memory, as apply does to
// its files, and plans again, for FuzzSyncIdempotent. When the inputs do
// not parse, or the first plan fails, it returns that error, and planned
// false. Otherwise it returns planned true, each difference the second
// plan finds, and the second plan's error.
func Resync(rootMod, rootSum, acceptanceMod, acceptanceSum []byte) (planned bool, again []string, err error) {
	root, err := parseMod(rootMod)
	if err != nil {
		return false, nil, err
	}
	acceptance, err := parseMod(acceptanceMod)
	if err != nil {
		return false, nil, err
	}
	rootLines, err := parseSum("go.sum", rootSum)
	if err != nil {
		return false, nil, err
	}
	acceptanceLines, err := parseSum("test/acceptance/go.sum", acceptanceSum)
	if err != nil {
		return false, nil, err
	}
	s, err := plan(root, acceptance, rootLines, acceptanceLines)
	if err != nil {
		return false, nil, err
	}
	applied := acceptance
	applied.requires = maps.Clone(acceptance.requires)
	for path, version := range s.versions {
		req := applied.requires[path]
		req.version = version
		applied.requires[path] = req
	}
	s2, err := plan(root, applied, rootLines, s.sum)
	if err != nil {
		return true, nil, fmt.Errorf("the second plan: %w", err)
	}
	for _, d := range s2.differences {
		again = append(again, d.String())
	}
	if fmt.Sprint(s2.sum) != fmt.Sprint(s.sum) {
		again = append(again, "the second plan changes go.sum again")
	}
	return true, again, nil
}
