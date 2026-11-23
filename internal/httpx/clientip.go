// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package httpx

import (
	"net/http"
	"net/netip"
	"slices"
	"strings"
)

// ClientIP returns the address of the client a request comes from, and
// whether it has one. The address is never zoned or IPv4-mapped: an
// IPv4 client's is IPv4.
type ClientIP func(*http.Request) (netip.Addr, bool)

// NewClientIP returns the ClientIP that believes X-Forwarded-For only
// from the proxies trusted holds. It keeps a copy of trusted, so later
// changes to the slice do not reach it. A prefix in its IPv4-mapped
// spelling trusts the IPv4 addresses it maps, and the zero Prefix trusts
// nothing.
//
// The request's peer is its RemoteAddr, an address and port, as
// net/http's server sets it; a request whose RemoteAddr is not one has no
// client address. A peer that is not trusted is the client, whatever it
// forwards. A trusted peer's X-Forwarded-For hops, from every line of the
// header in order, each split at commas and trimmed of spaces and tabs,
// are walked from the right: each trusted proxy appends the address it
// heard from, so the client is the right-most hop that is not trusted.
// Hops further left were written by the client itself, and are never the
// answer. A hop that is not a bare address stops the walk, which gives
// the hop to its right, the trusted proxy that appended it, or the peer.
// When every hop is trusted, the client is the left-most.
func NewClientIP(trusted []netip.Prefix) ClientIP {
	own := make([]netip.Prefix, 0, len(trusted))
	for _, p := range trusted {
		if a := p.Addr(); a.Is4In6() && p.Bits() >= 96 {
			p = netip.PrefixFrom(a.Unmap(), p.Bits()-96)
		}
		own = append(own, p.Masked())
	}
	isTrusted := func(a netip.Addr) bool {
		return slices.ContainsFunc(own, func(p netip.Prefix) bool { return p.Contains(a) })
	}
	return func(r *http.Request) (netip.Addr, bool) {
		peer, err := netip.ParseAddrPort(r.RemoteAddr)
		if err != nil {
			return netip.Addr{}, false
		}
		client := plain(peer.Addr())
		if !isTrusted(client) {
			return client, true
		}
		lines := r.Header.Values("X-Forwarded-For")
		for _, line := range slices.Backward(lines) {
			for {
				i := strings.LastIndexByte(line, ',')
				hop, err := netip.ParseAddr(strings.Trim(line[i+1:], " \t"))
				if err != nil {
					return client, true
				}
				client = plain(hop)
				if !isTrusted(client) {
					return client, true
				}
				if i < 0 {
					break
				}
				line = line[:i]
			}
		}
		return client, true
	}
}

// plain returns a without its zone, and unmapped from IPv6 when it is an
// IPv4 address.
func plain(a netip.Addr) netip.Addr {
	return a.WithZone("").Unmap()
}
