// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package server_test

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/divbast/pisati/internal/server"
)

func TestHealth(t *testing.T) {
	t.Parallel()

	tests := []struct {
		method, path string
		want         int
		body         string
	}{
		{method: http.MethodGet, path: "/healthz", want: http.StatusOK, body: "ok"},
		{method: http.MethodGet, path: "/", want: http.StatusNotFound},
		{method: http.MethodGet, path: "/index.php", want: http.StatusNotFound},
		{method: http.MethodPost, path: "/healthz", want: http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.method+tt.path, func(t *testing.T) {
			t.Parallel()

			rec := httptest.NewRecorder()
			server.Health().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), tt.method, tt.path, http.NoBody))
			if rec.Code != tt.want || rec.Body.String() != tt.body {
				t.Errorf("%s %s = %d %q, want %d %q", tt.method, tt.path, rec.Code, rec.Body.String(), tt.want, tt.body)
			}
		})
	}
}

// answering is a server that answers every request with status and body.
func answering(t *testing.T, status int, body string) string {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		if _, err := io.WriteString(w, body); err != nil {
			t.Errorf("answering %d %q: %v", status, body, err)
		}
	}))
	t.Cleanup(s.Close)
	return s.URL
}

// closedURL is a URL nothing listens on.
func closedURL(t *testing.T) string {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return "http://" + addr + "/"
}

func TestProbe(t *testing.T) {
	t.Parallel()

	app := httptest.NewServer(server.App())
	t.Cleanup(app.Close)
	health := httptest.NewServer(server.Health())
	t.Cleanup(health.Close)

	tests := []struct {
		name           string
		appURL, health string
		// wantLines is the error's text, a line for each half that failed,
		// each starting with the given prefix. None means no error.
		wantLines []string
	}{
		{name: "healthy", appURL: app.URL + "/", health: health.URL + "/healthz"},
		{
			name: "page-wrong", appURL: answering(t, http.StatusOK, "Hello from somewhere else"), health: health.URL + "/healthz",
			wantLines: []string{`page: GET / answered 200 "Hello from somewhere else", want 200 "Hello from pisati"`},
		},
		{
			name: "healthz-503", appURL: app.URL + "/", health: answering(t, http.StatusServiceUnavailable, "down"),
			wantLines: []string{"healthz: GET /healthz answered 503, want 200"},
		},
		{
			name: "both-unreachable", appURL: closedURL(t), health: closedURL(t),
			wantLines: []string{"healthz: ", "page: "},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := server.Probe(t.Context(), tt.appURL, tt.health)
			if len(tt.wantLines) == 0 {
				if err != nil {
					t.Errorf("Probe() = %q, want nil when healthy", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Probe() = nil, want an error starting %q", tt.wantLines)
			}
			lines := strings.Split(err.Error(), "\n")
			if len(lines) != len(tt.wantLines) {
				t.Fatalf("Probe() = %q, want one line for each half that failed: %q", lines, tt.wantLines)
			}
			for i, want := range tt.wantLines {
				if !strings.HasPrefix(lines[i], want) {
					t.Errorf("line %d = %q, want it to start %q", i+1, lines[i], want)
				}
			}
		})
	}
}
