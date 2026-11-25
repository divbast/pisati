// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package acceptance_test

import (
	"bytes"
	"fmt"
	"os"
	"testing"

	"github.com/divbast/ci/harness"
)

// lockfile is where the image holds the lockfile of the npm packages the
// front end was built with, and repoLockfile that lockfile in this
// repository, from this module's directory.
const (
	lockfile     = "/usr/share/doc/pisati/pnpm-lock.yaml"
	repoLockfile = "../../pnpm-lock.yaml"
)

// TestImageCarriesLockfile checks the image holds the lockfile of the npm
// packages that built the stylesheets and scripts compiled into the
// binary, so that an image scanner, which finds no node_modules in it,
// still sees which packages they came from: exactly this repository's
// pnpm-lock.yaml, as a regular file.
func TestImageCarriesLockfile(t *testing.T) {
	t.Parallel()
	e, image := underTest(t)
	ctx := t.Context()
	checks := harness.NewChecks(t)

	want, err := os.ReadFile(repoLockfile)
	if err != nil {
		t.Fatal(err)
	}
	c, err := e.Start(ctx, t, &harness.RunSpec{
		Image:      image,
		Network:    "none",
		Confine:    harness.Contract(tmpfsSize),
		Entrypoint: []string{binary, "version"},
	})
	if err != nil {
		checks.True("a container of the image starts", false, err.Error())
		return
	}
	if _, err := c.WaitExit(ctx, harness.ServeWithin); err != nil {
		checks.True("pisati version exits", false, err.Error())
		return
	}
	files, err := copyOut(ctx, e, c, lockfile)
	if err != nil {
		checks.True("the image holds "+lockfile, false, err.Error())
		return
	}
	f, ok := files["."]
	checks.True("the image holds "+lockfile+" as a regular file", ok && f.mode.IsRegular(), listed(files))
	checks.True(lockfile+" is this repository's pnpm-lock.yaml", ok && bytes.Equal(f.data, want),
		fmt.Sprintf("%d bytes, against %d in %s", len(f.data), len(want), repoLockfile))
}
