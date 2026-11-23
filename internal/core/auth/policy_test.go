// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package auth_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/divbast/pisati/internal/core/auth"
)

func TestCheckPassword(t *testing.T) {
	for _, c := range []struct {
		name  string
		pw    string
		words []string
		want  error
	}{
		{"no words", "a password nobody listed", nil, nil},
		{"words it lacks", "a password nobody listed", []string{"alice", "example.org", ""}, nil},
		{"the user name inside it", "my alice password 1", []string{"alice"}, auth.ErrContextWord},
		{"the user name in capitals", "my ALICE password 1", []string{"alice"}, auth.ErrContextWord},
		{"the user name given in capitals", "my alice password 1", []string{"ALICE"}, auth.ErrContextWord},
		{"any of the words", "pisati is my blog name", []string{"alice", "Pisati"}, auth.ErrContextWord},
		{"the whole password is the word", "wonderland-alice", []string{"Wonderland-Alice"}, auth.ErrContextWord},
		{"a word longer than the password", "wonderland-alice", []string{"wonderland-alice!"}, nil},
		{"a word of 3 runes is ignored", "bobbobbobbobbobbob", []string{"bob"}, nil},
		{"a word of 4 runes counts", "bobsbobsbobsbobs!", []string{"bobs"}, auth.ErrContextWord},
		{"a word of 3 runes in 9 bytes is ignored", "日本語日本語日本語日本語日本語", []string{"日本語"}, nil},
		{"a word of 4 runes in 12 bytes counts", "私の日本語版のパスワードはこれです", []string{"日本語版"}, auth.ErrContextWord},
		{"Unicode case folding: Greek", "ο κωδικός μου ΩΜΕΓΑ 1", []string{"ωμεγα"}, auth.ErrContextWord},
		{"Unicode case folding: the long s", "open ſeſame, my friend", []string{"SESAME"}, auth.ErrContextWord},
		{"Unicode case folding: the Kelvin sign", "zero \u212aelvin is cold!", []string{"kelvin"}, auth.ErrContextWord},
		{"not normalised: \u00e9 as one rune does not match \u00e9 as two", "my caf\u00e9 password 1", []string{"cafe\u0301"}, nil},
	} {
		err := auth.CheckPassword(password(t, c.pw), c.words...)
		switch {
		case c.want == nil && err != nil:
			t.Errorf("%s: CheckPassword(%q, %q) = %v, want it accepted", c.name, c.pw, c.words, err)
		case c.want != nil && !errors.Is(err, c.want):
			t.Errorf("%s: CheckPassword(%q, %q) = %v, want %v", c.name, c.pw, c.words, err, c.want)
		}
		for _, w := range append(c.words, c.pw) {
			if err != nil && w != "" && strings.Contains(err.Error(), w) {
				t.Errorf("%s: CheckPassword's error %q repeats %q", c.name, err, w)
			}
		}
	}
	if err := auth.CheckPassword(auth.Password{}, "alice"); !errors.Is(err, auth.ErrPasswordTooShort) {
		t.Errorf("CheckPassword(the zero Password) = %v, want ErrPasswordTooShort", err)
	}
}
