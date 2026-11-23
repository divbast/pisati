// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package app_test

import (
	"errors"
	"fmt"
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
		{err: &app.NotFound{}, status: http.StatusNotFound, message: "app: not found"},
		{err: &app.Forbidden{}, status: http.StatusForbidden, message: "app: forbidden"},
		{err: &app.Unauthenticated{}, status: http.StatusUnauthorized, message: "app: not signed in"},
		{
			err:     &app.Invalid{Field: "title", Reason: "it is longer than 200 characters"},
			status:  http.StatusBadRequest,
			message: "app: invalid title: it is longer than 200 characters",
		},
		{err: &app.Conflict{}, status: http.StatusConflict, message: "app: conflict"},
		{
			err:     &app.RateLimited{RetryAfter: 30 * time.Second},
			status:  http.StatusTooManyRequests,
			message: "app: rate limited; retry after 30s",
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
		err = fmt.Errorf("save: %w", &app.Invalid{Field: "slug", Reason: "it holds a space"})
		if inv, ok := errors.AsType[*app.Invalid](err); !ok || inv.Field != "slug" || inv.Reason != "it holds a space" {
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
			if msg := err.Error(); !strings.HasPrefix(msg, "app: ") {
				t.Errorf("%T(nil).Error() = %q", err, msg)
			}
		}
	})
}
