// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

// Package msg is Pisati's message catalogue: every text a page shows a
// visitor, one function per message, each giving the message in the
// language it is asked for. Templates take their text from here alone,
// so that each message is written once, and a language that lacks one
// fails to build rather than show a page without it.
//
// English is the only language yet. Messages that count something, and
// so need plural forms, arrive with golang.org/x/text's plural rules when
// the first of them does.
package msg

import "github.com/divbast/pisati/internal/app"

// Lang is a language Pisati's pages are written in.
type Lang uint8

// English is the language every message is written in first, and so the
// zero Lang: a page that names no language is in English.
const English Lang = 0

// text returns l's catalogue. English is the only one, so every Lang,
// English and any value no constant names alike, gets it.
func (Lang) text() english {
	return english{}
}

// Code returns l's language tag, as a page's lang attribute takes it: en
// for English.
func (l Lang) Code() string { return l.text().code() }

// SiteName is the site's name, as the header shows it and every page's
// title ends with it.
func SiteName(l Lang) string { return l.text().siteName() }

// PageTitle is a page's title: page, the page's own name, and the site's
// name after it.
func PageTitle(l Lang, page string) string { return l.text().pageTitle(page) }

// SkipToContent is the link a keyboard reaches first, which skips the
// header to the page's content.
func SkipToContent(l Lang) string { return l.text().skipToContent() }

// ThemeLabel names the form that switches the theme, for a screen reader.
func ThemeLabel(l Lang) string { return l.text().themeLabel() }

// ThemeLight is the button that picks the light theme.
func ThemeLight(l Lang) string { return l.text().themeLight() }

// ThemeDark is the button that picks the dark theme.
func ThemeDark(l Lang) string { return l.text().themeDark() }

// ThemeAuto is the button that has the theme follow the visitor's system.
func ThemeAuto(l Lang) string { return l.text().themeAuto() }

// HomeBody is what the home page says while nothing is published.
func HomeBody(l Lang) string { return l.text().homeBody() }

// SignInLink is the home page's link to the sign-in page.
func SignInLink(l Lang) string { return l.text().signInLink() }

// SignInTitle is the sign-in page's name and heading.
func SignInTitle(l Lang) string { return l.text().signInTitle() }

// UsernameLabel labels the sign-in form's username field.
func UsernameLabel(l Lang) string { return l.text().usernameLabel() }

// PasswordLabel labels the sign-in form's password field.
func PasswordLabel(l Lang) string { return l.text().passwordLabel() }

// SignInButton is the sign-in form's button.
func SignInButton(l Lang) string { return l.text().signInButton() }

// NotAvailableTitle heads the page a feature that is not built yet
// answers with.
func NotAvailableTitle(l Lang) string { return l.text().notAvailableTitle() }

// NotAvailableBody says signing in is not built yet.
func NotAvailableBody(l Lang) string { return l.text().notAvailableBody() }

// NotFoundTitle heads the page a 404 shows.
func NotFoundTitle(l Lang) string { return l.text().notFoundTitle() }

// NotFoundBody says there is no page where the visitor asked.
func NotFoundBody(l Lang) string { return l.text().notFoundBody() }

// ForbiddenTitle heads the page a 403 shows.
func ForbiddenTitle(l Lang) string { return l.text().forbiddenTitle() }

// ForbiddenBody says the visitor may not do what they asked.
func ForbiddenBody(l Lang) string { return l.text().forbiddenBody() }

// InvalidTitle heads the page a 400 shows, above the reason InvalidReason
// gives.
func InvalidTitle(l Lang) string { return l.text().invalidTitle() }

// InvalidReason says which rule an input broke, by r. It never names the
// input or repeats it: a Reason holds neither. The zero Reason, and any
// value no constant names, get a text of their own, which says only that
// an input could not be used.
func InvalidReason(l Lang, r app.Reason) string { return l.text().invalidReason(r) }

// ConflictTitle heads the page a 409 shows.
func ConflictTitle(l Lang) string { return l.text().conflictTitle() }

// ConflictBody says the change asked for clashes with what is there.
func ConflictBody(l Lang) string { return l.text().conflictBody() }

// TooManyTitle heads the page a 429 shows.
func TooManyTitle(l Lang) string { return l.text().tooManyTitle() }

// TooManyBody asks the visitor to wait before trying again.
func TooManyBody(l Lang) string { return l.text().tooManyBody() }

// ServerErrorTitle heads the page a 500 shows.
func ServerErrorTitle(l Lang) string { return l.text().serverErrorTitle() }

// ServerErrorBody says the server failed, and nothing of how.
func ServerErrorBody(l Lang) string { return l.text().serverErrorBody() }

// BackHome is every error page's link to the home page.
func BackHome(l Lang) string { return l.text().backHome() }
