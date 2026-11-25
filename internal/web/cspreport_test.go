// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package web_test

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/divbast/pisati/internal/httpx"
)

// chromeUpload is a CSP report upload as Chrome 158 sends one: the
// headers and body it sent, but for the site's own origin and host. It
// sends no Sec-Fetch-Site, no Referer and no cookie, and its Origin is
// the page's.
func chromeUpload(body string) request {
	return request{
		method: http.MethodPost, target: "/_/csp-report", body: body,
		header: map[string]string{
			"Content-Type":    "application/reports+json",
			"Origin":          origin,
			"Sec-Fetch-Site":  "",
			"Accept-Encoding": "gzip, deflate, br, zstd",
			"Accept-Language": "en-GB,en-US;q=0.9,en;q=0.8",
			"Priority":        "u=4, i",
			"User-Agent":      "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) HeadlessChrome/158.0.0.0 Safari/537.36",
		},
	}
}

// chromeReports is the body of the second upload Chrome 158 sent, of
// three reports, for the site's own pages.
const chromeReports = `[{"age":100,"body":{"blockedURL":"https://example.com/pixel.png?secret=1","columnNumber":47,` +
	`"disposition":"enforce","documentURL":"https://blog.example.org/","effectiveDirective":"img-src","lineNumber":4,` +
	`"originalPolicy":"default-src 'none'; img-src 'self'; report-to csp","referrer":"","sample":"",` +
	`"sourceFile":"https://blog.example.org/","statusCode":200},"type":"csp-violation","url":"https://blog.example.org/",` +
	`"user_agent":"Mozilla/5.0 HeadlessChrome/158.0.0.0"},` +
	`{"age":100,"body":{"blockedURL":"eval","columnNumber":27,"disposition":"enforce","documentURL":"https://blog.example.org/",` +
	`"effectiveDirective":"script-src","lineNumber":5,"originalPolicy":"default-src 'none'","referrer":"","sample":"",` +
	`"sourceFile":"https://blog.example.org/","statusCode":200},"type":"csp-violation","url":"https://blog.example.org/",` +
	`"user_agent":"Mozilla/5.0 HeadlessChrome/158.0.0.0"},` +
	`{"age":100,"body":{"blockedURL":"inline","columnNumber":9,"disposition":"enforce","documentURL":"https://blog.example.org/",` +
	`"effectiveDirective":"script-src-elem","lineNumber":4,"originalPolicy":"default-src 'none'","referrer":"","sample":"",` +
	`"sourceFile":"https://blog.example.org/","statusCode":200},"type":"csp-violation","url":"https://blog.example.org/",` +
	`"user_agent":"Mozilla/5.0 HeadlessChrome/158.0.0.0"}]`

// violation is one report, as a test writes it: the body's fields that
// are set.
func violation(body map[string]any) string {
	b, err := json.Marshal(map[string]any{"type": "csp-violation", "url": origin + "/", "body": body})
	if err != nil {
		panic(err)
	}
	return string(b)
}

// unlimited returns limiter options that let every upload through, for a
// test that sends more than a burst of them.
func unlimited() *httpx.LimiterOptions {
	return &httpx.LimiterOptions{Rate: 1e6, Burst: 1e6, MaxEntries: 2, Idle: time.Minute}
}

// recordKeys are the keys of a csp violation record: slog's own, then the
// fields it logs.
var recordKeys = []string{
	"time", "level", "msg", "type", "effective_directive", "disposition", "status_code", "blocked_url", "page_url",
}

// violations returns the csp violation records p has logged, failing the
// test on any other record but the count of those dropped.
func violations(t *testing.T, p *pisati) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, rec := range p.records() {
		switch rec["msg"] {
		case "csp violation":
			out = append(out, rec)
		case "csp reports dropped":
		default:
			t.Errorf("an unexpected record: %v", rec)
		}
	}
	return out
}

func TestCSPReportFromABrowserPasses(t *testing.T) {
	t.Parallel()

	for _, hostHeader := range []string{host, "127.0.0.1:8080"} {
		p := newPisati(t, options{})
		req := chromeUpload(chromeReports)
		// Behind a proxy that passes another Host, the upload still
		// passes: CrossOrigin trusts the base URL's origin.
		req.host = hostHeader
		w := p.do(req)
		if w.Code != http.StatusNoContent {
			t.Fatalf("Host %s: Chrome's upload = %d, want 204; body %q", hostHeader, w.Code, w.Body.String())
		}
		wantCache(t, w, noStore, "a CSP report")
		got := violations(t, p)
		want := []map[string]string{
			{
				"type": "csp-violation", "effective_directive": "img-src", "disposition": "enforce", "status_code": "200",
				"blocked_url": "https://example.com", "page_url": "https://blog.example.org",
			},
			{
				"type": "csp-violation", "effective_directive": "script-src", "disposition": "enforce", "status_code": "200",
				"blocked_url": "eval", "page_url": "https://blog.example.org",
			},
			{
				"type": "csp-violation", "effective_directive": "script-src-elem", "disposition": "enforce", "status_code": "200",
				"blocked_url": "inline", "page_url": "https://blog.example.org",
			},
		}
		if len(got) != len(want) {
			t.Fatalf("Host %s: %d records, want %d: %v", hostHeader, len(got), len(want), got)
		}
		for i, rec := range got {
			if rec["level"] != "WARN" {
				t.Errorf("record %d is at %v, want WARN", i, rec["level"])
			}
			if keys := slices.Sorted(maps.Keys(rec)); !slices.Equal(keys, slices.Sorted(slices.Values(recordKeys))) {
				t.Errorf("record %d has the keys %v, want exactly %v", i, keys, recordKeys)
			}
			for k, v := range want[i] {
				if fmt.Sprint(rec[k]) != v {
					t.Errorf("record %d: %s = %v, want %s", i, k, rec[k], v)
				}
			}
		}
	}
}

func TestCSPReportEndpointBounded(t *testing.T) {
	t.Parallel()

	t.Run("a body over 16 KiB is refused with 413", func(t *testing.T) {
		t.Parallel()

		p := newPisati(t, options{})
		big := "[" + violation(map[string]any{"sample": strings.Repeat("a", int(httpx.CSPReportLimit))}) + "]"
		for _, length := range []int64{int64(len(big)), -1} {
			req := chromeUpload(big)
			req.length = length
			w := p.do(req)
			if w.Code != http.StatusRequestEntityTooLarge {
				t.Errorf("a %d-byte upload (Content-Length %d) = %d, want 413", len(big), length, w.Code)
			}
			wantCache(t, w, noStore, "413")
		}
		if recs := p.records(); len(recs) != 0 {
			t.Errorf("an oversized upload logged %v", recs)
		}
	})

	t.Run("a flood is refused with 429", func(t *testing.T) {
		t.Parallel()

		synctest.Test(t, func(t *testing.T) {
			p := newPisati(t, options{})
			for i := range 5 {
				if w := p.do(chromeUpload(chromeReports)); w.Code != http.StatusNoContent {
					t.Fatalf("upload %d of the burst = %d, want 204", i+1, w.Code)
				}
			}
			w := p.do(chromeUpload(chromeReports))
			if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "1" {
				t.Fatalf("the upload past the burst = %d, Retry-After %q; want 429 and 1", w.Code, w.Header().Get("Retry-After"))
			}
			wantCache(t, w, noStore, "429")
			if n := len(violations(t, p)); n != 15 {
				t.Errorf("%d records, want the 15 of the five uploads let through", n)
			}
			time.Sleep(time.Second)
			if w := p.do(chromeUpload(chromeReports)); w.Code != http.StatusNoContent {
				t.Errorf("an upload a second later = %d, want 204", w.Code)
			}
		})
	})

	t.Run("the raw body never reaches the log", func(t *testing.T) {
		t.Parallel()

		p := newPisati(t, options{})
		body := `[{"type":"csp-violation","url":"https://blog.example.org/top-url-marker?q=top#top","age":7,` +
			`"user_agent":"agent-marker","body":{"blockedURL":"https://user:pw@cdn.example:8443/blocked-path-marker?q=blocked#frag",` +
			`"documentURL":"https://blog.example.org/page-path-marker?query-marker=1#fragment-marker",` +
			`"effectiveDirective":"img-src","disposition":"report","statusCode":404,"sourceFile":"https://source-marker.example/x.js",` +
			`"sample":"sample-marker","originalPolicy":"policy-marker","referrer":"https://referrer-marker.example/",` +
			`"lineNumber":1,"columnNumber":2}}]`
		req := chromeUpload(body)
		req.remote = "203.0.113.77:4444"
		if w := p.do(req); w.Code != http.StatusNoContent {
			t.Fatalf("the upload = %d, want 204", w.Code)
		}
		logged := p.logs.String()
		for _, marker := range []string{
			"marker", "203.0.113.77", "user", "pw", "8443/", "?q", "#", "blocked-path", "frag", "lineNumber",
		} {
			if strings.Contains(logged, marker) {
				t.Errorf("the log holds %q:\n%s", marker, logged)
			}
		}
		got := violations(t, p)
		if len(got) != 1 || got[0]["blocked_url"] != "https://cdn.example:8443" || got[0]["page_url"] != "https://blog.example.org" ||
			got[0]["disposition"] != "report" || fmt.Sprint(got[0]["status_code"]) != "404" {
			t.Errorf("records %v, want one with blocked_url https://cdn.example:8443 and page_url https://blog.example.org", got)
		}
	})

	t.Run("at most 25 reports an upload are logged", func(t *testing.T) {
		t.Parallel()

		p := newPisati(t, options{})
		reports := make([]string, 30)
		for i := range reports {
			reports[i] = violation(map[string]any{"effectiveDirective": "img-src", "blockedURL": "inline", "statusCode": i})
		}
		if w := p.do(chromeUpload("[" + strings.Join(reports, ",") + "]")); w.Code != http.StatusNoContent {
			t.Fatalf("an upload of 30 reports = %d, want 204", w.Code)
		}
		recs := p.records()
		if n := len(violations(t, p)); n != 25 {
			t.Errorf("%d reports logged, want 25", n)
		}
		last := recs[len(recs)-1]
		if last["msg"] != "csp reports dropped" || fmt.Sprint(last["count"]) != "5" || last["level"] != "WARN" {
			t.Errorf("the last record is %v, want csp reports dropped, with count 5, at WARN", last)
		}
	})

	t.Run("an upload of 25 reports drops none", func(t *testing.T) {
		t.Parallel()

		p := newPisati(t, options{})
		reports := slices.Repeat([]string{violation(map[string]any{"blockedURL": "inline"})}, 25)
		p.do(chromeUpload("[" + strings.Join(reports, ",") + "]"))
		for _, rec := range p.records() {
			if rec["msg"] != "csp violation" {
				t.Errorf("an upload of 25 reports logged %v", rec)
			}
		}
	})

	t.Run("anything but a reports upload is refused with 415", func(t *testing.T) {
		t.Parallel()

		p := newPisati(t, options{reports: unlimited()})
		for _, ct := range []string{"application/json", "application/csp-report", "text/plain", "application/reports+json;;", "-"} {
			req := chromeUpload(chromeReports)
			req.header["Content-Type"] = ct
			if ct == "-" {
				req.header["Content-Type"] = ""
			}
			w := p.do(req)
			if w.Code != http.StatusUnsupportedMediaType {
				t.Errorf("Content-Type %q = %d, want 415", ct, w.Code)
			}
			wantCache(t, w, noStore, "415")
		}
		req := chromeUpload(chromeReports)
		req.header["Content-Type"] = "Application/Reports+JSON; charset=utf-8"
		if w := p.do(req); w.Code != http.StatusNoContent {
			t.Errorf("Content-Type with a charset, in another case = %d, want 204", w.Code)
		}
		if n := len(violations(t, p)); n != 3 {
			t.Errorf("%d records, want only the 3 of the accepted upload", n)
		}
	})

	t.Run("a body that is no upload is refused with 400", func(t *testing.T) {
		t.Parallel()

		p := newPisati(t, options{reports: unlimited()})
		for _, body := range []string{
			``, `{}`, `null`, `[`, `"x"`, `[] []`, `[1]`, `[{"type":5}]`,
			`[{"type":"csp-violation","body":{"statusCode":"200"}}]`,
			`[{"type":"csp-violation","body":{"statusCode":200.5}}]`,
			`[{"type":"csp-violation","body":{"blockedURL":7}}]`,
			`[{"type":"csp-violation","body":[]}]`,
			`[{"type":"csp-violation","body":{"effectiveDirective":"img-src"}},{"type":"csp-violation","body":{"disposition":false}}]`,
		} {
			w := p.do(chromeUpload(body))
			if w.Code != http.StatusBadRequest {
				t.Errorf("the body %q = %d, want 400", body, w.Code)
			}
			wantCache(t, w, noStore, "400")
		}
		if recs := p.records(); len(recs) != 0 {
			t.Errorf("a refused upload logged %v", recs)
		}
		if w := p.do(chromeUpload(`[]`)); w.Code != http.StatusNoContent {
			t.Errorf("an empty upload = %d, want 204", w.Code)
		}
	})

	t.Run("only values from closed sets are logged", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			body map[string]any
			want map[string]any
		}{
			{body: map[string]any{"effectiveDirective": "made-up-src"}, want: map[string]any{"effective_directive": "other"}},
			{body: map[string]any{"effectiveDirective": "style-src-attr"}, want: map[string]any{"effective_directive": "style-src-attr"}},
			{body: map[string]any{"disposition": "maybe"}, want: map[string]any{"disposition": "other"}},
			{body: map[string]any{"disposition": "enforce"}, want: map[string]any{"disposition": "enforce"}},
			{body: map[string]any{"statusCode": 1000}, want: map[string]any{"status_code": nil}},
			{body: map[string]any{"statusCode": -1}, want: map[string]any{"status_code": nil}},
			{body: map[string]any{"statusCode": 0}, want: map[string]any{"status_code": "0"}},
			{body: map[string]any{"blockedURL": "wasm-eval"}, want: map[string]any{"blocked_url": "wasm-eval"}},
			{body: map[string]any{"blockedURL": "trusted-types-sink"}, want: map[string]any{"blocked_url": "trusted-types-sink"}},
			{body: map[string]any{"blockedURL": "data:image/png;base64,AAAA"}, want: map[string]any{"blocked_url": "data:"}},
			{body: map[string]any{"blockedURL": "blob:https://blog.example.org/0b1c"}, want: map[string]any{"blocked_url": "blob:"}},
			{body: map[string]any{"blockedURL": "about:blank"}, want: map[string]any{"blocked_url": "about:"}},
			{body: map[string]any{"blockedURL": "HTTPS://CDN.Example/a"}, want: map[string]any{"blocked_url": "https://cdn.example"}},
			{body: map[string]any{"blockedURL": "https://[2001:db8::1]:8443/a"}, want: map[string]any{"blocked_url": "https://[2001:db8::1]:8443"}},
			{body: map[string]any{"blockedURL": "not a url"}, want: map[string]any{"blocked_url": "other"}},
			{body: map[string]any{"blockedURL": "/relative/path"}, want: map[string]any{"blocked_url": "other"}},
			{body: map[string]any{"blockedURL": "https://exa%20mple.org/"}, want: map[string]any{"blocked_url": "other"}},
			{body: map[string]any{"blockedURL": "https://naïve.example/"}, want: map[string]any{"blocked_url": "other"}},
			{body: map[string]any{"blockedURL": ""}, want: map[string]any{"blocked_url": "other"}},
			{body: map[string]any{"documentURL": "https://blog.example.org/a?b#c"}, want: map[string]any{"page_url": "https://blog.example.org"}},
			{body: map[string]any{}, want: map[string]any{"effective_directive": "other", "disposition": "other", "blocked_url": "other", "page_url": "other"}},
		}
		for _, tt := range tests {
			p := newPisati(t, options{})
			if w := p.do(chromeUpload("[" + violation(tt.body) + "]")); w.Code != http.StatusNoContent {
				t.Fatalf("%v = %d, want 204", tt.body, w.Code)
			}
			got := violations(t, p)
			if len(got) != 1 {
				t.Fatalf("%v: %d records, want 1", tt.body, len(got))
			}
			for k, v := range tt.want {
				switch {
				case v == nil:
					if _, ok := got[0][k]; ok {
						t.Errorf("%v: %s = %v, want it left out", tt.body, k, got[0][k])
					}
				case fmt.Sprint(got[0][k]) != fmt.Sprint(v):
					t.Errorf("%v: %s = %v, want %v", tt.body, k, got[0][k], v)
				}
			}
		}
	})

	t.Run("keys match in their own case alone", func(t *testing.T) {
		t.Parallel()

		// A key in another case is not the field it spells: it is an
		// unknown key, ignored, whatever its value.
		tests := []struct {
			body string
			want map[string]any
		}{
			{body: `[{"Type":"csp-violation","body":{"blockedURL":"inline"}}]`},
			{
				body: `[{"type":"csp-violation","BODY":{"blockedURL":"inline"}}]`,
				want: map[string]any{"blocked_url": "other"},
			},
			{
				body: `[{"type":"csp-violation","body":{"statuscode":200,"BlockedURL":"eval","blockedURL":"inline"}}]`,
				want: map[string]any{"status_code": nil, "blocked_url": "inline"},
			},
			{
				body: `[{"type":"csp-violation","body":{"StatusCode":"not a number","Disposition":7,"disposition":"report"}}]`,
				want: map[string]any{"status_code": nil, "disposition": "report"},
			},
		}
		for _, tt := range tests {
			p := newPisati(t, options{})
			if w := p.do(chromeUpload(tt.body)); w.Code != http.StatusNoContent {
				t.Errorf("%s = %d, want 204", tt.body, w.Code)
				continue
			}
			got := violations(t, p)
			if tt.want == nil {
				if len(got) != 0 {
					t.Errorf("%s: records %v, want none: its type is no csp-violation", tt.body, got)
				}
				continue
			}
			if len(got) != 1 {
				t.Errorf("%s: %d records, want 1", tt.body, len(got))
				continue
			}
			for k, v := range tt.want {
				switch {
				case v == nil:
					if _, ok := got[0][k]; ok {
						t.Errorf("%s: %s = %v, want it left out", tt.body, k, got[0][k])
					}
				case fmt.Sprint(got[0][k]) != fmt.Sprint(v):
					t.Errorf("%s: %s = %v, want %v", tt.body, k, got[0][k], v)
				}
			}
		}
	})

	t.Run("a report of another type is skipped", func(t *testing.T) {
		t.Parallel()

		p := newPisati(t, options{})
		body := `[{"type":"deprecation","body":{"id":"x"}},{"type":"CSP-violation"},` + violation(map[string]any{"blockedURL": "eval"}) + `]`
		if w := p.do(chromeUpload(body)); w.Code != http.StatusNoContent {
			t.Fatalf("the upload = %d, want 204", w.Code)
		}
		if got := violations(t, p); len(got) != 1 || got[0]["blocked_url"] != "eval" {
			t.Errorf("records %v, want only the csp-violation", got)
		}
	})
}

// The values a csp violation record may hold, by key.
var (
	directiveRE = regexp.MustCompile(`^(other|(base-uri|child-src|connect-src|default-src|font-src|form-action|` +
		`frame-ancestors|frame-src|img-src|manifest-src|media-src|object-src|report-to|report-uri|sandbox|` +
		`script-src|script-src-attr|script-src-elem|style-src|style-src-attr|style-src-elem|webrtc|worker-src))$`)
	urlRE = regexp.MustCompile(`^(other|inline|eval|wasm-eval|trusted-types-policy|trusted-types-sink|` +
		`[a-z][a-z0-9+.-]*:|[a-z][a-z0-9+.-]*://[a-z0-9.:\[\]-]+)$`)
)

func FuzzCSPReport(f *testing.F) {
	f.Add([]byte(chromeReports))
	f.Add([]byte(`[]`))
	f.Add([]byte(`[{"type":"csp-violation","body":{"blockedURL":"https://a:b@c.example:1/p?q#f","statusCode":999}}]`))
	f.Add([]byte(`[{"type":"csp-violation","body":{"documentURL":"javascript:alert(1)","effectiveDirective":"img-src\n"}}]`))
	f.Add([]byte(`[{"type":"csp-violation"},null,{"type":"other"}]`))
	f.Add([]byte(`{"type":"csp-violation"}`))
	reports := slices.Repeat([]string{violation(map[string]any{"blockedURL": "inline"})}, 30)
	f.Add([]byte("[" + strings.Join(reports, ",") + "]"))
	f.Fuzz(func(t *testing.T, body []byte) {
		p := newPisati(t, options{reports: unlimited()})
		w := p.do(chromeUpload(string(body)))
		switch w.Code {
		case http.StatusNoContent, http.StatusBadRequest, http.StatusRequestEntityTooLarge:
		default:
			t.Fatalf("status %d, want 204, 400 or 413", w.Code)
		}
		recs := p.records()
		if w.Code != http.StatusNoContent && len(recs) != 0 {
			t.Fatalf("a refused upload (%d) logged %v", w.Code, recs)
		}
		logged, dropped := 0, 0
		for _, rec := range recs {
			switch rec["msg"] {
			case "csp violation":
				logged++
				for k, v := range rec {
					s := fmt.Sprint(v)
					switch k {
					case "time", "level", "msg":
					case "type":
						if s != "csp-violation" {
							t.Errorf("type %q", s)
						}
					case "effective_directive":
						if !directiveRE.MatchString(s) {
							t.Errorf("effective_directive %q, outside the closed set", s)
						}
					case "disposition":
						if s != "enforce" && s != "report" && s != "other" {
							t.Errorf("disposition %q, outside the closed set", s)
						}
					case "status_code":
						if n, err := strconv.Atoi(s); err != nil || n < 0 || n > 999 {
							t.Errorf("status_code %q, not 0 to 999", s)
						}
					case "blocked_url", "page_url":
						if !urlRE.MatchString(s) {
							t.Errorf("%s %q holds more than a scheme and host", k, s)
						}
					default:
						t.Errorf("a record with the key %q", k)
					}
				}
			case "csp reports dropped":
				dropped++
				if n, ok := rec["count"].(float64); !ok || n < 1 {
					t.Errorf("csp reports dropped with count %v", rec["count"])
				}
			default:
				t.Errorf("a record %v", rec)
			}
		}
		if logged > 25 || dropped > 1 || (dropped == 1 && logged != 25) {
			t.Errorf("%d reports logged and %d drop records, want at most 25, and a drop record only after 25", logged, dropped)
		}
	})
}
