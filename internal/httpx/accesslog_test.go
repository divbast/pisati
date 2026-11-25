// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package httpx_test

import (
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/divbast/pisati/internal/httpx"
)

func TestAccessLogOmitsQueryAndIP(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		handler http.HandlerFunc
		status  float64
		bytes   float64
	}{
		{
			name: "written",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusCreated)
				if _, err := io.WriteString(w, "hello"); err != nil {
					t.Errorf("writing the body: %v", err)
				}
			},
			status: http.StatusCreated,
			bytes:  5,
		},
		{
			name:    "nothing written",
			handler: func(http.ResponseWriter, *http.Request) {},
			status:  http.StatusOK,
			bytes:   0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			log, buf := newLog()
			clock := steppingClock(time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC), 1500*time.Millisecond)
			h := httpx.Chain(tt.handler, httpx.RequestID(), httpx.AccessLog(log, clock))

			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
				"/posts/hello?token=quokka-token&email=reader%40mail.example", http.NoBody)
			req.RemoteAddr = "203.0.113.7:4321"
			req.Header.Set("X-Forwarded-For", "198.51.100.23")
			req.Header.Set("User-Agent", "probe/1.0")
			req.Header.Set("Cookie", "pisati-session=quokka-cookie")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			records := buf.records(t)
			if len(records) != 1 {
				t.Fatalf("logged %d records, want 1: %s", len(records), buf)
			}
			got := records[0]
			keys := slices.Sorted(maps.Keys(got))
			wantKeys := []string{"bytes", "duration", "level", "method", "msg", "path", "request_id", "status", "time"}
			if !slices.Equal(keys, wantKeys) {
				t.Errorf("logged the keys %q, want exactly %q", keys, wantKeys)
			}
			want := map[string]any{
				"level":  "INFO",
				"msg":    "request answered",
				"method": http.MethodPost,
				"path":   "/posts/hello",
				"status": tt.status,
				"bytes":  tt.bytes,
				// Go's text form of a duration, not nanoseconds.
				"duration":   "1.5s",
				"request_id": rec.Result().Header.Get("X-Request-ID"),
			}
			for k, v := range want {
				if got[k] != v {
					t.Errorf("logged %s = %v, want %v", k, got[k], v)
				}
			}
			for _, leak := range []string{"token", "quokka", "mail.example", "203.0.113.7", "198.51.100.23", "probe/1.0"} {
				if strings.Contains(buf.String(), leak) {
					t.Errorf("the log holds %q: %s", leak, buf)
				}
			}
		})
	}
}
