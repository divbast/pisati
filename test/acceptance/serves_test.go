// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package acceptance_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/divbast/ci/harness"
)

// htmlType is the Content-Type of every page the site renders.
const htmlType = "text/html; charset=utf-8"

// unknown are paths the app port answers with its 404 page: a PHP app's
// script by its own name, /healthz, which only the health port serves,
// and a path nothing names.
var unknown = []string{"/index.php", "/healthz", "/nope"}

// TestServes checks the app port, from a client outside the container,
// through the published port: GET / answers 200 with the home page, and
// every path the site has no page for answers 404 with its 404 page,
// HTML like every other page, never an empty body or another listener's
// answer.
func TestServes(t *testing.T) {
	t.Parallel()
	e, image := underTest(t)
	ctx := t.Context()

	s, err := e.Serve(ctx, t, &harness.ServeSpec{Run: serving(image, stateMount(t, e)), AppPort: appPort})
	checks := harness.NewChecks(t)
	if err != nil {
		checks.True("GET / from the host gets an answer", false, err.Error())
		return
	}

	resp, err := s.Get(ctx, "/", nil)
	harness.Answers(t, "GET / from the host answers 200", http.StatusOK, resp, err)
	if err == nil {
		isPage(checks, "GET / from the host", resp)
	}

	for _, path := range unknown {
		what := "GET " + path + " from the host"
		resp, err = s.Get(ctx, path, nil)
		harness.Answers(t, what+" answers 404", http.StatusNotFound, resp, err)
		if err == nil {
			isPage(checks, what, resp)
		}
	}
}

// isPage checks that resp, the answer to what, is one of the site's HTML
// pages: its Content-Type, and a body that is an HTML document.
func isPage(checks *harness.Checks, what string, resp harness.Response) {
	harness.Equal(checks, what+" is HTML", htmlType, resp.Header.Get("Content-Type"))
	body := string(resp.Body)
	checks.True(what+" is an HTML document", strings.HasPrefix(strings.ToLower(body), "<!doctype html>"), harness.Clip(body))
}
