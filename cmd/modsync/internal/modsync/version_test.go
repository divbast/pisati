// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package modsync_test

import (
	"testing"

	"github.com/divbast/pisati/cmd/modsync/internal/modsync"
)

// TestCompareVersions checks the order semantic versioning gives module
// versions: release numbers compared as numbers, a pre-release before
// its release, pre-release identifiers compared one by one, numbers
// before words, and build metadata ignored. Anything but a canonical
// version, as go.mod writes one, is refused.
func TestCompareVersions(t *testing.T) {
	tests := []struct {
		a, b string
		want int
		// err is what the error says; empty when both are versions.
		err string
	}{
		{a: "v1.3.0", b: "v1.3.1", want: -1},
		{a: "v1.3.1", b: "v1.3.0", want: 1},
		{a: "v1.3.0", b: "v1.3.0", want: 0},
		{a: "v1.10.0", b: "v1.9.0", want: 1},
		{a: "v0.3.1100", b: "v0.3.1118", want: -1},
		{a: "v2.0.0", b: "v10.0.0", want: -1},
		{a: "v1.0.0-rc.1", b: "v1.0.0", want: -1},
		{a: "v1.0.0-alpha", b: "v1.0.0-alpha.1", want: -1},
		{a: "v1.0.0-alpha.1", b: "v1.0.0-alpha.beta", want: -1},
		{a: "v1.0.0-beta.2", b: "v1.0.0-beta.11", want: -1},
		{a: "v1.0.0-rc.1", b: "v1.0.0-rc.1", want: 0},
		{a: "v1.0.0-RC", b: "v1.0.0-rc", want: -1},
		{a: "v2.0.0+incompatible", b: "v2.0.0", want: 0},
		{a: "v0.0.0-20191010083416-a7dc8b61c822", b: "v0.0.0-20200101000000-000000000000", want: -1},
		{a: "v0.51.0-0.20261101000000-abcdefabcdef", b: "v0.50.0", want: 1},
		{a: "v0.51.0-0.20261101000000-abcdefabcdef", b: "v0.51.0", want: -1},
		{a: "1.0.0", b: "v1.0.0", err: `"1.0.0" is not a module version`},
		{a: "v1.0.0", b: "v1.0", err: `"v1.0" is not a module version`},
		{a: "v01.0.0", b: "v1.0.0", err: `"v01.0.0" is not a module version`},
		{a: "v1.0.0-", b: "v1.0.0", err: `"v1.0.0-" is not a module version`},
		{a: "v1.0.0-01", b: "v1.0.0", err: `"v1.0.0-01" is not a module version`},
		{a: "v1.0.0-a..b", b: "v1.0.0", err: `"v1.0.0-a..b" is not a module version`},
		{a: "v1.0.0+", b: "v1.0.0", err: `"v1.0.0+" is not a module version`},
		{a: "v1.0.0-a_b", b: "v1.0.0", err: `"v1.0.0-a_b" is not a module version`},
		{a: "v1.0.0", b: "v1.x.0", err: `"v1.x.0" is not a module version`},
	}
	for _, tt := range tests {
		got, err := modsync.CompareVersions(tt.a, tt.b)
		switch {
		case tt.err == "" && err != nil:
			t.Errorf("CompareVersions(%q, %q): %v", tt.a, tt.b, err)
		case tt.err == "" && got != tt.want:
			t.Errorf("CompareVersions(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		case tt.err != "" && (err == nil || err.Error() != tt.err):
			t.Errorf("CompareVersions(%q, %q): error %v, want %q", tt.a, tt.b, err, tt.err)
		}
	}
}

// FuzzCompareVersions checks the order's invariants on any two inputs:
// they are compared only when each is a version, which is when a version
// compares equal to itself; and swapping them flips the result.
func FuzzCompareVersions(f *testing.F) {
	for _, seed := range [][2]string{
		{"v1.3.0", "v1.3.1"},
		{"v1.0.0-alpha.1", "v1.0.0-alpha.beta"},
		{"v2.0.0+incompatible", "v2.0.0"},
		{"v0.0.0-20191010083416-a7dc8b61c822", "v0.0.0-20200101000000-000000000000"},
		{"v1.0.0-01", "v1.0"},
		{"", "v"},
	} {
		f.Add(seed[0], seed[1])
	}
	f.Fuzz(func(t *testing.T, a, b string) {
		ab, errAB := modsync.CompareVersions(a, b)
		ba, errBA := modsync.CompareVersions(b, a)
		aa, errA := modsync.CompareVersions(a, a)
		bb, errB := modsync.CompareVersions(b, b)
		valid := errA == nil && errB == nil
		switch {
		case (errAB == nil) != valid || (errBA == nil) != valid:
			t.Fatalf("CompareVersions(%q, %q) and (%q, %q) erred %v and %v, but each with itself %v and %v", a, b, b, a, errAB, errBA, errA, errB)
		case errA == nil && aa != 0, errB == nil && bb != 0:
			t.Fatalf("a version does not equal itself: %q gives %d, %q gives %d", a, aa, b, bb)
		case valid && ab != -ba:
			t.Fatalf("CompareVersions(%q, %q) = %d, but (%q, %q) = %d", a, b, ab, b, a, ba)
		case valid && ab != -1 && ab != 0 && ab != 1:
			t.Fatalf("CompareVersions(%q, %q) = %d, not -1, 0 or 1", a, b, ab)
		}
	})
}
