// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package msg

import "github.com/divbast/pisati/internal/app"

// english is the English catalogue: each message's text, as msg.go's
// function of the same name gives it.
type english struct{}

func (english) code() string     { return "en" }
func (english) siteName() string { return "Pisati" }

func (e english) pageTitle(page string) string { return page + " · " + e.siteName() }

func (english) skipToContent() string     { return "Skip to content" }
func (english) themeLabel() string        { return "Theme" }
func (english) themeLight() string        { return "Light" }
func (english) themeDark() string         { return "Dark" }
func (english) themeAuto() string         { return "Auto" }
func (english) homeBody() string          { return "Nothing has been published here yet." }
func (english) signInLink() string        { return "Sign in" }
func (english) signInTitle() string       { return "Sign in" }
func (english) usernameLabel() string     { return "Username" }
func (english) passwordLabel() string     { return "Password" }
func (english) signInButton() string      { return "Sign in" }
func (english) notAvailableTitle() string { return "Not available yet" }
func (english) notAvailableBody() string  { return "Signing in isn't available yet." }
func (english) notFoundTitle() string     { return "Page not found" }
func (english) notFoundBody() string      { return "There's no page at this address." }
func (english) forbiddenTitle() string    { return "Not allowed" }
func (english) forbiddenBody() string     { return "You don't have permission to do that." }
func (english) invalidTitle() string      { return "Check what you entered" }
func (english) conflictTitle() string     { return "Can't make that change" }
func (english) conflictBody() string      { return "It conflicts with something that already exists." }
func (english) tooManyTitle() string      { return "Too many requests" }
func (english) tooManyBody() string       { return "Wait a moment, then try again." }
func (english) serverErrorTitle() string  { return "Something went wrong" }
func (english) serverErrorBody() string {
	return "The server couldn't handle this request. Try again later."
}
func (english) backHome() string { return "Go to the home page" }

func (english) invalidReason(r app.Reason) string {
	switch r {
	case app.Required:
		return "Something required was left empty."
	case app.TooShort:
		return "Something you entered is too short."
	case app.TooLong:
		return "Something you entered is too long."
	case app.NotAllowed:
		return "Something you entered isn't allowed."
	case app.Malformed:
		return "Something you entered isn't in the right form."
	}
	return "Something you entered can't be used."
}
