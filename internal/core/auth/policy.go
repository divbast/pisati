// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package auth

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ErrContextWord is a password that contains one of the words it was
// checked with. Its message is fixed, and never holds the password or the
// word.
var ErrContextWord = errors.New("auth: password contains a word from its context")

// minContextRunes is the fewest characters a context word needs to count:
// a shorter one is too likely to turn up in a password by chance.
const minContextRunes = 4

// CheckPassword returns nil when the new password p may be used, and
// ErrContextWord when p contains one of contextWords, such as the user's
// name, e-mail address or the blog's title, ignoring case under Unicode
// simple case folding, as strings.EqualFold does, but not normalising. A
// word of fewer than 4 characters is ignored. It returns
// ErrPasswordTooShort for the zero Password.
func CheckPassword(p Password, contextWords ...string) error {
	if p.reveal == nil {
		return ErrPasswordTooShort
	}
	if containsWord(p.value(), contextWords) {
		return ErrContextWord
	}
	return nil
}

// containsWord reports whether s contains a word of words that has at
// least minContextRunes characters, ignoring case.
func containsWord(s string, words []string) bool {
	folded := fold(s)
	for _, w := range words {
		if utf8.RuneCountInString(w) >= minContextRunes && strings.Contains(folded, fold(w)) {
			return true
		}
	}
	return false
}

// fold maps each character of s to the least one in its case-folding
// orbit, so that two strings strings.EqualFold calls equal fold to the
// same string, character for character. strings.ToLower is not enough:
// it leaves ſ, which folds to s, as it is.
func fold(s string) string {
	return strings.Map(func(r rune) rune {
		least := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			least = min(least, f)
		}
		return least
	}, s)
}
