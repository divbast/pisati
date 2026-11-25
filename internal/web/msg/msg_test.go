// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package msg_test

import (
	"strings"
	"testing"

	"github.com/divbast/pisati/internal/app"
	"github.com/divbast/pisati/internal/web/msg"
)

// messages is every message without arguments, by its function's name.
var messages = map[string]func(msg.Lang) string{
	"SiteName":          msg.SiteName,
	"SkipToContent":     msg.SkipToContent,
	"ThemeLabel":        msg.ThemeLabel,
	"ThemeLight":        msg.ThemeLight,
	"ThemeDark":         msg.ThemeDark,
	"ThemeAuto":         msg.ThemeAuto,
	"HomeBody":          msg.HomeBody,
	"SignInLink":        msg.SignInLink,
	"SignInTitle":       msg.SignInTitle,
	"UsernameLabel":     msg.UsernameLabel,
	"PasswordLabel":     msg.PasswordLabel,
	"SignInButton":      msg.SignInButton,
	"NotAvailableTitle": msg.NotAvailableTitle,
	"NotAvailableBody":  msg.NotAvailableBody,
	"NotFoundTitle":     msg.NotFoundTitle,
	"NotFoundBody":      msg.NotFoundBody,
	"ForbiddenTitle":    msg.ForbiddenTitle,
	"ForbiddenBody":     msg.ForbiddenBody,
	"InvalidTitle":      msg.InvalidTitle,
	"ConflictTitle":     msg.ConflictTitle,
	"ConflictBody":      msg.ConflictBody,
	"TooManyTitle":      msg.TooManyTitle,
	"TooManyBody":       msg.TooManyBody,
	"ServerErrorTitle":  msg.ServerErrorTitle,
	"ServerErrorBody":   msg.ServerErrorBody,
	"BackHome":          msg.BackHome,
}

func TestEveryMessageHasText(t *testing.T) {
	t.Parallel()

	for name, text := range messages {
		got := text(msg.English)
		if got == "" || strings.TrimSpace(got) != got {
			t.Errorf("%s(English) = %q, want text with no space at either end", name, got)
		}
	}
}

func TestEnglishIsTheZeroLang(t *testing.T) {
	t.Parallel()

	var zero msg.Lang
	if zero != msg.English {
		t.Errorf("the zero Lang is %v, want English", zero)
	}
	if got := msg.English.Code(); got != "en" {
		t.Errorf("English.Code() = %q, want en", got)
	}
}

func TestPageTitle(t *testing.T) {
	t.Parallel()

	if got, want := msg.PageTitle(msg.English, "Sign in"), "Sign in · Pisati"; got != want {
		t.Errorf("PageTitle(English, %q) = %q, want %q", "Sign in", got, want)
	}
}

func TestInvalidReasonGivesEachReasonItsOwnText(t *testing.T) {
	t.Parallel()

	reasons := []app.Reason{app.Required, app.TooShort, app.TooLong, app.NotAllowed, app.Malformed}
	seen := map[string]app.Reason{}
	for _, r := range reasons {
		got := msg.InvalidReason(msg.English, r)
		if got == "" {
			t.Errorf("InvalidReason(English, %v) is empty", r)
		}
		if other, ok := seen[got]; ok {
			t.Errorf("InvalidReason gives %v and %v the same text, %q", other, r, got)
		}
		seen[got] = r
	}
	// The zero Reason, and one outside the set, get a text of their own
	// that no rule has.
	for _, r := range []app.Reason{0, app.Malformed + 1} {
		got := msg.InvalidReason(msg.English, r)
		if got == "" {
			t.Errorf("InvalidReason(English, %d) is empty", r)
		}
		if other, ok := seen[got]; ok {
			t.Errorf("InvalidReason(English, %d) = %q, the text of %v", r, got, other)
		}
	}
}
