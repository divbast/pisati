// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package httpx_test

import (
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/divbast/pisati/internal/httpx"
)

// proxies are the trusted proxies TestClientIP and FuzzForwardedFor use:
// an IPv4 prefix, an IPv6 one, and an IPv4 prefix in its IPv4-mapped
// spelling, which trusts the IPv4 addresses it maps.
func proxies() []netip.Prefix {
	return []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("2001:db8:1::/48"),
		netip.MustParsePrefix("::ffff:172.16.0.0/108"),
	}
}

// forwarded returns a request from remote whose X-Forwarded-For header
// has lines, in order.
func forwarded(remote string, lines ...string) *http.Request {
	r := &http.Request{RemoteAddr: remote, Header: http.Header{}}
	for _, l := range lines {
		r.Header.Add("X-Forwarded-For", l)
	}
	return r
}

func TestTrustedPeer(t *testing.T) {
	t.Parallel()

	trusted := httpx.NewTrustedPeer(proxies())
	tests := []struct {
		name   string
		remote string
		want   bool
	}{
		{name: "a peer in an IPv4 prefix", remote: "10.0.0.1:4000", want: true},
		{name: "a peer in an IPv6 prefix", remote: "[2001:db8:1::5]:4000", want: true},
		{name: "an IPv4-mapped peer in an IPv4 prefix", remote: "[::ffff:10.0.0.1]:4000", want: true},
		{name: "an IPv4 peer in an IPv4-mapped prefix", remote: "172.16.0.9:4000", want: true},
		{name: "a peer outside every prefix", remote: "192.0.2.7:4000", want: false},
		{name: "what it forwards is not the peer", remote: "192.0.2.7:4000", want: false},
		{name: "a peer that is not an address", remote: "pipe", want: false},
		{name: "a peer without a port", remote: "10.0.0.1", want: false},
		{name: "no peer", remote: "", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := trusted(forwarded(tt.remote, "10.0.0.2")); got != tt.want {
				t.Errorf("a request from %q: trusted %t, want %t", tt.remote, got, tt.want)
			}
		})
	}

	none := httpx.NewTrustedPeer(nil)
	if none(forwarded("10.0.0.1:4000")) {
		t.Error("with no prefixes, a peer is trusted")
	}
}

func TestClientIP(t *testing.T) {
	t.Parallel()

	ip := httpx.NewClientIP(proxies())
	tests := []struct {
		name   string
		remote string
		lines  []string
		// want is the client's address, or "" for none.
		want string
	}{
		{name: "an untrusted peer is the client, whatever it forwards", remote: "192.0.2.7:4000", lines: []string{"198.51.100.1"}, want: "192.0.2.7"},
		{name: "a trusted peer that forwards nothing", remote: "10.0.0.1:4000", want: "10.0.0.1"},
		{name: "a trusted peer's hop", remote: "10.0.0.1:4000", lines: []string{"198.51.100.1"}, want: "198.51.100.1"},
		{name: "a forged left-most hop is ignored", remote: "10.0.0.1:4000", lines: []string{"203.0.113.66, 198.51.100.1"}, want: "198.51.100.1"},
		{name: "trusted hops are skipped", remote: "10.0.0.1:4000", lines: []string{"203.0.113.66, 198.51.100.1, 10.1.2.3, 10.0.0.2"}, want: "198.51.100.1"},
		{name: "every header line, in order", remote: "10.0.0.1:4000", lines: []string{"203.0.113.66", "198.51.100.1, 10.0.0.2"}, want: "198.51.100.1"},
		{name: "the last line is right-most", remote: "10.0.0.1:4000", lines: []string{"198.51.100.1", "10.0.0.2"}, want: "198.51.100.1"},
		{name: "spaces and tabs around hops", remote: "10.0.0.1:4000", lines: []string{" 203.0.113.66 ,\t198.51.100.1\t, 10.0.0.2 "}, want: "198.51.100.1"},
		{name: "every hop trusted gives the left-most", remote: "10.0.0.1:4000", lines: []string{"10.0.0.3, 10.0.0.2"}, want: "10.0.0.3"},
		{name: "an unparsable hop gives the proxy that added it", remote: "10.0.0.1:4000", lines: []string{"198.51.100.1, unknown, 10.0.0.2"}, want: "10.0.0.2"},
		{name: "an unparsable last hop gives the peer", remote: "10.0.0.1:4000", lines: []string{"198.51.100.1, unknown"}, want: "10.0.0.1"},
		{name: "an unparsable line stops the walk", remote: "10.0.0.1:4000", lines: []string{"198.51.100.1", "unknown", "10.0.0.2"}, want: "10.0.0.2"},
		{name: "an empty hop is unparsable", remote: "10.0.0.1:4000", lines: []string{"198.51.100.1,,10.0.0.2"}, want: "10.0.0.2"},
		{name: "an empty line is unparsable", remote: "10.0.0.1:4000", lines: []string{"198.51.100.1", ""}, want: "10.0.0.1"},
		{name: "a hop with a port is its address", remote: "10.0.0.1:4000", lines: []string{"198.51.100.1:80"}, want: "198.51.100.1"},
		{name: "an IPv6 hop with a port is its address", remote: "10.0.0.1:4000", lines: []string{"[2001:db8:2::9]:443"}, want: "2001:db8:2::9"},
		{name: "a forged left-most hop with a port is ignored", remote: "10.0.0.1:4000", lines: []string{"203.0.113.66:1, 198.51.100.1:2"}, want: "198.51.100.1"},
		{name: "a trusted hop with a port is skipped", remote: "10.0.0.1:4000", lines: []string{"198.51.100.1, 10.0.0.2:80"}, want: "198.51.100.1"},
		{name: "a hop with an empty port is unparsable", remote: "10.0.0.1:4000", lines: []string{"198.51.100.1:"}, want: "10.0.0.1"},
		{name: "a bracketed hop without a port is unparsable", remote: "10.0.0.1:4000", lines: []string{"[2001:db8:2::9]"}, want: "10.0.0.1"},
		{name: "an IPv6 peer and hops", remote: "[2001:db8:1::5]:4000", lines: []string{"2001:db8:2::9, 2001:db8:1::7"}, want: "2001:db8:2::9"},
		{name: "an IPv4-mapped peer matches an IPv4 prefix", remote: "[::ffff:10.0.0.1]:4000", lines: []string{"198.51.100.1"}, want: "198.51.100.1"},
		{name: "an IPv4-mapped untrusted peer is unmapped", remote: "[::ffff:192.0.2.7]:4000", want: "192.0.2.7"},
		{name: "an IPv4-mapped hop is unmapped", remote: "10.0.0.1:4000", lines: []string{"::ffff:198.51.100.1"}, want: "198.51.100.1"},
		{name: "an IPv4-mapped trusted hop is skipped", remote: "10.0.0.1:4000", lines: []string{"198.51.100.1, ::ffff:10.9.9.9"}, want: "198.51.100.1"},
		{name: "an IPv4-mapped prefix trusts IPv4 peers", remote: "172.16.0.9:4000", lines: []string{"198.51.100.1"}, want: "198.51.100.1"},
		{name: "a peer's zone is dropped", remote: "[fe80::1%en0]:4000", want: "fe80::1"},
		{name: "a hop's zone is dropped", remote: "10.0.0.1:4000", lines: []string{"fe80::2%en0"}, want: "fe80::2"},
		{name: "a peer that is not an address has none", remote: "pipe"},
		{name: "a peer without a port has none", remote: "192.0.2.7"},
		{name: "no peer has none", remote: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := ip(forwarded(tt.remote, tt.lines...))
			if tt.want == "" {
				if ok || got.IsValid() {
					t.Errorf("ClientIP(%q, %q) = %v, %t; want no address", tt.remote, tt.lines, got, ok)
				}
				return
			}
			if want := netip.MustParseAddr(tt.want); !ok || got != want {
				t.Errorf("ClientIP(%q, %q) = %v, %t; want %v, true", tt.remote, tt.lines, got, ok, want)
			}
		})
	}

	t.Run("the trusted prefixes are copied", func(t *testing.T) {
		t.Parallel()

		trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
		ip := httpx.NewClientIP(trusted)
		trusted[0] = netip.MustParsePrefix("192.0.2.0/24")
		r := forwarded("10.0.0.1:4000", "198.51.100.1")
		if got, ok := ip(r); !ok || got != netip.MustParseAddr("198.51.100.1") {
			t.Errorf("after the caller's slice changed, ClientIP = %v, %t; want 198.51.100.1, true", got, ok)
		}
	})

	t.Run("nothing trusted", func(t *testing.T) {
		t.Parallel()

		for _, trusted := range [][]netip.Prefix{nil, {}, {netip.Prefix{}}} {
			ip := httpx.NewClientIP(trusted)
			if got, ok := ip(forwarded("10.0.0.1:4000", "198.51.100.1")); !ok || got != netip.MustParseAddr("10.0.0.1") {
				t.Errorf("NewClientIP(%v): ClientIP = %v, %t; want the peer 10.0.0.1, true", trusted, got, ok)
			}
		}
	})
}

// hops returns every hop lines hold, in order, each trimmed of spaces and
// tabs.
func hops(lines ...string) []string {
	var out []string
	for _, l := range lines {
		for h := range strings.SplitSeq(l, ",") {
			out = append(out, strings.Trim(h, " \t"))
		}
	}
	return out
}

// parseHop reads one hop as ClientIP is to: an address, or an address
// and port, without a zone and unmapped.
func parseHop(h string) (netip.Addr, bool) {
	if a, err := netip.ParseAddr(h); err == nil {
		return a.WithZone("").Unmap(), true
	}
	if ap, err := netip.ParseAddrPort(h); err == nil {
		return ap.Addr().WithZone("").Unmap(), true
	}
	return netip.Addr{}, false
}

// referenceClient is ClientIP's rule written as plainly as it reads: a
// trusted peer's hops are walked from the right, past trusted ones, and a
// hop that is not an address stops the walk at the one to its right.
func referenceClient(peer netip.Addr, all []string, isTrusted func(netip.Addr) bool) netip.Addr {
	if !isTrusted(peer) {
		return peer
	}
	client := peer
	for _, h := range slices.Backward(all) {
		a, ok := parseHop(h)
		if !ok {
			return client
		}
		client = a
		if !isTrusted(a) {
			return a
		}
	}
	return client
}

func FuzzForwardedFor(f *testing.F) {
	for _, seed := range [][3]string{
		{"10.0.0.1:4000", "203.0.113.66, 198.51.100.1", "10.0.0.2"},
		{"10.0.0.1:4000", "198.51.100.1, unknown", "10.0.0.2"},
		{"[2001:db8:1::5]:4000", "2001:db8:2::9, 2001:db8:1::7", ""},
		{"[::ffff:10.0.0.1]:4000", "::ffff:198.51.100.1,,", "fe80::1%en0"},
		{"10.0.0.1:4000", "203.0.113.66:1, [2001:db8:2::9]:443", "10.0.0.2:80"},
		{"172.16.0.9:4000", "10.0.0.3, 10.0.0.2", "::ffff:10.9.9.9"},
		{"192.0.2.7:4000", "198.51.100.1", ""},
		{"pipe", "198.51.100.1", ""},
	} {
		f.Add(seed[0], seed[1], seed[2])
	}
	trusted := proxies()
	ip := httpx.NewClientIP(trusted)
	isTrusted := func(a netip.Addr) bool {
		return slices.ContainsFunc(trusted, func(p netip.Prefix) bool {
			if p.Addr().Is4In6() {
				p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96)
			}
			return p.Contains(a)
		})
	}
	f.Fuzz(func(t *testing.T, remote, line1, line2 string) {
		got, ok := ip(forwarded(remote, line1, line2))
		peer, err := netip.ParseAddrPort(remote)
		if ok != (err == nil) {
			t.Fatalf("ClientIP(%q) found an address: %t, want %t", remote, ok, err == nil)
		}
		if !ok {
			return
		}
		if !got.IsValid() || got.Zone() != "" || got.Is4In6() {
			t.Fatalf("ClientIP = %v, want a valid address without a zone or an IPv4 mapping", got)
		}
		from := peer.Addr().WithZone("").Unmap()
		if !isTrusted(from) {
			if got != from {
				t.Fatalf("an untrusted peer %v gave %v, want the peer", from, got)
			}
			return
		}
		// The client is the peer or a hop to the right of the right-most
		// hop that is not an address.
		all := hops(line1, line2)
		start := 0
		for i, h := range all {
			if _, ok := parseHop(h); !ok {
				start = i + 1
			}
		}
		candidates := []netip.Addr{from}
		for _, h := range all[start:] {
			a, _ := parseHop(h)
			candidates = append(candidates, a)
		}
		if !slices.Contains(candidates, got) {
			t.Fatalf("ClientIP(%q, %q, %q) = %v, which is not the peer or a hop right of every unparsable one: %v",
				remote, line1, line2, got, candidates)
		}
		if want := referenceClient(from, all, isTrusted); got != want {
			t.Fatalf("ClientIP(%q, %q, %q) = %v, want %v, the right-most hop that is not trusted",
				remote, line1, line2, got, want)
		}
	})
}
