// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package httpx_test

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"testing"

	"github.com/divbast/pisati/internal/config"
	"github.com/divbast/pisati/internal/httpx"
)

// panicky serves the paths TestRecoverReturns500AndKeepsServing asks for.
func panicky(t *testing.T) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ok", func(w http.ResponseWriter, _ *http.Request) {
		if _, err := io.WriteString(w, "ok"); err != nil {
			t.Errorf("writing /ok: %v", err)
		}
	})
	mux.HandleFunc("GET /value", func(w http.ResponseWriter, _ *http.Request) {
		// Headers for a response the handler never gets to send.
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Content-Length", "1000")
		panic("the request's own words")
	})
	mux.HandleFunc("GET /runtime", func(w http.ResponseWriter, r *http.Request) {
		digits := "0123"
		w.Header().Set("X-Digit", digits[len(r.URL.Path):]) // "/runtime" is longer
	})
	mux.HandleFunc("GET /partial", func(w http.ResponseWriter, _ *http.Request) {
		if _, err := io.WriteString(w, "partial"); err != nil {
			t.Errorf("writing /partial: %v", err)
		}
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("flushing /partial: %v", err)
		}
		panic("after the response began")
	})
	mux.HandleFunc("GET /abort", func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	})
	return mux
}

func TestRecoverReturns500AndKeepsServing(t *testing.T) {
	t.Parallel()

	t.Run("before the response", func(t *testing.T) {
		t.Parallel()

		for _, tt := range []struct{ path, panic string }{
			{path: "/value", panic: "a value of type string"},
			{path: "/runtime", panic: "runtime error: slice bounds out of range [8:4]"},
		} {
			t.Run(tt.path, func(t *testing.T) {
				t.Parallel()

				log, buf := newLog()
				// In the order Pisati chains them: the 500 is written past
				// SecurityHeaders' writer.
				srv := serve(t, httpx.Chain(panicky(t), httpx.RequestID(), httpx.Recover(log),
					httpx.SecurityHeaders(baseURL(t, "http://localhost:8080"), config.HSTSHostOnly)))

				var reused []bool
				trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
					reused = append(reused, info.Reused)
				}}
				status, header, body := send(t, srv, request(t, srv, http.MethodGet, tt.path, http.NoBody).
					WithContext(httptrace.WithClientTrace(t.Context(), trace)))
				if status != http.StatusInternalServerError || body != "Internal Server Error\n" {
					t.Errorf("GET %s = %d %q, want 500 with the generic body", tt.path, status, body)
				}
				if got := header.Get("Content-Type"); got != "text/plain; charset=utf-8" {
					t.Errorf("GET %s Content-Type = %q, want text/plain; charset=utf-8", tt.path, got)
				}
				csp := header.Get("Content-Security-Policy")
				if want := strictCSP(nonceIn(csp)); csp != want || header.Get("Cross-Origin-Opener-Policy") != "same-origin" {
					t.Errorf("GET %s has Content-Security-Policy %q and COOP %q, want the strict policy and the other security headers",
						tt.path, csp, header.Get("Cross-Origin-Opener-Policy"))
				}

				records := buf.records(t)
				if len(records) != 1 {
					t.Fatalf("logged %d records, want 1: %s", len(records), buf)
				}
				rec := records[0]
				if rec["level"] != "ERROR" || rec["msg"] != "request handler panicked" {
					t.Errorf("logged %v %q, want ERROR \"request handler panicked\"", rec["level"], rec["msg"])
				}
				if id := header.Get("X-Request-ID"); id == "" || rec["request_id"] != id {
					t.Errorf("logged request_id %v, want the response's X-Request-ID %q", rec["request_id"], id)
				}
				if rec["method"] != http.MethodGet || rec["path"] != tt.path || rec["panic"] != tt.panic {
					t.Errorf("logged method %v, path %v, panic %v; want GET, %s, %q", rec["method"], rec["path"], rec["panic"], tt.path, tt.panic)
				}
				if stack, ok := rec["stack"].(string); !ok || !strings.Contains(stack, "recover_test.go") {
					t.Errorf("logged the stack %v, want the handler's frames", rec["stack"])
				}
				if strings.Contains(buf.String(), "the request's own words") {
					t.Errorf("logged the value the handler panicked with: %s", buf)
				}

				status, _, body = send(t, srv, request(t, srv, http.MethodGet, "/ok", http.NoBody).
					WithContext(httptrace.WithClientTrace(t.Context(), trace)))
				if status != http.StatusOK || body != "ok" {
					t.Errorf("GET /ok after the panic = %d %q, want 200 \"ok\"", status, body)
				}
				if len(reused) != 2 || !reused[1] {
					t.Errorf("connections reused: %v, want the second request on the first's connection", reused)
				}
			})
		}
	})

	t.Run("after the response began", func(t *testing.T) {
		t.Parallel()

		log, buf := newLog()
		srv := serve(t, httpx.Chain(panicky(t), httpx.RequestID(), httpx.Recover(log)))
		resp, err := srv.Client().Do(request(t, srv, http.MethodGet, "/partial", http.NoBody))
		if err != nil {
			t.Fatalf("GET /partial: %v", err)
		}
		body, err := io.ReadAll(resp.Body)
		if cerr := resp.Body.Close(); cerr != nil {
			t.Errorf("closing the body: %v", cerr)
		}
		if resp.StatusCode != http.StatusOK || string(body) != "partial" || !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("GET /partial = %d %q, %v; want the 200 it began, cut off with io.ErrUnexpectedEOF", resp.StatusCode, body, err)
		}
		if records := buf.records(t); len(records) != 1 || records[0]["msg"] != "request handler panicked" {
			t.Errorf("logged %s, want the one panic", buf)
		}
	})

	t.Run("http.ErrAbortHandler", func(t *testing.T) {
		t.Parallel()

		log, buf := newLog()
		srv := serve(t, httpx.Chain(panicky(t), httpx.RequestID(), httpx.Recover(log)))
		resp, err := srv.Client().Do(request(t, srv, http.MethodGet, "/abort", http.NoBody))
		if err == nil {
			t.Errorf("GET /abort = %d, want the connection cut", resp.StatusCode)
			if cerr := resp.Body.Close(); cerr != nil {
				t.Errorf("closing the body: %v", cerr)
			}
		}
		if s := buf.String(); s != "" {
			t.Errorf("logged %s, want nothing: the handler chose to abort", s)
		}
	})
}
