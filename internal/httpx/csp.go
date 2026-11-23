// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package httpx

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"
)

const cspHeader = "Content-Security-Policy"

// CSP is the Content-Security-Policy of one response. It starts as the
// strict policy every response has,
//
//	default-src 'none'; script-src 'self' 'nonce-<n>'; style-src 'self';
//	img-src 'self'; font-src 'self'; connect-src 'self';
//	form-action 'self'; frame-ancestors 'none'; base-uri 'none';
//	object-src 'none'; report-to csp
//
// in one line, with the response's nonce as <n>, and with
// upgrade-insecure-requests at the end over https. A handler widens its
// own response's policy through WithCSP, and only in the ways CSP's
// methods allow, none of which touches script-src: a script runs only
// with the nonce whatever a handler does. Nothing it allows outlasts the
// one response. The only other widening is dev mode's, which
// SecurityHeaders makes for the Vite dev server.
//
// The zero CSP is the strict policy without a nonce, which runs no script
// but its origin's own files. SecurityHeaders makes each response's.
type CSP struct {
	nonce       string
	upgrade     bool
	dev         devServer
	styleAttrs  bool
	httpsFrames bool
	httpsMedia  bool
}

// devServer is where the Vite dev server is: its origin, and its
// hot-update WebSocket's. Both are empty outside dev mode.
type devServer struct {
	origin, socket string
}

// AllowStyleAttributes allows style attributes, as a rich post's HTML
// has them: style-src-attr 'unsafe-inline'. Style elements and style
// sheets stay as strict.
func (c *CSP) AllowStyleAttributes() {
	c.styleAttrs = true
}

// AllowHTTPSFrames allows frames from any https URL: frame-src https:.
func (c *CSP) AllowHTTPSFrames() {
	c.httpsFrames = true
}

// AllowHTTPSMedia allows audio and video from any https URL:
// media-src https:.
func (c *CSP) AllowHTTPSMedia() {
	c.httpsMedia = true
}

// value returns the policy as Content-Security-Policy's value.
func (c *CSP) value() string {
	var b strings.Builder
	b.WriteString("default-src 'none'; script-src 'self'")
	if c.nonce != "" {
		b.WriteString(" 'nonce-" + c.nonce + "'")
	}
	b.WriteString("; style-src 'self'")
	if c.dev.origin != "" {
		// Style sheets come from the dev server's origin. The style
		// elements Vite's client adds, for CSS that TypeScript imports,
		// are inline, which no origin allows: the client gives them the
		// page's nonce, read from its csp-nonce meta element.
		b.WriteString(" " + c.dev.origin)
		if c.nonce != "" {
			b.WriteString(" 'nonce-" + c.nonce + "'")
		}
	}
	b.WriteString("; img-src 'self'; font-src 'self'; connect-src 'self'")
	if c.dev.origin != "" {
		b.WriteString(" " + c.dev.origin + " " + c.dev.socket)
	}
	if c.httpsMedia {
		b.WriteString("; media-src https:")
	}
	if c.httpsFrames {
		b.WriteString("; frame-src https:")
	}
	b.WriteString("; form-action 'self'; frame-ancestors 'none'; base-uri 'none'; object-src 'none'")
	if c.styleAttrs {
		b.WriteString("; style-src-attr 'unsafe-inline'")
	}
	b.WriteString("; report-to csp")
	if c.upgrade {
		b.WriteString("; upgrade-insecure-requests")
	}
	return b.String()
}

// policy is one response's CSP, and whether the response's header has
// been written, after which the CSP no longer reaches it.
type policy struct {
	csp  CSP
	sent bool
}

// write sets the CSP in h, the header about to be written.
func (p *policy) write(h http.Header) {
	p.sent = true
	h.Set(cspHeader, p.csp.value())
}

// policyKey is the context key of a response's policy.
type policyKey struct{}

// WithCSP lets the handler answering the request ctx belongs to widen
// its own response's policy: widen is given the response's CSP, and the
// response is sent with what widen leaves in it. WithCSP reports whether
// widen ran. It does not for a context that did not pass through
// SecurityHeaders, or once the response's header has been written, when
// the policy can no longer change. Like the ResponseWriter, it is for the
// handler's own goroutine.
func WithCSP(ctx context.Context, widen func(*CSP)) bool {
	p, ok := ctx.Value(policyKey{}).(*policy)
	if !ok || p.sent {
		return false
	}
	widen(&p.csp)
	return true
}

// newNonce returns a new nonce: 128 bits from crypto/rand, in standard
// base64, which every CSP level accepts.
func newNonce() string {
	b := random128()
	return base64.StdEncoding.EncodeToString(b[:])
}
