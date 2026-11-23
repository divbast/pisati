// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package httpx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"syscall"
	"time"
)

// ErrAddressRefused is a request the GuardedClient will not send: its
// URL's scheme, or a redirect's, is not http or https, or its host has
// no address but ones the client refuses. Do's error for one wraps it.
var ErrAddressRefused = errors.New("address refused")

// ErrTooLarge is a response body longer than GuardedOptions.MaxBody.
// Reading the body past the limit returns an error wrapping it.
var ErrTooLarge = errors.New("response body too large")

// ErrTooManyRedirects is a request that is redirected more often than
// GuardedOptions.MaxRedirects allows. Do's error for one wraps it.
var ErrTooManyRedirects = errors.New("too many redirects")

// GuardedOptions are how a GuardedClient sends. NewGuardedClient refuses
// options with any field outside its range, the zero GuardedOptions
// included. DefaultGuardedOptions returns the values Pisati sends with.
type GuardedOptions struct {
	// Timeout bounds a whole exchange: looking up the host, connecting,
	// the request, every redirect, and reading the response's body.
	// Positive.
	Timeout time.Duration
	// MaxBody is how many bytes of a response's body may be read.
	// Positive.
	MaxBody ByteLimit
	// MaxRedirects is how many redirects a request follows: 0 follows
	// none. Not negative.
	MaxRedirects int
	// Lookup returns the addresses of a host name, in the order they are
	// to be tried. Nil, as Pisati leaves it, asks the system's resolver.
	// Tests set it, so that no name is looked up on the network.
	Lookup func(ctx context.Context, host string) ([]netip.Addr, error)
	// Connect opens a connection to an address the client's check has
	// passed: an IP address and port, never a name. ctx bounds the
	// connecting alone. Nil, as Pisati leaves it, dials with a net.Dialer
	// whose Control checks the address again, on the socket about to
	// connect. Tests set it, to reach a server on loopback, or none,
	// without leaving the machine.
	Connect func(ctx context.Context, network string, to netip.AddrPort) (net.Conn, error)
}

// DefaultGuardedOptions returns the GuardedOptions Pisati sends with: a
// 10 s timeout, 1 MiB of response body, and up to 3 redirects.
func DefaultGuardedOptions() GuardedOptions {
	return GuardedOptions{Timeout: 10 * time.Second, MaxBody: 1 << 20, MaxRedirects: 3}
}

// problems returns an error for each field of o outside its range.
func (o GuardedOptions) problems() []error {
	var errs []error
	if o.Timeout <= 0 {
		errs = append(errs, fmt.Errorf("httpx: %w guarded client options: Timeout is %v, want it positive", ErrInvalid, o.Timeout))
	}
	if o.MaxBody <= 0 {
		errs = append(errs, fmt.Errorf("httpx: %w guarded client options: MaxBody is %d, want it positive", ErrInvalid, o.MaxBody))
	}
	if o.MaxRedirects < 0 {
		errs = append(errs, fmt.Errorf("httpx: %w guarded client options: MaxRedirects is %d, want 0 or more",
			ErrInvalid, o.MaxRedirects))
	}
	return errs
}

// GuardedClient is the client every request Pisati sends out goes
// through: an address-checked client, which refuses private and reserved
// addresses, and bounds how long an exchange takes and how much of an
// answer it reads. Make one with NewGuardedClient; it is safe for
// concurrent use. The zero GuardedClient sends nothing: its Do returns
// an error.
//
// It sends only http and https URLs, and connects only to addresses
// outside the ranges it refuses:
//
//   - IPv4: 0.0.0.0/8, which holds the unspecified address, 10.0.0.0/8,
//     100.64.0.0/10, 127.0.0.0/8, 169.254.0.0/16, 172.16.0.0/12,
//     192.0.2.0/24, 192.168.0.0/16, 198.18.0.0/15, 198.51.100.0/24,
//     203.0.113.0/24, 224.0.0.0/4 and 240.0.0.0/4;
//   - IPv6: ::/128, ::1/128, 2001:db8::/32, fc00::/7, fe80::/10,
//     ff00::/8, and 64:ff9b:1::/48, the local-use translation prefix,
//     which is not globally reachable, and embeds an IPv4 address
//     wherever the local network chooses;
//   - an IPv6 address that embeds an IPv4 address refused above:
//     IPv4-mapped (::ffff:0:0/96), NAT64 (64:ff9b::/96) or 6to4
//     (2002::/16).
//
// The check is made as each connection is: a host's addresses are looked
// up, the refused ones are skipped, and the rest are tried in turn, each
// given Timeout divided by how many are left to try, so that one that
// never answers leaves time for the others. A redirect is checked as the
// request is, and connects through the same check, so a name whose
// answer changes between lookups is checked on every connection it
// makes. No proxy is used, whatever the environment says.
type GuardedClient struct {
	client  *http.Client
	check   addressCheck
	lookup  func(ctx context.Context, host string) ([]netip.Addr, error)
	connect func(ctx context.Context, network string, to netip.AddrPort) (net.Conn, error)
	timeout time.Duration
	maxBody int64
}

// NewGuardedClient returns a GuardedClient that sends within o's bounds.
// It returns an error wrapping ErrInvalid, naming every problem at once,
// when o has a field outside its range.
func NewGuardedClient(o GuardedOptions) (*GuardedClient, error) {
	if err := errors.Join(o.problems()...); err != nil {
		return nil, err
	}
	c := &GuardedClient{
		check:   newAddressCheck(),
		lookup:  o.Lookup,
		connect: o.Connect,
		timeout: o.Timeout,
		maxBody: int64(o.MaxBody),
	}
	if c.lookup == nil {
		c.lookup = systemLookup
	}
	if c.connect == nil {
		d := &net.Dialer{Control: c.check.control}
		c.connect = func(ctx context.Context, network string, to netip.AddrPort) (net.Conn, error) {
			return d.DialContext(ctx, network, to.String())
		}
	}
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	protocols.SetHTTP2(true)
	maxRedirects := o.MaxRedirects
	c.client = &http.Client{
		Transport: &http.Transport{
			// Never http.ProxyFromEnvironment: the environment is main's
			// to read, and a proxy would connect where the check cannot
			// see.
			Proxy:       nil,
			DialContext: c.dial,
			// The transport bounds a handshake only when told to.
			TLSHandshakeTimeout: o.Timeout,
			// The rest are http.DefaultTransport's.
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			ExpectContinueTimeout: time.Second,
			Protocols:             &protocols,
		},
		Timeout: o.Timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > maxRedirects {
				return fmt.Errorf("httpx: %w: more than %d", ErrTooManyRedirects, maxRedirects)
			}
			return c.check.url(req.URL)
		},
	}
	return c, nil
}

// Do sends req, with ctx in place of its own context, and returns the
// response, as http.Client's Do does. Before anything is looked up, it
// refuses, with an error wrapping ErrAddressRefused, a URL whose scheme
// is not http or https, or whose host is an address the client refuses.
// The response's body fails to read past GuardedOptions.MaxBody bytes,
// with an error wrapping ErrTooLarge, and closing it still works as
// ever. The caller closes it.
func (c *GuardedClient) Do(ctx context.Context, req *http.Request) (*http.Response, error) {
	if c == nil || c.client == nil {
		return nil, errors.New("httpx: the GuardedClient was not made by NewGuardedClient")
	}
	if req == nil || req.URL == nil {
		return nil, errors.New("httpx: Do needs a request with a URL")
	}
	if err := c.check.url(req.URL); err != nil {
		return nil, err
	}
	resp, err := c.client.Do(req.WithContext(ctx)) //nolint:gosec // G704: this is the address-checked client; the check above, CheckRedirect and dial check every URL and address
	if err != nil {
		return nil, err
	}
	resp.Body = &cappedBody{body: resp.Body, left: c.maxBody, limit: c.maxBody}
	return resp, nil
}

// CloseIdleConnections closes the connections c keeps open between
// requests, which ends the goroutines that serve them. Pisati calls it as
// it shuts down. The zero GuardedClient has none.
func (c *GuardedClient) CloseIdleConnections() {
	if c == nil || c.client == nil {
		return
	}
	c.client.CloseIdleConnections()
}

// dial connects to address, a host and port, for the transport: to the
// first of the host's addresses the check passes that answers.
func (c *GuardedClient) dial(ctx context.Context, network, address string) (net.Conn, error) {
	// The transport dials on past the request it dials for, without the
	// request's deadline.
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("httpx: dialling %q: %w", address, err)
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil {
		return nil, fmt.Errorf("httpx: dialling %q: the port is not a number from 0 to 65535", address)
	}
	addrs, err := c.addresses(ctx, host)
	if err != nil {
		return nil, err
	}
	var allowed []netip.AddrPort
	for _, a := range addrs {
		if a = plain(a); c.check.allows(a) {
			allowed = append(allowed, netip.AddrPortFrom(a, uint16(port)))
		}
	}
	if len(allowed) == 0 {
		return nil, fmt.Errorf("httpx: %w: %s has only private or reserved addresses: %v", ErrAddressRefused, host, addrs)
	}
	errs := make([]error, 0, len(allowed))
	for i, to := range allowed {
		attempt, stop := context.WithTimeout(ctx, c.timeout/time.Duration(len(allowed)-i))
		conn, err := c.connect(attempt, network, to)
		stop()
		if err == nil {
			return conn, nil
		}
		errs = append(errs, fmt.Errorf("httpx: connecting to %v for %s: %w", to, host, err))
	}
	return nil, errors.Join(errs...)
}

// addresses returns the addresses of host: itself when it is an address,
// and otherwise what a lookup returns.
func (c *GuardedClient) addresses(ctx context.Context, host string) ([]netip.Addr, error) {
	if a, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{a}, nil
	}
	addrs, err := c.lookup(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("httpx: looking up %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("httpx: looking up %s: it has no address", host)
	}
	return addrs, nil
}

// systemLookup returns host's addresses from the system's resolver.
func systemLookup(ctx context.Context, host string) ([]netip.Addr, error) {
	var r net.Resolver
	return r.LookupNetIP(ctx, "ip", host)
}

// addressCheck says which addresses a GuardedClient may connect to.
type addressCheck struct {
	// refused4 and refused6 are the ranges refused outright.
	refused4, refused6 []netip.Prefix
	// nat64 and sixToFour embed an IPv4 address, which is checked in turn.
	nat64, sixToFour netip.Prefix
}

// newAddressCheck returns the check GuardedClient's doc describes.
func newAddressCheck() addressCheck {
	parse := func(prefixes ...string) []netip.Prefix {
		out := make([]netip.Prefix, 0, len(prefixes))
		for _, p := range prefixes {
			out = append(out, netip.MustParsePrefix(p))
		}
		return out
	}
	return addressCheck{
		refused4: parse(
			"0.0.0.0/8",       // this network, with the unspecified address
			"10.0.0.0/8",      // private (RFC 1918)
			"100.64.0.0/10",   // shared address space (RFC 6598)
			"127.0.0.0/8",     // loopback
			"169.254.0.0/16",  // link-local
			"172.16.0.0/12",   // private (RFC 1918)
			"192.0.2.0/24",    // documentation, TEST-NET-1
			"192.168.0.0/16",  // private (RFC 1918)
			"198.18.0.0/15",   // benchmarking
			"198.51.100.0/24", // documentation, TEST-NET-2
			"203.0.113.0/24",  // documentation, TEST-NET-3
			"224.0.0.0/4",     // multicast
			"240.0.0.0/4",     // reserved, with the broadcast address
		),
		refused6: parse(
			"::/128",         // unspecified
			"::1/128",        // loopback
			"64:ff9b:1::/48", // local-use translation (RFC 8215)
			"2001:db8::/32",  // documentation
			"fc00::/7",       // unique local
			"fe80::/10",      // link-local
			"ff00::/8",       // multicast
		),
		nat64:     netip.MustParsePrefix("64:ff9b::/96"),
		sixToFour: netip.MustParsePrefix("2002::/16"),
	}
}

// allows reports whether a GuardedClient may connect to a.
func (k addressCheck) allows(a netip.Addr) bool {
	// Unmapping checks an IPv4-mapped address as the IPv4 address it is.
	a = plain(a)
	if a.Is6() {
		if slices.ContainsFunc(k.refused6, func(p netip.Prefix) bool { return p.Contains(a) }) {
			return false
		}
		b := a.As16()
		switch {
		case k.nat64.Contains(a):
			a = netip.AddrFrom4([4]byte(b[12:16]))
		case k.sixToFour.Contains(a):
			a = netip.AddrFrom4([4]byte(b[2:6]))
		default:
			return true
		}
	}
	return a.Is4() && !slices.ContainsFunc(k.refused4, func(p netip.Prefix) bool { return p.Contains(a) })
}

// url refuses u when its scheme is not http or https, or its host is an
// address the check refuses. A host name is checked as it is connected
// to.
func (k addressCheck) url(u *url.URL) error {
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("httpx: %w: the scheme %q is not http or https", ErrAddressRefused, u.Scheme)
	}
	if a, err := netip.ParseAddr(u.Hostname()); err == nil && !k.allows(a) {
		return fmt.Errorf("httpx: %w: %v is a private or reserved address", ErrAddressRefused, plain(a))
	}
	return nil
}

// control is the net.Dialer's Control: it refuses to connect a socket to
// an address the check refuses. The dialer calls it with the address
// about to be connected to, once any name has been resolved.
func (k addressCheck) control(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("httpx: %w: %q is not an address and port", ErrAddressRefused, address)
	}
	if !k.allows(ap.Addr()) {
		return fmt.Errorf("httpx: %w: %v is a private or reserved address", ErrAddressRefused, plain(ap.Addr()))
	}
	return nil
}

// cappedBody is a response body that fails to read, with an error
// wrapping ErrTooLarge, once more than limit bytes of it would be read.
type cappedBody struct {
	body io.ReadCloser
	// left is how many more bytes may be read.
	left, limit int64
	// err, once the limit is passed, is what every Read returns.
	err error
}

func (b *cappedBody) Read(p []byte) (int, error) {
	if b.err != nil {
		return 0, b.err
	}
	if len(p) == 0 {
		return 0, nil
	}
	// One byte more than may be read tells a body of exactly the limit
	// from a longer one.
	if int64(len(p)) > b.left+1 {
		p = p[:b.left+1]
	}
	n, err := b.body.Read(p)
	if int64(n) <= b.left {
		b.left -= int64(n)
		return n, err
	}
	n = int(b.left)
	b.left = 0
	b.err = fmt.Errorf("httpx: %w: over %d bytes", ErrTooLarge, b.limit)
	return n, b.err
}

// Close closes the body.
func (b *cappedBody) Close() error {
	return b.body.Close()
}
