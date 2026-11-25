// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package web_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/divbast/pisati/internal/app"
	"github.com/divbast/pisati/internal/web/msg"
)

func TestWriteErrorExhaustive(t *testing.T) {
	t.Parallel()

	en := msg.English
	tests := []struct {
		name     string
		err      error
		target   string
		status   int
		heading  string
		body     string
		location string
		retry    string
		logged   bool
	}{
		{name: "NotFound", err: &app.NotFound{}, status: http.StatusNotFound, heading: msg.NotFoundTitle(en), body: msg.NotFoundBody(en)},
		{name: "Forbidden", err: &app.Forbidden{}, status: http.StatusForbidden, heading: msg.ForbiddenTitle(en), body: msg.ForbiddenBody(en)},
		{
			name: "Unauthenticated", err: &app.Unauthenticated{}, target: "/drafts?page=2",
			status: http.StatusSeeOther, location: "/login?next=%2Fdrafts%3Fpage%3D2",
		},
		{
			// A path the next rule refuses is not carried to the sign-in page.
			name: "Unauthenticated from a path next refuses", err: &app.Unauthenticated{}, target: "//evil.example/x",
			status: http.StatusSeeOther, location: "/login",
		},
		{
			name: "Invalid", err: &app.Invalid{Field: "slug-field-name", Reason: app.Malformed},
			status: http.StatusBadRequest, heading: msg.InvalidTitle(en), body: msg.InvalidReason(en, app.Malformed),
		},
		{
			name: "Invalid with no reason", err: &app.Invalid{Field: "slug-field-name"},
			status: http.StatusBadRequest, heading: msg.InvalidTitle(en), body: msg.InvalidReason(en, 0),
		},
		{name: "Conflict", err: &app.Conflict{}, status: http.StatusConflict, heading: msg.ConflictTitle(en), body: msg.ConflictBody(en)},
		{
			name: "RateLimited", err: &app.RateLimited{RetryAfter: 1500 * time.Millisecond},
			status: http.StatusTooManyRequests, heading: msg.TooManyTitle(en), body: msg.TooManyBody(en), retry: "2",
		},
		{
			name: "a wrapped Error", err: fmt.Errorf("load the post: %w", &app.NotFound{}),
			status: http.StatusNotFound, heading: msg.NotFoundTitle(en), body: msg.NotFoundBody(en),
		},
		{
			name: "any other error", err: errors.New("database is locked at /var/lib/pisati/pisati.db"),
			status: http.StatusInternalServerError, heading: msg.ServerErrorTitle(en), body: msg.ServerErrorBody(en), logged: true,
		},
		{
			name: "a nil error", err: nil,
			status: http.StatusInternalServerError, heading: msg.ServerErrorTitle(en), body: msg.ServerErrorBody(en), logged: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			p := newPisati(t, options{})
			target := tt.target
			if target == "" {
				target = "/somewhere"
			}
			w := httptest.NewRecorder()
			r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
			p.site.WriteError(w, r, tt.err)

			if w.Code != tt.status {
				t.Fatalf("status %d, want %d", w.Code, tt.status)
			}
			if got := w.Header().Get("Location"); got != tt.location {
				t.Errorf("Location %q, want %q", got, tt.location)
			}
			if got := w.Header().Get("Retry-After"); got != tt.retry {
				t.Errorf("Retry-After %q, want %q", got, tt.retry)
			}
			if tt.heading != "" {
				doc := parse(t, w)
				if got := text(one(t, all(doc, "h1"), "headings")); got != tt.heading {
					t.Errorf("heading %q, want %q", got, tt.heading)
				}
				if got, want := text(one(t, all(doc, "title"), "titles")), msg.PageTitle(en, tt.heading); got != want {
					t.Errorf("title %q, want %q", got, want)
				}
				if !strings.Contains(text(doc), tt.body) {
					t.Errorf("the page does not say %q", tt.body)
				}
				one(t, byAttr(one(t, all(doc, "main"), "mains"), "a", "href", "/"), "links home in main")
			}
			// The page tells the visitor what happened in the catalogue's
			// words alone: never what they sent, nor what failed inside.
			for _, leak := range []string{"slug-field-name", "database", "/var/lib"} {
				if strings.Contains(w.Body.String(), leak) {
					t.Errorf("the page shows %q", leak)
				}
			}
			recs := p.records()
			switch {
			case tt.logged && len(recs) != 1:
				t.Errorf("%d records, want the failure logged once: %v", len(recs), recs)
			case tt.logged && (recs[0]["level"] != "ERROR" || recs[0]["msg"] != "request answered with status 500, because Pisati could not complete it"):
				t.Errorf("logged %v, want request failed at ERROR", recs[0])
			case !tt.logged && len(recs) != 0:
				t.Errorf("logged %v, want nothing for an error the visitor can act on", recs)
			}
		})
	}
}
