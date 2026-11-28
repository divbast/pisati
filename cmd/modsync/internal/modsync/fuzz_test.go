// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package modsync_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/divbast/pisati/cmd/modsync/internal/modsync"
)

// FuzzParseMod checks that any input, as go mod edit -json might print
// it, parses or is refused with an error, the same way every time, and
// never panics.
func FuzzParseMod(f *testing.F) {
	for _, seed := range []string{
		`{"Module": {"Path": "example.com/root"}, "Go": "1.27.0", "Require": [{"Path": "go.uber.org/goleak", "Version": "v1.3.0"}, {"Path": "golang.org/x/sys", "Version": "v0.50.0", "Indirect": true}]}`,
		`{"Replace": [{"Old": {"Path": "github.com/divbast/ci"}, "New": {"Path": "../../.ci"}}, {"Old": {"Path": "a.example/m", "Version": "v1.0.0"}, "New": {"Path": "b.example/m", "Version": "v1.1.0"}}]}`,
		`{"Exclude": [{"Path": "go.uber.org/goleak", "Version": "v1.3.1"}]}`,
		`{"Require": [{"Path": "a.example/m", "Version": "v1.0"}]}`,
		`{} {}`,
		`{"Require": {}}`,
		``,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		first, again := modsync.ParseMod(data), modsync.ParseMod(data)
		if fmt.Sprint(first) != fmt.Sprint(again) {
			t.Fatalf("ParseMod(%q) gave %v, then %v", data, first, again)
		}
	})
}

// FuzzSyncIdempotent builds a root module and an acceptance module from
// data: for each of three paths, whether each module requires it, at
// which of five versions, directly or not, and which go.sum lines the
// acceptance module holds. It checks that, once the plan modsync makes is
// applied, a second plan finds nothing more to change.
func FuzzSyncIdempotent(f *testing.F) {
	for _, seed := range []string{
		"",
		// Both require a.example/m at v1.0.0, with its lines.
		"\x01\x01\x00\x01",
		// The root requires a.example/m at v1.1.0, the acceptance module
		// at v1.0.0, with v1.0.0's lines: modsync moves it up.
		"\x11\x01\x00\x01",
		// The same, and b.example/m behind, indirectly, and c.example/m
		// in step at a pseudo-version.
		"\x11\x01\x00\x01\x33\x13\x00\x02\x41\x41\x00\x10",
		"\x11\x21\x30\x0f\x13\x03\x00\x05\x00\x00\xff\xff",
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		root, rootSum, acceptance, acceptanceSum := modules(data)
		planned, again, err := modsync.Resync(root, rootSum, acceptance, acceptanceSum)
		if !planned {
			return
		}
		if err != nil || len(again) > 0 {
			t.Fatalf("modules from %q:\nroot %s\n%s\nacceptance %s\n%s\nonce in step: %v %q", data, root, rootSum, acceptance, acceptanceSum, err, again)
		}
	})
}

// modules returns the two modules data describes, four bytes for each of
// three paths, missing bytes read as zero: whether the root requires the
// path, and the acceptance module, the low bit of each byte; whether
// indirectly, the next bit; which of five versions, the high nibble; and
// the fourth byte's bits, which acceptance go.sum lines there are, each
// version's hash of its files and of its go.mod in turn.
func modules(data []byte) (root, rootSum, acceptance, acceptanceSum []byte) {
	paths := []string{"a.example/m", "b.example/m", "c.example/m"}
	versions := []string{"v1.0.0", "v1.1.0", "v1.2.0-rc.1", "v2.0.0+incompatible", "v0.0.0-20200101000000-abcdefabcdef"}
	at := func(i int) byte {
		if i < len(data) {
			return data[i]
		}
		return 0
	}
	type require struct {
		Path     string `json:"Path"`
		Version  string `json:"Version"`
		Indirect bool   `json:"Indirect"`
	}
	type goMod struct {
		Require []require `json:"Require"`
	}
	var rootMod, acceptanceMod goMod
	var rootLines, acceptanceLines strings.Builder
	line := func(b *strings.Builder, path, version string) {
		fmt.Fprintf(b, "%s %s h1:%dfiles=\n%s %s/go.mod h1:%dgomod=\n", path, version, len(path)+len(version), path, version, len(path)+len(version))
	}
	for i, path := range paths {
		r, a, sums := at(4*i), at(4*i+1), at(4*i+3)
		if r&1 == 1 {
			version := versions[int(r>>4)%len(versions)]
			rootMod.Require = append(rootMod.Require, require{Path: path, Version: version, Indirect: r&2 == 2})
			line(&rootLines, path, version)
		}
		if a&1 == 1 {
			acceptanceMod.Require = append(acceptanceMod.Require, require{Path: path, Version: versions[int(a>>4)%len(versions)], Indirect: a&2 == 2})
		}
		for j, version := range versions {
			if sums&(1<<j) != 0 {
				line(&acceptanceLines, path, version)
			}
		}
	}
	rootJSON, rootErr := json.Marshal(rootMod)
	acceptanceJSON, acceptanceErr := json.Marshal(acceptanceMod)
	if rootErr != nil || acceptanceErr != nil {
		panic(fmt.Sprint(rootErr, acceptanceErr))
	}
	return rootJSON, []byte(rootLines.String()), acceptanceJSON, []byte(acceptanceLines.String())
}
