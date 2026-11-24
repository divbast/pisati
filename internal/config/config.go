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
	"errors"
	"fmt"
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
// source it could not read, that source's own error.
var (
	// ErrInvalid is a value its rule refuses, or a file that is not TOML.
	ErrInvalid = errors.New("invalid")
	// ErrUnknown is a TOML key or a PISATI_ variable that names no setting.
	ErrUnknown = errors.New("unknown setting")
	// ErrMissing is a setting that has no default and was not given.
	ErrMissing = errors.New("missing")
)

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
	// MasterKey is never nil in a Config that Load returns without an
	// error.
	MasterKey SecretSource
}

// Dev holds settings for development alone.
type Dev struct {
	// ViteURL is the Vite dev server. Load sets it only when it and the
	// base URL are both on localhost, 127.0.0.1 or [::1].
	ViteURL OptionalURL
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
// that says why it is not one. A path of exactly "/" is accepted and
// left out.
func ParseBaseURL(s string) (BaseURL, error) {
	u, err := parseWebURL(s)
	if err == nil {
		if p := u.EscapedPath(); p != "" && p != "/" {
			err = errors.New("it has a path")
		} else if u.Scheme == "http" && !loopback(u.Hostname()) {
			err = errors.New("it is plain http away from localhost")
		}
	}
	if err != nil {
		return BaseURL{}, fmt.Errorf("%w value %q: %w; want https://host[:port], or http:// on localhost, 127.0.0.1 or [::1] alone", ErrInvalid, s, err)
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
	a, err := parseListenAddr(s)
	if err != nil {
		return ListenAddr{}, fmt.Errorf("%w value %q: %w; want host:port, with a port from 1 to 65535", ErrInvalid, s, err)
	}
	return a, nil
}

func parseListenAddr(s string) (ListenAddr, error) {
	host, portText, err := net.SplitHostPort(s)
	if err != nil {
		return ListenAddr{}, errors.New("it is not host:port")
	}
	port, err := parsePort(portText)
	if err != nil {
		return ListenAddr{}, err
	}
	bracketed := strings.HasPrefix(s, "[")
	if addr, err := netip.ParseAddr(host); err == nil {
		if addr.Is6() != bracketed {
			return ListenAddr{}, errors.New("only an IPv6 address goes in brackets")
		}
		return ListenAddr{host: addr.String(), port: port}, nil
	}
	if host != "" && (bracketed || !hostName(host)) {
		return ListenAddr{}, errors.New("its host is not a host name or an IP address")
	}
	return ListenAddr{host: strings.ToLower(host), port: port}, nil
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
// holds has a host and no user information, query or fragment; its host
// is in the same canonical form as a BaseURL's. The zero OptionalURL
// holds nothing.
type OptionalURL struct {
	u   url.URL
	set bool
}

// ParseOptionalURL returns s as an OptionalURL: an empty s gives one that
// holds nothing. It returns an error wrapping ErrInvalid when s is not
// such a URL, saying why.
func ParseOptionalURL(s string) (OptionalURL, error) {
	if s == "" {
		return OptionalURL{}, nil
	}
	u, err := parseWebURL(s)
	if err != nil {
		return OptionalURL{}, fmt.Errorf("%w value %q: %w; want an http or https URL", ErrInvalid, s, err)
	}
	return OptionalURL{u: u, set: true}, nil
}

// Get returns the URL, and whether there is one.
func (o OptionalURL) Get() (url.URL, bool) {
	return o.u, o.set
}

// parseWebURL returns s as an absolute http or https URL with a host and
// no user information, query or fragment, its host in canonical form.
// Its errors say why s is not one, without repeating s.
func parseWebURL(s string) (url.URL, error) {
	// Checked on the text, since url.Parse keeps no trace of an empty
	// query or fragment.
	if strings.ContainsAny(s, "?#") {
		return url.URL{}, errors.New("it has a query or a fragment")
	}
	u, err := url.Parse(s)
	switch {
	case err != nil:
		return url.URL{}, errors.New("it is not a URL")
	case u.Scheme != "https" && u.Scheme != "http":
		return url.URL{}, errors.New("its scheme is not https or http")
	case u.Opaque != "" || u.Host == "":
		return url.URL{}, errors.New("it has no host")
	case u.User != nil:
		return url.URL{}, errors.New("it holds user information")
	}
	host, err := canonicalHost(u)
	if err != nil {
		return url.URL{}, err
	}
	u.Host = host
	return *u, nil
}

// canonicalHost returns u's host and port in canonical form: a name in
// lower case, an IP address in its shortest form, IPv6 in brackets, and
// the port only when it is not the scheme's default.
func canonicalHost(u *url.URL) (string, error) {
	name := u.Hostname()
	bracketed := strings.HasPrefix(u.Host, "[")
	var host string
	if addr, err := netip.ParseAddr(name); err == nil {
		switch {
		case addr.Is6() != bracketed:
			return "", errors.New("only an IPv6 address goes in brackets, and it must")
		case addr.Zone() != "":
			return "", errors.New("its host names an IPv6 zone")
		case addr.Is6():
			host = "[" + addr.String() + "]"
		default:
			host = addr.String()
		}
	} else if bracketed || !hostName(name) {
		return "", errors.New("its host is not a host name or an IP address")
	} else {
		host = strings.ToLower(name)
	}

	portText := u.Port()
	if portText == "" {
		if strings.HasSuffix(u.Host, ":") {
			return "", errors.New("it has a colon but no port")
		}
		return host, nil
	}
	port, err := parsePort(portText)
	if err != nil {
		return "", err
	}
	if (u.Scheme == "https" && port == 443) || (u.Scheme == "http" && port == 80) {
		return host, nil
	}
	return host + ":" + strconv.Itoa(int(port)), nil
}

// parsePort returns s as a port from 1 to 65535.
func parsePort(s string) (uint16, error) {
	n, err := strconv.ParseUint(s, 10, 16)
	if err != nil || n == 0 || n > math.MaxUint16 {
		return 0, errors.New("its port is not from 1 to 65535")
	}
	return uint16(n), nil
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
	return zero, fmt.Errorf("%w value %q: want %s", ErrInvalid, s, c.spelling())
}
