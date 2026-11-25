// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package httpx

import (
	"context"
	"net/http"
	"strconv"

	"github.com/divbast/pisati/internal/config"
)

// hstsMaxAge has a browser keep to https for two years.
const hstsMaxAge = "max-age=63072000"

// header is one response header and its value.
type header struct {
	name, value string
}

// SecurityHeaders sets, before the handler runs, the headers that harden
// every response in a browser:
//
//   - Content-Security-Policy: the strict policy CSP describes, with a
//     nonce of 128 bits from crypto/rand that is new for each request.
//     Nonce returns it, for the response's script elements, and WithCSP
//     lets the handler widen its own response's policy;
//   - Reporting-Endpoints, naming base's /_/csp-report as csp, the
//     endpoint the policy reports to;
//   - X-Content-Type-Options: nosniff;
//   - Referrer-Policy: strict-origin-when-cross-origin;
//   - Cross-Origin-Opener-Policy and Cross-Origin-Resource-Policy:
//     same-origin;
//   - Permissions-Policy, which allows no page the camera, microphone,
//     geolocation, payment or USB;
//   - when base is https, and only then: Strict-Transport-Security for
//     two years, reaching as far as hsts says, and upgrade-insecure-requests
//     in the policy.
//
// The response is sent with the policy its CSP holds when the header is
// written, whatever the handler sets as Content-Security-Policy itself.
// Cross-Origin-Embedder-Policy is each route's to set: see Isolation.
//
// In dev mode, when dev.ViteURL is set, every policy also lets the page
// use the Vite dev server: its origin in style-src and connect-src, its
// hot-update WebSocket's (ws:, or wss: for an https server) in
// connect-src, the response's nonce in style-src too, for the style
// elements Vite's client adds to the page, and worker-src 'self' blob:,
// for the worker Vite's client polls from while the dev server restarts.
// script-src is left as it is. Besides CSP's methods, this is the only
// way a policy is widened; outside dev mode no policy has worker-src.
//
// The zero BaseURL, an hsts that is none of the HSTSScope constants, or a
// dev.ViteURL with it or base away from localhost, 127.0.0.1 and [::1],
// is a bug, since config.Load returns none of them, and SecurityHeaders
// panics on it.
func SecurityHeaders(base config.BaseURL, hsts config.HSTSScope, dev config.Dev) Middleware {
	if base.String() == "" {
		panic("httpx.SecurityHeaders was given the zero BaseURL, " + bug + ": config.Load never gives one")
	}
	sts := hstsValue(hsts)
	vite := devServerOf(base, dev)
	https := base.IsHTTPS()
	fixed := []header{
		{name: "Reporting-Endpoints", value: `csp="` + base.String() + `/_/csp-report"`},
		{name: "X-Content-Type-Options", value: "nosniff"},
		{name: "Referrer-Policy", value: "strict-origin-when-cross-origin"},
		{name: "Cross-Origin-Opener-Policy", value: "same-origin"},
		{name: "Cross-Origin-Resource-Policy", value: "same-origin"},
		{name: "Permissions-Policy", value: "camera=(), microphone=(), geolocation=(), payment=(), usb=()"},
	}
	if https {
		fixed = append(fixed, header{name: "Strict-Transport-Security", value: sts})
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			for _, f := range fixed {
				h.Set(f.name, f.value)
			}
			p := &policy{csp: CSP{nonce: newNonce(), upgrade: https, dev: vite}}
			// Set now as well as when the header is written, so that a
			// response written past this writer, as Recover writes its
			// 500, still carries the strict policy.
			h.Set(cspHeader, p.csp.value())
			rec := &recorder{ResponseWriter: w, beforeHeader: p.write}
			next.ServeHTTP(rec, r.WithContext(context.WithValue(r.Context(), policyKey{}, p)))
			if !p.sent {
				// The handler wrote no header, so net/http writes it now
				// that the handler has returned: with the CSP as the
				// handler left it.
				p.write(h)
			}
		})
	}
}

// hstsValue returns Strict-Transport-Security's value for s.
func hstsValue(s config.HSTSScope) string {
	switch s {
	case config.HSTSHostOnly:
		return hstsMaxAge
	case config.HSTSIncludeSubdomains:
		return hstsMaxAge + "; includeSubDomains"
	case config.HSTSPreload:
		return hstsMaxAge + "; includeSubDomains; preload"
	}
	panic("httpx.SecurityHeaders was given the HSTS scope " + s.String() + ", which it does not know, " + bug)
}

// devServerOf returns where dev's Vite dev server is, or the zero
// devServer when dev sets none. It panics when the server, or base, is
// away from loopback: config.Load sets ViteURL only when both are on it.
func devServerOf(base config.BaseURL, dev config.Dev) devServer {
	vite, ok := dev.ViteOrigin()
	if !ok {
		return devServer{}
	}
	if !base.IsLocal() || !vite.IsLocal() {
		panic("httpx.SecurityHeaders was given a Vite dev server and a base URL that are not both on localhost, " +
			"127.0.0.1 or [::1], " + bug + ": config.Load never gives them")
	}
	socket := "ws://"
	if vite.IsHTTPS() {
		socket = "wss://"
	}
	return devServer{origin: vite.String(), socket: socket + vite.Host()}
}

// Nonce returns the nonce of the response to the request ctx belongs to,
// which each script element the response sends must carry, and whether
// there is one: a context that did not pass through SecurityHeaders has
// none.
func Nonce(ctx context.Context) (string, bool) {
	p, ok := ctx.Value(policyKey{}).(*policy)
	if !ok {
		return "", false
	}
	return p.csp.nonce, true
}

// Isolation is whether a route's responses are cross-origin isolated:
// whether a page may embed only what consents to it. Each route declares
// its own. A page that shows post content embeds media from origins that
// cannot consent, so only pages without it are Isolated. The zero
// Isolation is NotIsolated.
type Isolation uint8

const (
	// NotIsolated leaves a route's responses as they are. It is the zero
	// Isolation.
	NotIsolated Isolation = iota
	// Isolated sets Cross-Origin-Embedder-Policy: require-corp on a
	// route's responses.
	Isolated
)

// Middleware returns the middleware that gives a route's responses i:
// for Isolated, it sets Cross-Origin-Embedder-Policy: require-corp before
// the handler runs, and for NotIsolated it leaves the handler as it is.
// An i that is neither is a bug, and Middleware panics on it.
func (i Isolation) Middleware() Middleware {
	switch i {
	case NotIsolated:
		return func(next http.Handler) http.Handler { return next }
	case Isolated:
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Cross-Origin-Embedder-Policy", "require-corp")
				next.ServeHTTP(w, r)
			})
		}
	}
	panic("httpx.Isolation.Middleware was called on Isolation(" + strconv.Itoa(int(i)) + "), which it does not know, " + bug)
}
