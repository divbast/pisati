// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

// Package config loads Pisati's configuration once, at start: built-in
// defaults, then pisati.toml, then the drop-ins in pisati.toml.d, then
// PISATI_* variables. One table of settings drives every step.
//
// Load checks every value it reads and reports, in one error, every
// unknown key in each file, every unknown PISATI_ variable and every value
// refused. A file with a syntax or type error is reported at that error
// alone, since decoding stops there, and gives none of its settings.
//
// The package reads nothing by itself: files reach it through fs.FS and
// the environment through a lookup function, both built in main.
package config

import (
	"cmp"
	"errors"
	"log/slog"
	"math"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

// The conditions Load's errors report. Each error Load returns names the
// setting, file or variable at fault, and wraps one of these, or, for a
// source it could not read, that source's own error. Their own text never
// shows in the errors Load returns, which say in whole sentences what is
// wrong and what to do.
var (
	// ErrInvalid is a value its rule refuses, or a file that is not TOML.
	ErrInvalid = errors.New("invalid")
	// ErrUnknown is a TOML key or a PISATI_ variable that names no setting.
	ErrUnknown = errors.New("unknown setting")
	// ErrMissing is a setting that has no default and was not given.
	ErrMissing = errors.New("missing")
)

// badValue is a value that its setting's rule refuses: why, as what
// follows "which" or "that", and fix, a sentence on what to give instead.
// The value is shown quoted, unless hidden says why it must not be: a URL
// may hold a password, and a path may be a secret given in its place.
// item says the value is one item of a list.
type badValue struct {
	value, hidden string
	item          bool
	why, fix      string
}

func (b *badValue) Error() string {
	if b.hidden != "" {
		return "the value " + b.why + ". " + b.fix + ". The value is not shown, because " + b.hidden
	}
	return "the value " + strconv.Quote(b.value) + " " + b.why + ". " + b.fix
}

func (b *badValue) Unwrap() error { return ErrInvalid }

// of says b of what label names: a setting, with its variable, or a
// variable alone.
func (b *badValue) of(label string) string {
	switch {
	case b.hidden != "":
		return label + " is set to a value that " + b.why + ". " + b.fix +
			". Pisati does not show the value, because " + b.hidden
	case b.item:
		return label + " holds " + strconv.Quote(b.value) + ", which " + b.why + ". " + b.fix
	}
	return label + " is set to " + strconv.Quote(b.value) + ", which " + b.why + ". " + b.fix
}

// whyURLHidden is why a URL is never shown.
const whyURLHidden = "a URL may hold a password"

// baseURLFix says what server.base_url takes.
const baseURLFix = "Set it to an https URL with no path, such as https://blog.example.org, or, for use on this " +
	"machine alone, an http URL on localhost, 127.0.0.1 or [::1]"

// listenFix says what a listen address takes.
const listenFix = "Set it to host:port, with a port from 1 to 65535, such as 127.0.0.1:8080, or :8080 for every " +
	"address of this machine"

// Config is Pisati's whole configuration. In one that Load returns
// without an error, every field holds a value its type's rules accept.
type Config struct {
	Server    Server
	Storage   Storage
	Log       Log
	Telemetry Telemetry
	Keys      Keys
	Dev       Dev
}

// Server is how Pisati is reached.
type Server struct {
	// BaseURL is where the site is served. It has no default.
	BaseURL BaseURL
	// Listen serves the site, :8080 by default. InternalListen serves
	// /healthz, /metrics and pprof, :8081 by default.
	Listen, InternalListen ListenAddr
	// TrustedProxies are the addresses whose forwarded headers are
	// believed: none by default. A bare address is a /32 or /128.
	TrustedProxies []netip.Prefix
	// HSTS is how far Strict-Transport-Security reaches over https.
	HSTS HSTSScope
}

// Storage is where Pisati keeps its state.
type Storage struct {
	// StateDir is an absolute, clean path: $STATE_DIRECTORY when systemd
	// sets it, otherwise /var/lib/pisati.
	StateDir string
}

// Log is how Pisati logs.
type Log struct {
	// Level is one of slog.LevelDebug, LevelInfo (the default), LevelWarn
	// and LevelError.
	Level  slog.Level
	Format LogFormat
}

// Telemetry is what Pisati reports about itself.
type Telemetry struct {
	// OTLPEndpoint is where traces go over OTLP/HTTP. Unset by default,
	// which exports none.
	OTLPEndpoint OptionalURL
	// Pprof is whether the internal listener serves pprof.
	Pprof Switch
}

// Keys says where Pisati's keys come from.
type Keys struct {
	// MainKey is never nil in a Config that Load returns without an
	// error.
	MainKey SecretSource
}

// Dev holds settings for development alone.
type Dev struct {
	// ViteURL is the Vite dev server. Load sets it only when it and the
	// base URL are both on localhost, 127.0.0.1 or [::1].
	ViteURL OptionalURL
	// ViteCertFile and ViteKeyFile name the certificate and the private
	// key the Vite dev server serves https with: absolute, clean paths,
	// which Load requires when ViteURL is https. They apply only then,
	// and are accepted and left unused otherwise. Pisati never opens
	// them: vite.config.ts reads them, from PISATI_DEV_VITE_CERT_FILE and
	// PISATI_DEV_VITE_KEY_FILE.
	ViteCertFile, ViteKeyFile string
}

// ViteOrigin returns the origin of ViteURL, the Vite dev server's, and
// whether there is one: only when ViteURL is set.
func (d Dev) ViteOrigin() (Origin, bool) {
	u, ok := d.ViteURL.Get()
	if !ok {
		return Origin{}, false
	}
	return Origin{scheme: u.Scheme, host: u.Host, local: loopback(u.Hostname())}, true
}

// Origin is where a URL is served: its scheme, http or https, and its
// host, with the port when it is not the scheme's default, in the
// canonical form a BaseURL has. The zero Origin is none: its String is
// empty.
type Origin struct {
	scheme, host string
	local        bool
}

// String returns the origin as scheme://host[:port].
func (o Origin) String() string {
	if o.scheme == "" {
		return ""
	}
	return o.scheme + "://" + o.host
}

// Host returns the host, with the port when it is not the scheme's
// default: an IPv6 address is in brackets.
func (o Origin) Host() string {
	return o.host
}

// IsHTTPS reports whether the scheme is https.
func (o Origin) IsHTTPS() bool {
	return o.scheme == "https"
}

// IsLocal reports whether the host is localhost, 127.0.0.1 or [::1].
func (o Origin) IsLocal() bool {
	return o.local
}

// BaseURL is the URL the site is served at: https://host[:port], or
// http://host[:port] when the host is localhost, 127.0.0.1 or [::1]. It
// has no path, query, fragment or user information; its host is lower
// case, an IP address is in its shortest form, and the port is left out
// when it is the scheme's default. The zero BaseURL is not valid: its
// String is empty.
type BaseURL struct {
	origin string
	https  bool
	local  bool
}

// ParseBaseURL returns s as a BaseURL, or an error wrapping ErrInvalid
// that says why it is not one, without repeating s, which may hold a
// password. A path of exactly "/" is accepted and left out.
func ParseBaseURL(s string) (BaseURL, error) {
	u, why := parseWebURL(s)
	if why == "" {
		if p := u.EscapedPath(); p != "" && p != "/" {
			why = "has a path after its host"
		} else if u.Scheme == "http" && !loopback(u.Hostname()) {
			why = "is an http URL on a host other than localhost, 127.0.0.1 or [::1], where Pisati takes only https"
		}
	}
	if why != "" {
		return BaseURL{}, &badValue{hidden: whyURLHidden, why: why, fix: baseURLFix}
	}
	return BaseURL{
		origin: u.Scheme + "://" + u.Host,
		https:  u.Scheme == "https",
		local:  loopback(u.Hostname()),
	}, nil
}

// IsHTTPS reports whether the site is served over https.
func (b BaseURL) IsHTTPS() bool {
	return b.https
}

// IsLocal reports whether the site is served on localhost, 127.0.0.1 or
// [::1]. The zero BaseURL is not.
func (b BaseURL) IsLocal() bool {
	return b.local
}

// String returns the URL in its canonical form, which ParseBaseURL gives
// back unchanged.
func (b BaseURL) String() string {
	return b.origin
}

// ListenAddr is an address to listen on: a host, which may be empty for
// every interface, and a port from 1 to 65535. The zero ListenAddr is not
// valid: its Port is 0.
type ListenAddr struct {
	host string
	port uint16
}

// ParseListenAddr returns s, which is host:port, as a ListenAddr, or an
// error wrapping ErrInvalid that says why it is not one. The host is
// empty, an IP address (an IPv6 one in brackets) or a host name.
func ParseListenAddr(s string) (ListenAddr, error) {
	a, why, fix := parseListenAddr(s)
	if why != "" {
		return ListenAddr{}, &badValue{value: s, why: why, fix: fix}
	}
	return a, nil
}

// parseListenAddr returns s as a ListenAddr, or why it is not one and what
// to give instead.
func parseListenAddr(s string) (_ ListenAddr, why, fix string) {
	host, portText, err := net.SplitHostPort(s)
	if err != nil {
		return ListenAddr{}, "is not written as host:port", listenFix
	}
	port, why := parsePort(portText)
	if why != "" {
		return ListenAddr{}, why, listenFix
	}
	bracketed := strings.HasPrefix(s, "[")
	if addr, err := netip.ParseAddr(host); err == nil {
		if why, fix := brackets(addr, bracketed); why != "" {
			return ListenAddr{}, why, fix
		}
		return ListenAddr{host: addr.String(), port: port}, "", ""
	}
	if host != "" && (bracketed || !hostName(host)) {
		return ListenAddr{}, "has a host that is neither a host name nor an IP address", listenFix
	}
	return ListenAddr{host: strings.ToLower(host), port: port}, "", ""
}

// brackets returns why addr, written in brackets when bracketed, is not
// written as it must be, and what to do, or "" for both when it is: an
// IPv6 address goes in brackets, and only an IPv6 address does.
func brackets(addr netip.Addr, bracketed bool) (why, fix string) {
	switch {
	case addr.Is6() && !bracketed:
		return "has an IPv6 address that is not in brackets [ ]", "Write an IPv6 address in brackets, as in [::1]"
	case !addr.Is6() && bracketed:
		return "has brackets [ ] around a host that is not an IPv6 address",
			"Write brackets only around an IPv6 address, as in [::1]"
	}
	return "", ""
}

// String returns the address as host:port, which ParseListenAddr gives
// back unchanged.
func (a ListenAddr) String() string {
	return net.JoinHostPort(a.host, strconv.Itoa(int(a.port)))
}

// Port returns the port, from 1 to 65535.
func (a ListenAddr) Port() uint16 {
	return a.port
}

// HSTSScope is how far Strict-Transport-Security reaches, over https.
type HSTSScope uint8

const (
	// HSTSHostOnly covers the base URL's host alone. It is the default.
	HSTSHostOnly HSTSScope = iota
	// HSTSIncludeSubdomains covers its subdomains too.
	HSTSIncludeSubdomains
	// HSTSPreload covers its subdomains, and asks to be preloaded.
	HSTSPreload
)

// String returns s as the configuration spells it: host-only,
// include-subdomains or preload.
func (s HSTSScope) String() string {
	switch s {
	case HSTSHostOnly:
		return "host-only"
	case HSTSIncludeSubdomains:
		return "include-subdomains"
	case HSTSPreload:
		return "preload"
	}
	return "HSTSScope(" + strconv.Itoa(int(s)) + ")"
}

// LogFormat is how log records are written.
type LogFormat uint8

const (
	// LogJSON writes one JSON object a record. It is the default.
	LogJSON LogFormat = iota
	// LogText writes slog's key=value text.
	LogText
)

// String returns f as the configuration spells it: json or text.
func (f LogFormat) String() string {
	switch f {
	case LogJSON:
		return "json"
	case LogText:
		return "text"
	}
	return "LogFormat(" + strconv.Itoa(int(f)) + ")"
}

// Switch turns a feature on or off.
type Switch uint8

const (
	// Off is the default.
	Off Switch = iota
	// On turns the feature on.
	On
)

// String returns s as the configuration spells it: off or on.
func (s Switch) String() string {
	switch s {
	case Off:
		return "off"
	case On:
		return "on"
	}
	return "Switch(" + strconv.Itoa(int(s)) + ")"
}

// OptionalURL is an absolute http or https URL, or nothing. A URL it
// holds has a host and no user information, query or fragment, and its
// path holds no character a URL must percent-encode unescaped; its host
// is in the same canonical form as a BaseURL's. The zero OptionalURL
// holds nothing.
type OptionalURL struct {
	u   url.URL
	set bool
}

// ParseOptionalURL returns s as an OptionalURL: an empty s gives one that
// holds nothing. It returns an error wrapping ErrInvalid when s is not
// such a URL, saying why without repeating s, which may hold a password.
func ParseOptionalURL(s string) (OptionalURL, error) {
	if s == "" {
		return OptionalURL{}, nil
	}
	u, why := parseWebURL(s)
	if why != "" {
		return OptionalURL{}, &badValue{
			hidden: whyURLHidden, why: why,
			fix: "Set it to an https or http URL, or to nothing",
		}
	}
	return OptionalURL{u: u, set: true}, nil
}

// Get returns the URL, and whether there is one.
func (o OptionalURL) Get() (url.URL, bool) {
	return o.u, o.set
}

// parseWebURL returns s as an absolute http or https URL with a host and
// no user information, query or fragment, whose path holds no character
// a URL must percent-encode unescaped, its host in canonical form; or why
// s is not one, as what follows "a value that", which never repeats s.
func parseWebURL(s string) (_ url.URL, why string) {
	// Checked on the text, since url.Parse keeps no trace of an empty
	// query or fragment, and refuses a control character only as no URL
	// at all.
	switch {
	case strings.ContainsAny(s, "?#"):
		return url.URL{}, `has a query, after "?", or a fragment, after "#"`
	case strings.ContainsFunc(s, control):
		return url.URL{}, "has a control character in it, such as a tab or a line break"
	}
	u, err := url.Parse(s)
	switch {
	case err != nil:
		return url.URL{}, "cannot be read as a URL"
	case u.Scheme != "https" && u.Scheme != "http":
		return url.URL{}, "is not an https or http URL"
	case u.Opaque != "" || u.Host == "":
		return url.URL{}, `has no host after "//", as in https://blog.example.org`
	case u.User != nil:
		return url.URL{}, `has a user name or a password in it, before "@"`
	// RFC 3986 lets a path hold some characters only percent-encoded.
	// url.Parse keeps them unescaped, and String then escapes most of
	// them, a space among them, so the URL would not be the one given;
	// it leaves "[" and "]" as they are, which are refused all the same.
	// The path as given is RawPath, or, when url.Parse leaves that
	// empty, the path's default escaping, which is EscapedPath and holds
	// none of them.
	case !escapedPath(cmp.Or(u.RawPath, u.EscapedPath())):
		return url.URL{}, "has a character in its path that a URL must percent-encode, such as a space, written %20"
	}
	host, why := canonicalHost(u)
	if why != "" {
		return url.URL{}, why
	}
	u.Host = host
	return *u, ""
}

// canonicalHost returns u's host and port in canonical form: a name in
// lower case, an IP address in its shortest form, IPv6 in brackets, and
// the port only when it is not the scheme's default; or why they are not
// a host and a port, as what follows "a value that".
func canonicalHost(u *url.URL) (_, why string) {
	name := u.Hostname()
	bracketed := strings.HasPrefix(u.Host, "[")
	var host string
	if addr, err := netip.ParseAddr(name); err == nil {
		if why, _ := brackets(addr, bracketed); why != "" {
			return "", why
		}
		switch {
		case addr.Zone() != "":
			return "", "has an IPv6 address with a zone, such as %25eth0, which Pisati does not take"
		case addr.Is6():
			host = "[" + addr.String() + "]"
		default:
			host = addr.String()
		}
	} else if bracketed || !hostName(name) {
		return "", "has a host that is neither a host name nor an IP address"
	} else {
		host = strings.ToLower(name)
	}

	portText := u.Port()
	if portText == "" {
		if strings.HasSuffix(u.Host, ":") {
			return "", `has a ":" after its host, but no port`
		}
		return host, ""
	}
	port, why := parsePort(portText)
	if why != "" {
		return "", why
	}
	if (u.Scheme == "https" && port == 443) || (u.Scheme == "http" && port == 80) {
		return host, ""
	}
	return host + ":" + strconv.Itoa(int(port)), ""
}

// control reports whether r is an ASCII control character, which a URL
// holds only percent-encoded.
func control(r rune) bool {
	return r < ' ' || r == 0x7f
}

// escapedPath reports whether p, a URL's path as given, holds only what
// RFC 3986 lets a path hold as it is: letters, digits, "-._~", the
// sub-delimiters "!$&'()*+,;=", ":", "@" and "/", and "%" followed by two
// hex digits.
func escapedPath(p string) bool {
	for i := 0; i < len(p); i++ {
		switch c := p[i]; {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9',
			strings.IndexByte("-._~!$&'()*+,;=:@/", c) >= 0:
		case c == '%' && i+2 < len(p) && hexDigit(p[i+1]) && hexDigit(p[i+2]):
			i += 2
		default:
			return false
		}
	}
	return true
}

// hexDigit reports whether c is a hex digit, in either case.
func hexDigit(c byte) bool {
	return strings.IndexByte("0123456789abcdefABCDEF", c) >= 0
}

// parsePort returns s as a port from 1 to 65535, or why it is not one.
func parsePort(s string) (_ uint16, why string) {
	n, err := strconv.ParseUint(s, 10, 16)
	if err != nil || n == 0 || n > math.MaxUint16 {
		return 0, "has a port that is not a number from 1 to 65535"
	}
	return uint16(n), ""
}

// hostName reports whether name is a DNS host name: dot-separated labels
// of letters, digits, hyphens and underscores, none empty, longer than 63
// bytes, or starting or ending with a hyphen, at most 253 bytes in all.
// A last label that reads as a number is refused too, as a URL parser
// would take the name for an IPv4 address.
func hostName(name string) bool {
	if name == "" || len(name) > 253 {
		return false
	}
	labels := strings.Split(name, ".")
	for _, label := range labels {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		if strings.ContainsFunc(label, notHostRune) {
			return false
		}
	}
	return !numeric(labels[len(labels)-1])
}

// notHostRune reports whether r may not appear in a host name label.
func notHostRune(r rune) bool {
	switch {
	case 'a' <= r && r <= 'z', 'A' <= r && r <= 'Z', '0' <= r && r <= '9', r == '-', r == '_':
		return false
	}
	return true
}

// numeric reports whether label is a decimal or 0x-prefixed hex number.
func numeric(label string) bool {
	digits, base := label, "0123456789"
	if rest, ok := strings.CutPrefix(strings.ToLower(label), "0x"); ok {
		digits, base = rest, "0123456789abcdef"
	}
	return strings.Trim(digits, base) == ""
}

// loopback reports whether host, as canonicalHost writes it without a
// port or brackets, is localhost, 127.0.0.1 or ::1.
func loopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// choices is a closed set of values, at least one, each spelt as spell
// gives it.
type choices[T comparable] struct {
	spell func(T) string
	all   []T
}

// oneOf returns the closed set of first and the rest.
func oneOf[T comparable](spell func(T) string, first T, rest ...T) choices[T] {
	return choices[T]{spell: spell, all: append([]T{first}, rest...)}
}

// spelling lists the spellings, as "a, b or c".
func (c choices[T]) spelling() string {
	names := make([]string, len(c.all))
	for i, v := range c.all {
		names[i] = c.spell(v)
	}
	if len(names) == 1 {
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " or " + names[len(names)-1]
}

// parse returns the value spelt s, or an error wrapping ErrInvalid that
// lists the spellings.
func (c choices[T]) parse(s string) (T, error) {
	for _, v := range c.all {
		if c.spell(v) == s {
			return v, nil
		}
	}
	var zero T
	return zero, &badValue{value: s, why: "is not one of the values it takes", fix: "Set it to " + c.spelling()}
}
