// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package modsync_test

import (
	"strings"
	"testing"

	"github.com/divbast/pisati/cmd/modsync/internal/modsync"
)

// TestParseSumLine checks that each line the go command writes parses and
// comes back as it was written, that white space between the fields is
// read as the go command reads it, and that anything else is refused,
// with the reason.
func TestParseSumLine(t *testing.T) {
	tests := []struct {
		name string
		line string
		// want is the line as String writes it; empty when the line is
		// refused.
		want string
		// err is what the error says; empty when the line parses.
		err string
	}{
		{
			name: "the hash of a module's files",
			line: "go.uber.org/goleak v1.3.0 h1:2K3zAYmnTNqV73imy9J1T3WC+gmCePx2hEGkimedGto=",
			want: "go.uber.org/goleak v1.3.0 h1:2K3zAYmnTNqV73imy9J1T3WC+gmCePx2hEGkimedGto=",
		},
		{
			name: "the hash of a module's go.mod",
			line: "go.uber.org/goleak v1.3.0/go.mod h1:CoHD4mav9JJNrW/WLlf7HGZPjdw8EucARQHekz1X6bE=",
			want: "go.uber.org/goleak v1.3.0/go.mod h1:CoHD4mav9JJNrW/WLlf7HGZPjdw8EucARQHekz1X6bE=",
		},
		{
			name: "a pseudo-version",
			line: "github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 h1:C3w9PqII01/Oq1c1nUAm88MOHcQC9l5mIlSMApZMrHA=",
			want: "github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 h1:C3w9PqII01/Oq1c1nUAm88MOHcQC9l5mIlSMApZMrHA=",
		},
		{
			name: "an incompatible major",
			line: "github.com/example/old v2.0.0+incompatible/go.mod h1:abc=",
			want: "github.com/example/old v2.0.0+incompatible/go.mod h1:abc=",
		},
		{
			name: "tabs and runs of spaces, which the go command reads as one separator",
			line: " go.uber.org/goleak\tv1.3.0   h1:abc=\r",
			want: "go.uber.org/goleak v1.3.0 h1:abc=",
		},
		{name: "two fields", line: "go.uber.org/goleak v1.3.0", err: "2 fields, but a go.sum line has 3"},
		{name: "four fields", line: "go.uber.org/goleak v1.3.0 h1:abc= h1:def=", err: "4 fields, but a go.sum line has 3"},
		{name: "nothing", line: "", err: "0 fields, but a go.sum line has 3"},
		{name: "a path with a character no module path has", line: "go.uber.org/go\"leak v1.3.0 h1:abc=", err: `"go.uber.org/go\"leak" is not a module path`},
		{name: "a path that starts with a slash", line: "/go.uber.org/goleak v1.3.0 h1:abc=", err: `"/go.uber.org/goleak" is not a module path`},
		{name: "a version without its v", line: "go.uber.org/goleak 1.3.0 h1:abc=", err: `"1.3.0" is not a module version`},
		{name: "a version cut short", line: "go.uber.org/goleak v1.3 h1:abc=", err: `"v1.3" is not a module version`},
		{name: "a version with a leading zero", line: "go.uber.org/goleak v1.03.0 h1:abc=", err: `"v1.03.0" is not a module version`},
		{name: "only a v", line: "go.uber.org/goleak v h1:abc=", err: `"v" is not a module version`},
		{name: "a file other than go.mod", line: "go.uber.org/goleak v1.3.0/LICENSE h1:abc=", err: `"v1.3.0/LICENSE" is not a module version`},
		{name: "go.mod with no version", line: "go.uber.org/goleak /go.mod h1:abc=", err: `"/go.mod" is not a module version`},
		{name: "a hash with no algorithm", line: "go.uber.org/goleak v1.3.0 abc=", err: `"abc=" is not a go.sum hash`},
		{name: "an algorithm with no number", line: "go.uber.org/goleak v1.3.0 h:abc=", err: `"h:abc=" is not a go.sum hash`},
		{name: "an algorithm and no hash", line: "go.uber.org/goleak v1.3.0 h1:", err: `"h1:" is not a go.sum hash`},
		{name: "a hash outside base64", line: "go.uber.org/goleak v1.3.0 h1:ab*c", err: `"h1:ab*c" is not a go.sum hash`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			line, err := modsync.ParseSumLine(tt.line)
			if tt.err != "" {
				if err == nil {
					t.Fatalf("ParseSumLine(%q) = %q, want an error saying %q", tt.line, line, tt.err)
				}
				if err.Error() != tt.err {
					t.Fatalf("ParseSumLine(%q): error %q, want %q", tt.line, err, tt.err)
				}
				if line != (modsync.SumLine{}) {
					t.Fatalf("ParseSumLine(%q) returned %q beside its error, want the zero SumLine", tt.line, line)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseSumLine(%q): %v", tt.line, err)
			}
			if got := line.String(); got != tt.want {
				t.Fatalf("ParseSumLine(%q).String() = %q, want %q", tt.line, got, tt.want)
			}
		})
	}
}

// FuzzParseSumLine checks the parser's invariants on any input: a line it
// accepts is never the zero SumLine; it comes back as its three fields,
// joined by one space; and that form parses to the same SumLine, so
// parsing what String writes changes nothing.
func FuzzParseSumLine(f *testing.F) {
	for _, seed := range []string{
		"go.uber.org/goleak v1.3.0 h1:2K3zAYmnTNqV73imy9J1T3WC+gmCePx2hEGkimedGto=",
		"go.uber.org/goleak v1.3.0/go.mod h1:CoHD4mav9JJNrW/WLlf7HGZPjdw8EucARQHekz1X6bE=",
		"github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 h1:C3w9PqII01/Oq1c1nUAm88MOHcQC9l5mIlSMApZMrHA=",
		"github.com/example/old v2.0.0+incompatible/go.mod h1:abc=",
		" a\tv1  h1:x=\r",
		"a v1/go.mod/go.mod h1:x=",
		"a v1",
		"",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		line, err := modsync.ParseSumLine(s)
		if err != nil {
			return
		}
		if line == (modsync.SumLine{}) {
			t.Fatalf("ParseSumLine(%q) accepted it as the zero SumLine", s)
		}
		out := line.String()
		if want := strings.Join(strings.Fields(s), " "); out != want {
			t.Fatalf("ParseSumLine(%q).String() = %q, want its fields joined by one space, %q", s, out, want)
		}
		again, err := modsync.ParseSumLine(out)
		if err != nil {
			t.Fatalf("ParseSumLine(%q) refuses what String wrote for %q: %v", out, s, err)
		}
		if again != line {
			t.Fatalf("ParseSumLine(%q) = %q, but the line it came from, %q, parsed to %q", out, again, s, line)
		}
	})
}
