// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package acceptance_test

import (
	"testing"

	"github.com/divbast/ci/harness"
)

// plainHTTP is a base URL Pisati refuses: plain http, away from localhost.
const plainHTTP = "http://blog.example.com"

// TestRefusesReadOnlyState starts serve where it cannot write its state:
// on the image's own state directory, on the read-only root, as when no
// volume is mounted, and on a volume mounted read-only. Each time serve
// must exit 1 before it serves, naming the directory on stderr, rather
// than start without anywhere to keep its state.
func TestRefusesReadOnlyState(t *testing.T) {
	t.Parallel()
	e, image := underTest(t)
	for _, tc := range []struct {
		name   string
		mounts func(t *testing.T) []harness.Mount
	}{
		{"no volume", func(*testing.T) []harness.Mount { return nil }},
		{"a read-only volume", func(t *testing.T) []harness.Mount {
			m := stateMount(t, e)
			m.ReadOnly = true
			return []harness.Mount{m}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			refuses(t, e, &harness.RunSpec{
				Image:   image,
				Network: "none",
				Confine: harness.Contract(tmpfsSize),
				Env:     pisatiEnv(),
				Mounts:  tc.mounts(t),
			}, stateDir)
		})
	}
}

// TestRefusesPlainHTTPBaseURL starts serve with a base URL in plain http
// away from localhost, with a volume it can write to, so that the base
// URL is all it can refuse. serve must exit 1, naming the variable on
// stderr, and never the value, as a URL may hold a password.
func TestRefusesPlainHTTPBaseURL(t *testing.T) {
	t.Parallel()
	e, image := underTest(t)
	spec := serving(image, stateMount(t, e))
	spec.Network = "none"
	spec.Env = map[string]string{baseURLVar: plainHTTP}
	stderr := refuses(t, e, &spec, baseURLVar)
	harness.Lacks(t, "serve's stderr does not repeat the base URL", "blog.example.com", stderr)
}

// TestRefusesWithoutBaseURL starts serve with no base URL at all, which
// the image sets no default for, since a site's address cannot be
// guessed. serve must exit 1, naming the variable on stderr.
func TestRefusesWithoutBaseURL(t *testing.T) {
	t.Parallel()
	e, image := underTest(t)
	spec := serving(image, stateMount(t, e))
	spec.Network = "none"
	spec.Env = nil
	refuses(t, e, &spec, baseURLVar)
}

// refuses runs spec and checks that serve exits 1, with names on its
// stderr. It returns what serve wrote on stderr.
func refuses(t *testing.T, e harness.Engine, spec *harness.RunSpec, names string) string {
	t.Helper()
	checks := harness.NewChecks(t)
	res, err := runToExit(t.Context(), t, e, spec)
	if err != nil {
		checks.True("serve exits", false, err.Error())
		return ""
	}
	stderr := string(res.Stderr)
	harness.Equal(checks, "serve exits 1", 1, res.Code)
	harness.Contains(t, "serve's stderr names "+names, names, stderr)
	return stderr
}
