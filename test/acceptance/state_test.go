// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package acceptance_test

import (
	"bytes"
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/divbast/ci/harness"
)

// written are the files Pisati keeps in the state directory, each of
// which it writes on its first start: the database, the lock a running
// Pisati holds, and the main key it generates when none is configured.
var written = []string{"pisati.db", "pisati.lock", "keys/main.key"}

// mainKey is where in the state directory Pisati keeps the main key it
// generates.
const mainKey = "keys/main.key"

// migrated is what pisati migrate prints: the schema's number before and
// after.
var migrated = regexp.MustCompile(`^schema (\d+) -> (\d+)\n$`)

// TestStatePersists serves from a fresh volume, and checks what Pisati
// wrote there is its user's alone: every file 0600, every directory 0700.
// It stops that container, and runs pisati migrate in a new one on the
// volume, which finds the database the first left, already at the schema
// it migrates to. It then serves the volume again from a third container,
// which starts with the main key the first generated.
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

	out, _, err := stop(ctx, e, first.C)
	checks.True("the first container stops", err == nil, string(out))
	code, err := first.C.WaitExit(ctx, time.Second)
	if err != nil {
		checks.True("the first container exits", false, err.Error())
		return
	}
	harness.Equal(checks, "the first container exits 0", 0, code)

	spec := serving(image, state)
	spec.Network = "none"
	spec.Entrypoint = []string{binary, "migrate"}
	res, err := runToExit(ctx, t, e, &spec)
	if err != nil {
		checks.True("pisati migrate runs on the volume", false, err.Error())
		return
	}
	harness.Equal(checks, "pisati migrate exits 0", 0, res.Code)
	before, after, ok := schemas(string(res.Stdout))
	checks.True("pisati migrate prints the schema before and after", ok, harness.Clip(string(res.Stdout)+string(res.Stderr)))
	if ok {
		checks.True("pisati migrate finds the database the first start migrated, at the schema it migrates to",
			before == after && before > 0, fmt.Sprintf("schema %d -> %d", before, after))
	}

	third, err := e.Serve(ctx, t, &harness.ServeSpec{Run: serving(image, state), AppPort: appPort})
	if err != nil {
		checks.True("a third container serves from the same volume", false, err.Error())
		return
	}
	harness.StatusIs(t, third, "GET / from the third container answers 200", http.StatusOK, "/")
	again, err := copyOut(ctx, e, third.C, stateDir)
	if err != nil {
		checks.True("the third container's state directory can be read", false, err.Error())
		return
	}
	// A check's detail says what it found, and never the key itself.
	key, kept := again[mainKey]
	same := kept && len(key.data) > 0 && bytes.Equal(key.data, files[mainKey].data)
	found := fmt.Sprintf("the same %d bytes", len(key.data))
	switch {
	case !kept:
		found = "no " + mainKey
	case !same:
		found = "another key, or an empty file"
	}
	checks.True("the third container starts with the main key the first generated", same, found)
}

// schemas returns the schema's numbers before and after in out, what
// pisati migrate printed, and whether out is that one line.
func schemas(out string) (before, after int, ok bool) {
	m := migrated.FindStringSubmatch(out)
	if m == nil {
		return 0, 0, false
	}
	before, errBefore := strconv.Atoi(m[1])
	after, errAfter := strconv.Atoi(m[2])
	return before, after, errBefore == nil && errAfter == nil
}

// listed returns the names files holds, sorted, for a check's detail.
func listed(files map[string]copied) string {
	return strings.Join(slices.Sorted(maps.Keys(files)), ", ")
}
