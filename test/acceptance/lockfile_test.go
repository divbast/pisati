// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package acceptance_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/divbast/ci/harness"
)

// lockfile is where the image holds the lockfile of the npm packages the
// front end was built with, and repoLockfile that lockfile in this
// repository, from this module's directory. lockfileLabel is the image's
// label that names it, for divbast/ci's scan, which reads each lockfile
// the label names and scans the packages it records.
const (
	lockfile      = "/usr/share/doc/pisati/pnpm-lock.yaml"
	repoLockfile  = "../../pnpm-lock.yaml"
	lockfileLabel = "io.github.divbast.scan.lockfiles"
)

// TestImageCarriesLockfile checks the image holds the lockfile of the npm
// packages that built the stylesheets and scripts compiled into the
// binary: exactly this repository's pnpm-lock.yaml, as a regular file.
// The image holds no node_modules, so this file is its one record of
// which packages those were.
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

// TestImageNamesItsLockfile checks the image's label for divbast/ci's scan
// names exactly the lockfile the image holds, and nothing else: no other
// path, and no list.
func TestImageNamesItsLockfile(t *testing.T) {
	t.Parallel()
	e, image := underTest(t)
	checks := harness.NewChecks(t)

	labels, err := imageLabels(t.Context(), e, image)
	if err != nil {
		checks.True("the engine reports the image's labels", false, err.Error())
		return
	}
	got, ok := labels[lockfileLabel]
	checks.True("the image has the label "+lockfileLabel, ok, fmt.Sprintf("its labels: %v", labels))
	checks.True(lockfileLabel+" is "+lockfile, got == lockfile, fmt.Sprintf("it is %q", got))
}

// imageLabels returns the labels of image, as `<engine> image inspect`
// reports them, within a minute.
func imageLabels(ctx context.Context, e harness.Engine, image string) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, e.Name(), "image", "inspect", "--format", "{{json .Config.Labels}}", image) //nolint:gosec // G204: the engine is the harness's own, and the image the one under test
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s image inspect %s: %w: %s", e.Name(), image, err, strings.TrimSpace(stderr.String()))
	}
	var labels map[string]string
	if err := json.Unmarshal(out, &labels); err != nil {
		return nil, fmt.Errorf("%s image inspect %s printed what is not a set of labels: %w", e.Name(), image, err)
	}
	return labels, nil
}
