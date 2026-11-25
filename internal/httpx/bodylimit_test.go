// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package httpx_test

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/divbast/pisati/internal/httpx"
)

// unsized hides its reader's length, so a client sends it chunked.
type unsized struct{ io.Reader }

// reading answers 200 with how many bytes of body it read, or what
// RefuseTooLarge makes of its error. It counts the requests it sees.
func reading(t *testing.T, calls *atomic.Int64) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			if httpx.RefuseTooLarge(w, err) {
				return
			}
			t.Errorf("reading the body: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if _, err := io.WriteString(w, strconv.Itoa(len(body))); err != nil {
			t.Errorf("writing the answer: %v", err)
		}
	})
}

func TestBodyLimit413(t *testing.T) {
	t.Parallel()

	const limit = 1024
	tests := []struct {
		name    string
		limit   httpx.ByteLimit
		size    int
		chunked bool
		want    int
		// reached is whether the handler runs: a declared length over the
		// limit is refused before it.
		reached bool
	}{
		{name: "declared, at the limit", limit: limit, size: limit, want: http.StatusOK, reached: true},
		{name: "declared, over the limit", limit: limit, size: limit + 1, want: http.StatusRequestEntityTooLarge},
		{name: "chunked, at the limit", limit: limit, size: limit, chunked: true, want: http.StatusOK, reached: true},
		{name: "chunked, over the limit", limit: limit, size: limit + 1, chunked: true, want: http.StatusRequestEntityTooLarge, reached: true},
		{name: "zero, no body", limit: 0, size: 0, want: http.StatusOK, reached: true},
		{name: "zero, a byte", limit: 0, size: 1, want: http.StatusRequestEntityTooLarge},
		{name: "zero, a chunked byte", limit: 0, size: 1, chunked: true, want: http.StatusRequestEntityTooLarge, reached: true},
		{name: "negative, no body", limit: -1, size: 0, want: http.StatusOK, reached: true},
		{name: "negative, a byte", limit: -1, size: 1, want: http.StatusRequestEntityTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var calls atomic.Int64
			srv := serve(t, httpx.Chain(reading(t, &calls), httpx.BodyLimit(tt.limit)))
			var body io.Reader = http.NoBody
			if tt.size > 0 {
				body = strings.NewReader(strings.Repeat("x", tt.size))
				if tt.chunked {
					body = unsized{body}
				}
			}
			req := request(t, srv, http.MethodPost, "/", body)
			status, header, got := send(t, srv, req)

			want := strconv.Itoa(tt.size)
			if tt.want == http.StatusRequestEntityTooLarge {
				want = "Request Entity Too Large\n"
				if ct := header.Get("Content-Type"); ct != "text/plain; charset=utf-8" {
					t.Errorf("413 Content-Type = %q, want text/plain; charset=utf-8", ct)
				}
			}
			if status != tt.want || got != want {
				t.Errorf("POST %d bytes (chunked %t) under %d = %d %q, want %d %q", tt.size, tt.chunked, tt.limit, status, got, tt.want, want)
			}
			if reached := calls.Load() == 1; reached != tt.reached {
				t.Errorf("the handler ran: %t, want %t", reached, tt.reached)
			}
		})
	}

	t.Run("RefuseTooLarge on another error", func(t *testing.T) {
		t.Parallel()

		rec := httptest.NewRecorder()
		if httpx.RefuseTooLarge(rec, errors.New("the client went away")) {
			t.Error("RefuseTooLarge(an error not from the limit) = true, want false")
		}
		if rec.Code != http.StatusOK || rec.Body.Len() != 0 || len(rec.Header()) != 0 {
			t.Errorf("RefuseTooLarge(an error not from the limit) wrote %d %q %v, want nothing", rec.Code, rec.Body, rec.Header())
		}
		if httpx.RefuseTooLarge(rec, nil) {
			t.Error("RefuseTooLarge(nil) = true, want false")
		}

		rec = httptest.NewRecorder()
		wrapped := fmt.Errorf("parsing the form: %w", &http.MaxBytesError{Limit: 1})
		if !httpx.RefuseTooLarge(rec, wrapped) || rec.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("RefuseTooLarge(a wrapped *http.MaxBytesError) answered %d, want 413", rec.Code)
		}
	})
}

func TestBodyLimitPresets(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name      string
		got, want httpx.ByteLimit
	}{
		{name: "FormLimit", got: httpx.FormLimit, want: 64 << 10},
		{name: "JSONLimit", got: httpx.JSONLimit, want: 1 << 20},
		{name: "CSPReportLimit", got: httpx.CSPReportLimit, want: 16 << 10},
	} {
		if tt.got != tt.want {
			t.Errorf("%s = %d, want %d", tt.name, tt.got, tt.want)
		}
	}
}
