// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package acceptance_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/divbast/ci/harness"
)

// elsewhere is the origin of a page on another site.
const elsewhere = "https://elsewhere.example"

// themeForm is what the theme switch's form posts: the dark theme, and
// the home page to come back to.
const themeForm = "theme=dark&next=%2F"

// TestCrossSitePostRefused posts the site's forms as a browser does from a
// page on another site, and checks each is refused with 403 before its
// handler runs: the theme switch sets no cookie. A browser says where a
// request comes from with Sec-Fetch-Site, or, when it is too old to send
// that, with Origin alone, and either is refused. The same post from the
// site's own page, as a control, switches the theme.
func TestCrossSitePostRefused(t *testing.T) {
	t.Parallel()
	e, image := underTest(t)
	ctx := t.Context()

	s, err := e.Serve(ctx, t, &harness.ServeSpec{Run: serving(image, stateMount(t, e)), AppPort: appPort})
	checks := harness.NewChecks(t)
	if err != nil {
		checks.True("the container serves", false, err.Error())
		return
	}

	post := func(path string, header http.Header) (harness.Response, error) {
		h := header.Clone()
		h.Set("Content-Type", "application/x-www-form-urlencoded")
		return s.Do(ctx, nil, http.MethodPost, path, h, []byte(themeForm))
	}
	refused := []struct {
		what   string
		header http.Header
	}{
		{"from another site's page", http.Header{"Sec-Fetch-Site": {"cross-site"}, "Origin": {elsewhere}}},
		{"from another site's page, in a browser that sends only Origin", http.Header{"Origin": {elsewhere}}},
	}
	for _, path := range []string{"/_/theme", "/login"} {
		for _, r := range refused {
			what := "POST " + path + " " + r.what
			resp, err := post(path, r.header)
			harness.Answers(t, what+" answers 403", http.StatusForbidden, resp, err)
			if err == nil {
				harness.Equal(checks, what+" sets no cookie", "", resp.Header.Get("Set-Cookie"))
			}
		}
	}

	what := "POST /_/theme from the site's own page"
	resp, err := post("/_/theme", http.Header{"Sec-Fetch-Site": {"same-origin"}, "Origin": {baseURL}})
	harness.Answers(t, what+" answers 303", http.StatusSeeOther, resp, err)
	if err == nil {
		cookie := resp.Header.Get("Set-Cookie")
		checks.True(what+" sets the theme's cookie", strings.HasPrefix(cookie, "pisati-theme=dark;"), harness.Clip(cookie))
	}
}
