// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package explain_test

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"

	"github.com/divbast/pisati/internal/explain"
)

// errSentinel stands for a package's sentinel, which callers branch on.
var errSentinel = errors.New("sentinel")

// causes returns errors to explain: a sentinel, one wrapped by fmt, a
// typed error, which errors.AsType looks for, and two joined.
func causes() map[string]error {
	return map[string]error{
		"a sentinel":           errSentinel,
		"a wrapped sentinel":   fmt.Errorf("context: %w", errSentinel),
		"a typed error":        &fs.PathError{Op: "open", Path: "/k", Err: fs.ErrPermission},
		"two errors, joined":   errors.Join(errSentinel, fs.ErrExist),
		"a message with verbs": errors.New("100% of %d and %s"),
	}
}

// same reports whether a and b are the same error: each is the other, as
// errors.Is sees it, which holds for none of these causes and an error
// that wraps it.
func same(a, b error) bool {
	return errors.Is(a, b) && errors.Is(b, a)
}

// findsCause fails t unless err wraps cause, and what cause wraps, for
// errors.Is and errors.AsType.
func findsCause(t *testing.T, err, cause error) {
	t.Helper()
	if !errors.Is(err, cause) {
		t.Errorf("errors.Is(%q, %q) = false", err, cause)
	}
	if errors.Is(cause, errSentinel) && !errors.Is(err, errSentinel) {
		t.Errorf("errors.Is(%q, the sentinel cause wraps) = false", err)
	}
	if errors.Is(cause, fs.ErrPermission) {
		pe, ok := errors.AsType[*fs.PathError](err)
		if !ok || !same(pe, cause) {
			t.Errorf("errors.AsType[*fs.PathError](%q) = %v, %v; want the cause", err, pe, ok)
		}
		if !errors.Is(err, fs.ErrPermission) {
			t.Errorf("errors.Is(%q, fs.ErrPermission) = false", err)
		}
	}
}

func TestNew(t *testing.T) {
	t.Parallel()

	for name, cause := range causes() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			err := explain.New(cause, "Pisati cannot read %s: %s", "/k", "no reason")
			if got, want := err.Error(), "Pisati cannot read /k: no reason"; got != want {
				t.Errorf("message %q, want %q: the words alone, never the cause's", got, want)
			}
			if got := errors.Unwrap(err); !same(got, cause) {
				t.Errorf("errors.Unwrap = %v, want the cause", got)
			}
			findsCause(t, err, cause)
		})
	}
}

func TestWrap(t *testing.T) {
	t.Parallel()

	for name, cause := range causes() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			err := explain.Wrap(cause, "Pisati cannot open the database %s", "/s/pisati.db")
			want := "Pisati cannot open the database /s/pisati.db: " + cause.Error()
			if got := err.Error(); got != want {
				t.Errorf("message %q, want %q: the words, then the cause's message as it is", got, want)
			}
			// cmd/pisati reads a wrapped join's problems one a line by
			// the words before the cause's message, which it finds as
			// errors.Unwrap gives it.
			if got := errors.Unwrap(err); !same(got, cause) {
				t.Errorf("errors.Unwrap = %v, want the cause", got)
			}
			if words, ok := strings.CutSuffix(err.Error(), cause.Error()); !ok || words != "Pisati cannot open the database /s/pisati.db: " {
				t.Errorf("message %q does not end with the cause's", err)
			}
			findsCause(t, err, cause)
		})
	}
}

func TestNoCause(t *testing.T) {
	t.Parallel()

	for name, err := range map[string]error{
		"New":  explain.New(nil, "Pisati looked up %s, and found no address for it", "x.example"),
		"Wrap": explain.Wrap(nil, "Pisati looked up %s, and found no address for it", "x.example"),
	} {
		if got, want := err.Error(), "Pisati looked up x.example, and found no address for it"; got != want {
			t.Errorf("%s(nil): message %q, want %q", name, got, want)
		}
		if got := errors.Unwrap(err); got != nil {
			t.Errorf("%s(nil): errors.Unwrap = %v, want nil", name, got)
		}
		if errors.Is(err, errSentinel) {
			t.Errorf("%s(nil) wraps the sentinel", name)
		}
	}
}

// TestNotAJoin: a message of Pisati's own is one problem, whatever its
// cause joins. cmd/pisati counts an error as a join of separate problems
// only when it has Unwrap() []error and its message is theirs.
func TestNotAJoin(t *testing.T) {
	t.Parallel()

	err := explain.New(errors.Join(errSentinel, fs.ErrExist), "one problem")
	if _, ok := err.(interface{ Unwrap() []error }); ok {
		t.Errorf("%q has Unwrap() []error, so it could be taken for a join", err)
	}
}
