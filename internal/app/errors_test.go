// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package app_test

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/divbast/pisati/internal/app"
)

// statusOf maps err as a transport does: it finds the app.Error in err's
// chain and switches over every variant, and anything else is the
// server's fault. gochecksumtype fails the lint when a variant is missing
// from the switch, and the test fails when one reaches the default.
func statusOf(t *testing.T, err error) int {
	t.Helper()
	e, ok := errors.AsType[app.Error](err)
	if !ok {
		return http.StatusInternalServerError
	}
	switch e := e.(type) {
	case *app.NotFound:
		return http.StatusNotFound
	case *app.Forbidden:
		return http.StatusForbidden
	case *app.Unauthenticated:
		return http.StatusUnauthorized
	case *app.Invalid:
		return http.StatusBadRequest
	case *app.Conflict:
		return http.StatusConflict
	case *app.RateLimited:
		return http.StatusTooManyRequests
	default:
		t.Errorf("an app.Error the switch does not know: %T", e)
		return 0
	}
}

func TestErrorSwitchIsExhaustive(t *testing.T) {
	for _, tc := range []struct {
		err     app.Error
		status  int
		message string
	}{
		{err: &app.NotFound{}, status: http.StatusNotFound, message: "the page or the item asked for does not exist"},
		{err: &app.Forbidden{}, status: http.StatusForbidden, message: "the signed-in user may not do this"},
		{
			err: &app.Unauthenticated{}, status: http.StatusUnauthorized,
			message: "this needs a signed-in user, and the request comes from none",
		},
		{
			err:     &app.Invalid{Field: "title", Reason: app.TooLong},
			status:  http.StatusBadRequest,
			message: "the input title is too long",
		},
		{
			err: &app.Conflict{}, status: http.StatusConflict,
			message: "the change does not fit what is there already, such as a name that is taken already",
		},
		{
			err:     &app.RateLimited{RetryAfter: 30 * time.Second},
			status:  http.StatusTooManyRequests,
			message: "the request came too soon after others from the same place: try again after 30s",
		},
	} {
		t.Run(fmt.Sprintf("%T", tc.err), func(t *testing.T) {
			if got := tc.err.Error(); got != tc.message {
				t.Errorf("Error() = %q, want %q", got, tc.message)
			}
			for name, err := range map[string]error{
				"bare":    tc.err,
				"wrapped": fmt.Errorf("publish a post: %w", tc.err),
				"joined":  errors.Join(errors.New("another failure"), fmt.Errorf("outer: %w", tc.err)),
			} {
				if got := statusOf(t, err); got != tc.status {
					t.Errorf("%s: status %d, want %d", name, got, tc.status)
				}
			}
		})
	}

	t.Run("anything else", func(t *testing.T) {
		for _, err := range []error{
			errors.New("disk full"),
			fmt.Errorf("store: %w", errors.New("database is locked")),
		} {
			if got := statusOf(t, err); got != http.StatusInternalServerError {
				t.Errorf("%v: status %d, want 500", err, got)
			}
		}
	})

	t.Run("the fields reach the caller", func(t *testing.T) {
		err := fmt.Errorf("sign in: %w", &app.RateLimited{RetryAfter: 90 * time.Second})
		if rl, ok := errors.AsType[*app.RateLimited](err); !ok || rl.RetryAfter != 90*time.Second {
			t.Errorf("AsType[*RateLimited] = %v, %t; want RetryAfter 1m30s", rl, ok)
		}
		const slug app.Field = "slug"
		err = fmt.Errorf("save: %w", &app.Invalid{Field: slug, Reason: app.Malformed})
		if inv, ok := errors.AsType[*app.Invalid](err); !ok || inv.Field != slug || inv.Reason != app.Malformed {
			t.Errorf("AsType[*Invalid] = %v, %t", inv, ok)
		}
	})

	t.Run("nil variants", func(t *testing.T) {
		// A typed nil pointer is still an error whose message does not
		// panic.
		for _, err := range []app.Error{
			(*app.NotFound)(nil), (*app.Forbidden)(nil), (*app.Unauthenticated)(nil),
			(*app.Invalid)(nil), (*app.Conflict)(nil), (*app.RateLimited)(nil),
		} {
			if msg := err.Error(); msg == "" || strings.HasPrefix(msg, "app: ") {
				t.Errorf("%T(nil).Error() = %q, want a message in plain words", err, msg)
			}
		}
	})
}

// message maps r to what a user is shown, as a transport's message
// catalogue does: a switch over every Reason, which the exhaustive linter
// holds to the whole set.
func message(t *testing.T, r app.Reason) string {
	t.Helper()
	switch r {
	case app.Required:
		return "Fill this in."
	case app.TooShort:
		return "This is too short."
	case app.TooLong:
		return "This is too long."
	case app.NotAllowed:
		return "This is not allowed."
	case app.Malformed:
		return "This is not in the right form."
	}
	t.Errorf("a Reason the catalogue does not know: %d", r)
	return ""
}

func TestReasonsAreAClosedSet(t *testing.T) {
	seen := map[string]app.Reason{}
	for _, r := range []app.Reason{app.Required, app.TooShort, app.TooLong, app.NotAllowed, app.Malformed} {
		name := r.String()
		if name == "" || name == "is refused, for no reason given" {
			t.Errorf("Reason %d has the name %q", r, name)
		}
		if other, ok := seen[name]; ok {
			t.Errorf("Reasons %d and %d share the name %q", other, r, name)
		}
		seen[name] = r
		if message(t, r) == "" {
			t.Errorf("Reason %s has no message", r)
		}
		err := &app.Invalid{Field: "title", Reason: r}
		if want := "the input title " + name; err.Error() != want {
			t.Errorf("Error() = %q, want %q", err.Error(), want)
		}
	}
	// The zero Reason, and any outside the set, is unspecified.
	for _, r := range []app.Reason{0, 6, 255} {
		if got := r.String(); got != "is refused, for no reason given" {
			t.Errorf("Reason(%d).String() = %q, want the unspecified reason", r, got)
		}
	}
	if got := (&app.Invalid{Field: "title"}).Error(); got != "the input title is refused, for no reason given" {
		t.Errorf("Invalid without a Reason: %q", got)
	}
}

func TestRetryAfterSeconds(t *testing.T) {
	for _, tc := range []struct {
		after time.Duration
		want  int64
	}{
		{after: -5 * time.Second, want: 0},
		{after: -1, want: 0},
		{after: 0, want: 0},
		{after: 1, want: 1},
		{after: time.Second, want: 1},
		{after: 1500 * time.Millisecond, want: 2},
		{after: 90 * time.Second, want: 90},
		{after: time.Duration(math.MaxInt64), want: math.MaxInt64/int64(time.Second) + 1},
	} {
		if got := (&app.RateLimited{RetryAfter: tc.after}).RetryAfterSeconds(); got != tc.want {
			t.Errorf("RetryAfterSeconds(%v) = %d, want %d", tc.after, got, tc.want)
		}
	}
	if got := (*app.RateLimited)(nil).RetryAfterSeconds(); got != 0 {
		t.Errorf("nil RetryAfterSeconds() = %d, want 0", got)
	}
}
