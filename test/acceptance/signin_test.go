// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package acceptance_test

import (
	"context"
	"crypto/sha512"
	"encoding/base64"
	"html"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/divbast/ci/harness"
)

// TestSignInPageServed checks the sign-in page as a browser gets it from
// the image: its policy, which starts by refusing everything, its
// cross-origin isolation, and where the browser sends what the policy
// refuses. Its stylesheet and script tags each name a file the image
// serves, with the integrity value of exactly that file, which only a
// build whose manifest Pisati read can give: without one, the page
// renders without either.
func TestSignInPageServed(t *testing.T) {
	t.Parallel()
	e, image := underTest(t)
	ctx := t.Context()

	s, err := e.Serve(ctx, t, &harness.ServeSpec{Run: serving(image, stateMount(t, e)), AppPort: appPort})
	checks := harness.NewChecks(t)
	if err != nil {
		checks.True("GET /login from the host gets an answer", false, err.Error())
		return
	}

	resp, err := s.Get(ctx, "/login", nil)
	harness.Answers(t, "GET /login answers 200", http.StatusOK, resp, err)
	if err != nil {
		return
	}
	isPage(checks, "GET /login", resp)
	csp := resp.Header.Get("Content-Security-Policy")
	checks.True("the sign-in page's Content-Security-Policy starts default-src 'none'",
		strings.HasPrefix(csp, "default-src 'none';"), harness.Clip(csp))
	checks.True("the policy reports to the endpoint named csp", strings.HasSuffix(csp, "; report-to csp"), harness.Clip(csp))
	harness.Equal(checks, "the sign-in page is cross-origin isolated: Cross-Origin-Embedder-Policy",
		"require-corp", resp.Header.Get("Cross-Origin-Embedder-Policy"))
	harness.Equal(checks, "csp, in Reporting-Endpoints, is the base URL's /_/csp-report",
		`csp="`+baseURL+`/_/csp-report"`, resp.Header.Get("Reporting-Endpoints"))

	page := string(resp.Body)
	loadsBuilt(ctx, t, checks, s, page, "link", "href", map[string]string{"rel": "stylesheet"})
	loadsBuilt(ctx, t, checks, s, page, "script", "src", map[string]string{"type": "module"})
}

// Two regular expressions read the few elements loadsBuilt looks for
// from the page templ wrote: each element's start tag, and each of its
// attributes, as a name, with its value in double quotes when it has one.
var (
	startTag  = regexp.MustCompile(`(?is)<([a-z]+)\b([^>]*)>`)
	attribute = regexp.MustCompile(`(?is)([a-z][a-z-]*)(?:\s*=\s*"([^"]*)")?`)
)

// loadsBuilt checks that page has exactly one <tag> with every attribute
// in with, whose attribute src names a file under /static/ and whose
// integrity holds sha384- and a digest; and that the file s serves at src
// is exactly what that digest is of.
func loadsBuilt(ctx context.Context, tb testing.TB, checks *harness.Checks, s *harness.Serving, page, tag, src string, with map[string]string) {
	tb.Helper()
	what := "the sign-in page's <" + tag + ">"
	var found []map[string]string
	for _, m := range startTag.FindAllStringSubmatch(page, -1) {
		if !strings.EqualFold(m[1], tag) {
			continue
		}
		attrs := make(map[string]string)
		for _, a := range attribute.FindAllStringSubmatch(m[2], -1) {
			attrs[strings.ToLower(a[1])] = html.UnescapeString(a[2])
		}
		if matches(attrs, with) {
			found = append(found, attrs)
		}
	}
	if len(found) != 1 {
		checks.True("the sign-in page has one <"+tag+"> that loads the build", false, harness.Clip(page))
		return
	}
	url, sri := found[0][src], found[0]["integrity"]
	checks.True(what+" loads a file under /static/", strings.HasPrefix(url, "/static/"), harness.Clip(url))
	checks.True(what+" has an integrity value", strings.HasPrefix(sri, "sha384-"), harness.Clip(sri))
	if !strings.HasPrefix(url, "/static/") {
		return
	}
	resp, err := s.Get(ctx, url, nil)
	harness.Answers(tb, "GET "+url+", which "+what+" loads, answers 200", http.StatusOK, resp, err)
	if err != nil {
		return
	}
	sum := sha512.Sum384(resp.Body)
	harness.Equal(checks, what+"'s integrity value is the file's own", "sha384-"+base64.StdEncoding.EncodeToString(sum[:]), sri)
}

// matches reports whether attrs holds every attribute in with, with its
// value.
func matches(attrs, with map[string]string) bool {
	for name, value := range with {
		if got, ok := attrs[name]; !ok || got != value {
			return false
		}
	}
	return true
}
