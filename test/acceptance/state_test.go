// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package acceptance_test

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/divbast/ci/harness"
)

// The files Pisati keeps in the state directory, each of which it writes
// on its first start: the database, the lock a running Pisati holds, and
// the main key it generates when none is configured.
const (
	database = "pisati.db"
	lock     = "pisati.lock"
	mainKey  = "keys/main.key"
)

// written are those files.
var written = []string{database, lock, mainKey}

// TestStatePersists serves from a fresh volume, and checks what Pisati
// wrote there is its user's alone: every file 0600, every directory 0700.
// It stops that container, runs pisati migrate on the volume in a second,
// and serves the volume again from a third, which it stops too. Nothing
// in a migrate with nothing to do, or in a start with nothing due, writes
// to the database, so each container after the first must leave the
// database exactly as the first left it, byte for byte: a database made
// anew, as when the state did not persist, differs. The third must also
// start with the main key the first generated.
func TestStatePersists(t *testing.T) {
	t.Parallel()
	e, image := underTest(t)
	ctx := t.Context()
	checks := harness.NewChecks(t)
	state := stateMount(t, e)

	first, err := e.Serve(ctx, t, &harness.ServeSpec{Run: serving(image, state), AppPort: appPort})
	if err != nil {
		checks.True("the first container serves", false, err.Error())
		return
	}
	harness.StatusIs(t, first, "GET / from the first container answers 200", http.StatusOK, "/")
	files, err := copyOut(ctx, e, first.C, stateDir)
	if err != nil {
		checks.True("the first container's state directory can be read", false, err.Error())
		return
	}
	for _, name := range written {
		_, ok := files[name]
		checks.True("the first start wrote "+name, ok, listed(files))
	}
	for _, name := range slices.Sorted(maps.Keys(files)) {
		f := files[name]
		want := fs.FileMode(0o600)
		if f.mode.IsDir() {
			want = fs.ModeDir | 0o700
		}
		harness.Equal(checks, "only Pisati's user may use "+name+" in the state directory", want.String(), f.mode.String())
	}
	left, ok := stopAndCopy(ctx, t, checks, e, first.C, "the first container")
	if !ok {
		return
	}

	spec := serving(image, state)
	spec.Network = "none"
	spec.Entrypoint = []string{binary, "migrate"}
	migrate, err := e.Start(ctx, t, &spec)
	if err != nil {
		checks.True("pisati migrate starts on the volume", false, err.Error())
		return
	}
	code, err := migrate.WaitExit(ctx, harness.ServeWithin)
	if err != nil {
		checks.True("pisati migrate exits", false, err.Error())
		return
	}
	harness.Equal(checks, "pisati migrate exits 0", 0, code)
	migrated, err := copyOut(ctx, e, migrate, stateDir)
	if err != nil {
		checks.True("the state directory pisati migrate left can be read", false, err.Error())
		return
	}
	same(checks, "pisati migrate leaves the database the first start left", database, left, migrated)

	third, err := e.Serve(ctx, t, &harness.ServeSpec{Run: serving(image, state), AppPort: appPort})
	if err != nil {
		checks.True("a third container serves from the same volume", false, err.Error())
		return
	}
	harness.StatusIs(t, third, "GET / from the third container answers 200", http.StatusOK, "/")
	last, ok := stopAndCopy(ctx, t, checks, e, third.C, "the third container")
	if !ok {
		return
	}
	same(checks, "the third container starts with the main key the first generated", mainKey, left, last)
	same(checks, "the third container leaves the database the first start left", database, left, last)
}

// stopAndCopy stops c, named what, checks that it exits 0, and returns
// what its state directory holds then, once everything Pisati holds open
// is closed: its write-ahead log is then in the database itself. It says
// whether there was a state directory to read.
func stopAndCopy(ctx context.Context, tb testing.TB, checks *harness.Checks, e harness.Engine, c *harness.Container, what string) (map[string]copied, bool) {
	tb.Helper()
	out, _, err := stop(ctx, e, c)
	checks.True(what+" stops", err == nil, string(out))
	code, err := c.WaitExit(ctx, time.Second)
	if err != nil {
		checks.True(what+" exits", false, err.Error())
		return nil, false
	}
	harness.Equal(checks, what+" exits 0", 0, code)
	files, err := copyOut(ctx, e, c, stateDir)
	if err != nil {
		checks.True("the state directory "+what+" left can be read", false, err.Error())
		return nil, false
	}
	return files, true
}

// same checks that the file name, in the state directory, is in both want
// and got, not empty, and the same bytes in each. Its detail gives sizes
// alone, never the content, which holds the main key.
func same(checks *harness.Checks, what, name string, want, got map[string]copied) {
	w, inWant := want[name]
	g, inGot := got[name]
	ok := inWant && inGot && len(w.data) > 0 && bytes.Equal(w.data, g.data)
	found := fmt.Sprintf("the same %d bytes", len(g.data))
	switch {
	case !inWant || !inGot:
		found = fmt.Sprintf("%s in the first: %t; in the second: %t", name, inWant, inGot)
	case !ok:
		found = fmt.Sprintf("%d bytes, which differ from the %d the first left", len(g.data), len(w.data))
	}
	checks.True(what, ok, found)
}

// listed returns the names files holds, sorted, for a check's detail.
func listed(files map[string]copied) string {
	return strings.Join(slices.Sorted(maps.Keys(files)), ", ")
}
