// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package web

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/divbast/pisati/internal/httpx"
)

const (
	// reportsType is the media type a browser posts Reporting API uploads
	// with.
	reportsType = "application/reports+json"
	// violationType is the type of a CSP violation's report.
	violationType = "csp-violation"
	// maxLoggedReports is how many reports of one upload are logged: a
	// little more than 16 KiB of a browser's real reports holds.
	maxLoggedReports = 25
	// otherValue is logged for a value outside its closed set.
	otherValue = "other"
)

// report is one report of an upload, decoded strictly for the fields
// cspReport reads: a field of the wrong JSON type fails the whole upload.
// Every other field is ignored, so a browser that sends more still
// passes.
type report struct {
	Type string     `json:"type"`
	Body reportBody `json:"body"`
}

// reportBody is the body of a CSP violation's report, as CSP Level 3's
// CSPViolationReportBody names its fields.
type reportBody struct {
	BlockedURL         string `json:"blockedURL"`
	DocumentURL        string `json:"documentURL"`
	EffectiveDirective string `json:"effectiveDirective"`
	Disposition        string `json:"disposition"`
	// StatusCode is nil when the report gives none.
	StatusCode *int `json:"statusCode"`
}

// cspReport logs the CSP violations a browser reports, as the Reporting
// API uploads them to the endpoint SecurityHeaders names: a JSON array of
// reports, posted as application/reports+json. It answers 204.
//
// Each report of the csp-violation type, up to 25 an upload, is logged
// as one record at warn level, csp violation, holding exactly these, and
// only from their closed sets, so that no text a client writes reaches
// the log:
//
//   - type: csp-violation;
//   - effective_directive: one of CSP Level 3's directives, or other;
//   - disposition: enforce, report, or other;
//   - status_code: the page's status, from 0 to 999, left out otherwise;
//   - blocked_url and page_url: the blocked URL and the page's, each as
//     scheme://host[:port], or the scheme alone for a URL with no host,
//     such as data:; a keyword CSP reports instead of a URL, such as
//     inline or eval; or other.
//
// A report of any other type is skipped. Beyond 25 reports, one record,
// csp reports dropped, counts the rest. Nothing else of the upload is
// logged: not its body, nor a path, query or fragment, nor the client's
// address.
//
// Any other media type is answered 415, and a body that is not an array
// of reports, 400, each logging nothing. A body over its limit is
// answered 413.
func (s *Site) cspReport(w http.ResponseWriter, r *http.Request) {
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != reportsType {
		http.Error(w, http.StatusText(http.StatusUnsupportedMediaType), http.StatusUnsupportedMediaType)
		return
	}
	reports, err := decodeReports(r.Body)
	if err != nil {
		if httpx.RefuseTooLarge(w, err) {
			return
		}
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	logged, dropped := 0, 0
	for _, rep := range reports {
		if rep.Type != violationType {
			continue
		}
		if logged == maxLoggedReports {
			dropped++
			continue
		}
		logged++
		b := rep.Body
		args := []any{
			"type", violationType, "effective_directive", directiveOf(b.EffectiveDirective),
			"disposition", dispositionOf(b.Disposition),
		}
		if b.StatusCode != nil && *b.StatusCode >= 0 && *b.StatusCode <= 999 {
			args = append(args, "status_code", *b.StatusCode)
		}
		args = append(args, "blocked_url", reduceURL(b.BlockedURL), "page_url", reduceURL(b.DocumentURL))
		s.log.WarnContext(ctx, "csp violation", args...)
	}
	if dropped > 0 {
		s.log.WarnContext(ctx, "csp reports dropped", "count", dropped)
	}
	w.WriteHeader(http.StatusNoContent)
}

// decodeReports decodes body as one JSON array of reports, and nothing
// after it.
func decodeReports(body io.Reader) ([]report, error) {
	dec := json.NewDecoder(body)
	var reports []report
	if err := dec.Decode(&reports); err != nil {
		return nil, err
	}
	if reports == nil {
		return nil, errors.New("web: the upload is null, not an array of reports")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("web: the upload holds more than one JSON value")
		}
		return nil, err
	}
	return reports, nil
}

// directiveOf returns d when it is one of CSP Level 3's directives, the
// closed set a report's effective directive is logged from, and other
// otherwise.
func directiveOf(d string) string {
	switch d {
	case "base-uri", "child-src", "connect-src", "default-src", "font-src", "form-action", "frame-ancestors",
		"frame-src", "img-src", "manifest-src", "media-src", "object-src", "report-to", "report-uri", "sandbox",
		"script-src", "script-src-attr", "script-src-elem", "style-src", "style-src-attr", "style-src-elem",
		"webrtc", "worker-src":
		return d
	}
	return otherValue
}

// dispositionOf returns d when it is enforce or report, and other
// otherwise.
func dispositionOf(d string) string {
	if d == "enforce" || d == "report" {
		return d
	}
	return otherValue
}

// isKeyword reports whether s is one of the values CSP Level 3 reports as
// a blocked URL that is no URL.
func isKeyword(s string) bool {
	switch s {
	case "inline", "eval", "wasm-eval", "trusted-types-policy", "trusted-types-sink":
		return true
	}
	return false
}

// hostRE is a host as reduceURL logs it: lower-case letters, digits,
// dots, hyphens, an IPv6 address's brackets and colons, and a port.
var hostRE = regexp.MustCompile(`^[a-z0-9.\-:\[\]]+$`)

// reduceURL returns s as reports log it: a keyword isKeyword knows as it
// is;
// an absolute URL as its scheme and host, port included, and without user
// information, path, query or fragment; one with no host as its scheme
// alone, as in data:; and anything else as other.
func reduceURL(s string) string {
	if isKeyword(s) {
		return s
	}
	u, err := url.Parse(s)
	if err != nil || !u.IsAbs() {
		return otherValue
	}
	if u.Host == "" {
		return u.Scheme + ":"
	}
	host := strings.ToLower(u.Host)
	if !hostRE.MatchString(host) {
		return otherValue
	}
	return u.Scheme + "://" + host
}
