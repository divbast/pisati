// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package httpx_test

import (
	"compress/gzip"
	"context"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/divbast/pisati/internal/httpx"
)

// errNoConnection is what a fakeNet without connect answers each
// connection with.
var errNoConnection = errors.New("test: the fake network makes no connections")

// fakeNet is the network a GuardedClient under test reaches: the names it
// holds resolve to their addresses, and each connection goes to connect,
// or fails with errNoConnection when connect is nil. It records every name
// looked up and every address connected to. Nothing it does leaves the
// machine.
type fakeNet struct {
	names   map[string][]netip.Addr
	connect func(ctx context.Context, to netip.AddrPort) (net.Conn, error)

	mu      sync.Mutex
	lookups []string
	dialled []netip.AddrPort
}

func (n *fakeNet) lookup(_ context.Context, host string) ([]netip.Addr, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.lookups = append(n.lookups, host)
	addrs, ok := n.names[host]
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	return slices.Clone(addrs), nil
}

func (n *fakeNet) dial(ctx context.Context, _ string, to netip.AddrPort) (net.Conn, error) {
	n.mu.Lock()
	n.dialled = append(n.dialled, to)
	n.mu.Unlock()
	if n.connect == nil {
		return nil, errNoConnection
	}
	return n.connect(ctx, to)
}

// seen returns the names looked up and the addresses connected to so far.
func (n *fakeNet) seen() ([]string, []netip.AddrPort) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.lookups), slices.Clone(n.dialled)
}

// guarded returns a GuardedClient of o on n, whose idle connections are
// closed when the test ends.
func guarded(t *testing.T, o httpx.GuardedOptions, n *fakeNet) *httpx.GuardedClient {
	t.Helper()
	o.Lookup = n.lookup
	o.Connect = n.dial
	c, err := httpx.NewGuardedClient(o)
	if err != nil {
		t.Fatalf("NewGuardedClient(%+v): %v", o, err)
	}
	t.Cleanup(c.CloseIdleConnections)
	return c
}

// fetch sends GET url through c with ctx, and returns the answer's status
// and as much of its body as could be read, with the error from sending
// or reading, if any.
func fetch(ctx context.Context, t *testing.T, c *httpx.GuardedClient, url string) (int, string, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("NewRequest %s: %v", url, err)
	}
	resp, err := c.Do(ctx, req)
	if err != nil {
		return 0, "", err
	}
	body, err := io.ReadAll(resp.Body)
	if cerr := resp.Body.Close(); cerr != nil {
		t.Errorf("closing the body of %s: %v", url, cerr)
	}
	return resp.StatusCode, string(body), err
}

// getRequest returns a GET request for url with ctx, failing the test if
// it cannot.
func getRequest(ctx context.Context, t *testing.T, url string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("NewRequest %s: %v", url, err)
	}
	return req
}

// urlOf returns an http URL for a, on port 80.
func urlOf(a netip.Addr) string {
	return "http://" + netip.AddrPortFrom(a, 80).String() + "/"
}

// spellings returns a and, when a is an IPv4 address, the IPv6 addresses
// that embed it: IPv4-mapped, under the NAT64 well-known prefix, and as
// 6to4.
func spellings(a netip.Addr) []netip.Addr {
	if !a.Is4() {
		return []netip.Addr{a}
	}
	v4 := a.As4()
	mapped := netip.AddrFrom16(a.As16())
	nat64 := [16]byte{0x00, 0x64, 0xff, 0x9b, 12: v4[0], 13: v4[1], 14: v4[2], 15: v4[3]}
	sixToFour := [16]byte{0x20, 0x02, v4[0], v4[1], v4[2], v4[3], 15: 1}
	return []netip.Addr{a, mapped, netip.AddrFrom16(nat64), netip.AddrFrom16(sixToFour)}
}

// refusedRanges are the ranges the GuardedClient refuses, each with
// addresses at its edges.
func refusedRanges() []struct {
	name  string
	addrs []string
} {
	return []struct {
		name  string
		addrs []string
	}{
		{name: "unspecified", addrs: []string{"0.0.0.0", "::"}},
		{name: "0.0.0.0/8", addrs: []string{"0.0.0.1", "0.255.255.255"}},
		{name: "loopback", addrs: []string{"127.0.0.1", "127.255.255.255", "::1"}},
		{name: "10.0.0.0/8", addrs: []string{"10.0.0.0", "10.255.255.255"}},
		{name: "172.16.0.0/12", addrs: []string{"172.16.0.0", "172.31.255.255"}},
		{name: "192.168.0.0/16", addrs: []string{"192.168.0.0", "192.168.255.255"}},
		{name: "fc00::/7", addrs: []string{"fc00::", "fdff:ffff:ffff:ffff:ffff:ffff:ffff:ffff"}},
		{name: "link-local", addrs: []string{"169.254.0.0", "169.254.255.255", "fe80::", "febf:ffff:ffff:ffff:ffff:ffff:ffff:ffff"}},
		{name: "100.64.0.0/10", addrs: []string{"100.64.0.0", "100.127.255.255"}},
		{name: "multicast", addrs: []string{"224.0.0.0", "239.255.255.255", "ff00::", "ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff"}},
		{name: "240.0.0.0/4", addrs: []string{"240.0.0.0", "255.255.255.255"}},
		{name: "TEST-NET-1", addrs: []string{"192.0.2.0", "192.0.2.255"}},
		{name: "TEST-NET-2", addrs: []string{"198.51.100.0", "198.51.100.255"}},
		{name: "TEST-NET-3", addrs: []string{"203.0.113.0", "203.0.113.255"}},
		{name: "2001:db8::/32", addrs: []string{"2001:db8::", "2001:db8:ffff:ffff:ffff:ffff:ffff:ffff"}},
		{name: "198.18.0.0/15", addrs: []string{"198.18.0.0", "198.19.255.255"}},
		{name: "local-use NAT64 64:ff9b:1::/48", addrs: []string{"64:ff9b:1::", "64:ff9b:1::102:304", "64:ff9b:1:ffff:ffff:ffff:ffff:ffff"}},
		{name: "192.0.0.0/24", addrs: []string{"192.0.0.0", "192.0.0.9", "192.0.0.170", "192.0.0.255"}},
		{name: "192.88.99.0/24", addrs: []string{"192.88.99.0", "192.88.99.2", "192.88.99.255"}},
		{name: "IPv4-compatible ::/96", addrs: []string{"::2", "::102:304", "::ffff:ffff"}},
		{name: "IPv4-translated ::ffff:0:0:0/96", addrs: []string{"::ffff:0:0:0", "::ffff:0:102:304", "::ffff:0:ffff:ffff"}},
		{name: "discard-only 100::/64", addrs: []string{"100::", "100::ffff:ffff:ffff:ffff"}},
		{name: "dummy 100:0:0:1::/64", addrs: []string{"100:0:0:1::", "100::1:ffff:ffff:ffff:ffff"}},
		{name: "2001::/23", addrs: []string{"2001::", "2001:1::1", "2001:3::1", "2001:20::1", "2001:1ff:ffff:ffff:ffff:ffff:ffff:ffff"}},
		{name: "Teredo 2001::/32", addrs: []string{"2001::1", "2001:0:ffff:ffff:ffff:ffff:ffff:ffff"}},
		{name: "benchmarking 2001:2::/48", addrs: []string{"2001:2::", "2001:2:0:ffff:ffff:ffff:ffff:ffff"}},
		{name: "3fff::/20", addrs: []string{"3fff::", "3fff:fff:ffff:ffff:ffff:ffff:ffff:ffff"}},
		{name: "SRv6 SIDs 5f00::/16", addrs: []string{"5f00::", "5f00:ffff:ffff:ffff:ffff:ffff:ffff:ffff"}},
		{name: "site-local fec0::/10", addrs: []string{"fec0::", "feff:ffff:ffff:ffff:ffff:ffff:ffff:ffff"}},
	}
}

// allowedAddrs are addresses outside every refused range, many just
// outside one.
func allowedAddrs() []string {
	return []string{
		"1.0.0.0", "9.255.255.255", "11.0.0.0", "100.63.255.255", "100.128.0.0",
		"126.255.255.255", "128.0.0.0", "169.253.255.255", "169.255.0.0", "172.15.255.255",
		"172.32.0.0", "191.255.255.255", "192.0.1.0", "192.0.1.255", "192.0.3.0",
		"192.88.98.255", "192.88.100.0", "192.167.255.255", "192.169.0.0",
		"198.17.255.255", "198.20.0.0", "198.51.99.255", "198.51.101.0", "203.0.112.255",
		"203.0.114.0", "223.255.255.255",
		"2001:db7:ffff:ffff:ffff:ffff:ffff:ffff", "2001:db9::", "2001:200::1", "2a00:1450:4001::1", "2606:4700::1111",
		"3ffe:ffff:ffff:ffff:ffff:ffff:ffff:ffff", "3fff:1000::", "5eff:ffff:ffff:ffff:ffff:ffff:ffff:ffff", "5f01::",
		"::1:0:0", "::fffe:ffff:ffff:ffff", "::ffff:1:0:0", "100:0:0:2::",
	}
}

func TestGuardedClientRefuses(t *testing.T) {
	t.Parallel()

	opts := httpx.DefaultGuardedOptions()
	for _, r := range refusedRanges() {
		t.Run(r.name, func(t *testing.T) {
			t.Parallel()

			for _, s := range r.addrs {
				for _, a := range spellings(netip.MustParseAddr(s)) {
					n := &fakeNet{names: map[string][]netip.Addr{"refused.example": {a}}}
					c := guarded(t, opts, n)
					if _, _, err := fetch(t.Context(), t, c, "http://refused.example/"); !errors.Is(err, httpx.ErrAddressRefused) {
						t.Errorf("a name for %v: %v, want an error wrapping ErrAddressRefused", a, err)
					}
					if _, _, err := fetch(t.Context(), t, c, urlOf(a)); !errors.Is(err, httpx.ErrAddressRefused) {
						t.Errorf("%s: %v, want an error wrapping ErrAddressRefused", urlOf(a), err)
					}
					if lookups, dialled := n.seen(); len(dialled) != 0 || !slices.Equal(lookups, []string{"refused.example"}) {
						t.Errorf("for %v: looked up %q and connected to %v, want refused.example alone and no connection", a, lookups, dialled)
					}
				}
			}
		})
	}

	t.Run("allowed", func(t *testing.T) {
		t.Parallel()

		for _, s := range allowedAddrs() {
			for _, a := range spellings(netip.MustParseAddr(s)) {
				n := &fakeNet{names: map[string][]netip.Addr{"public.example": {a}}}
				c := guarded(t, opts, n)
				for _, url := range []string{"http://public.example/", urlOf(a)} {
					_, _, err := fetch(t.Context(), t, c, url)
					if errors.Is(err, httpx.ErrAddressRefused) || !errors.Is(err, errNoConnection) {
						t.Errorf("%s for %v: %v, want it connected to", url, a, err)
					}
				}
				want := netip.AddrPortFrom(a.Unmap(), 80)
				if _, dialled := n.seen(); !slices.Equal(dialled, []netip.AddrPort{want, want}) {
					t.Errorf("for %v: connected to %v, want %v twice", a, dialled, want)
				}
			}
		}
	})

	t.Run("a name's refused addresses are skipped", func(t *testing.T) {
		t.Parallel()

		mixed := []netip.Addr{netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("::1"), netip.MustParseAddr("1.2.3.4")}
		n := &fakeNet{names: map[string][]netip.Addr{"mixed.example": mixed}}
		c := guarded(t, opts, n)
		if _, _, err := fetch(t.Context(), t, c, "http://mixed.example:8080/"); !errors.Is(err, errNoConnection) {
			t.Errorf("mixed.example: %v, want it connected to", err)
		}
		if _, dialled := n.seen(); !slices.Equal(dialled, []netip.AddrPort{netip.MustParseAddrPort("1.2.3.4:8080")}) {
			t.Errorf("connected to %v, want 1.2.3.4:8080 alone", dialled)
		}
	})

	t.Run("a name with no address", func(t *testing.T) {
		t.Parallel()

		n := &fakeNet{names: map[string][]netip.Addr{"empty.example": {}}}
		c := guarded(t, opts, n)
		for _, url := range []string{"http://empty.example/", "http://missing.example/"} {
			if _, _, err := fetch(t.Context(), t, c, url); err == nil || errors.Is(err, httpx.ErrAddressRefused) {
				t.Errorf("%s: %v, want an error that is not ErrAddressRefused", url, err)
			}
		}
		if _, dialled := n.seen(); len(dialled) != 0 {
			t.Errorf("connected to %v, want nothing", dialled)
		}
	})

	t.Run("a zone", func(t *testing.T) {
		t.Parallel()

		n := &fakeNet{}
		c := guarded(t, opts, n)
		if _, _, err := fetch(t.Context(), t, c, "http://[fe80::1%25en0]/"); !errors.Is(err, httpx.ErrAddressRefused) {
			t.Errorf("a link-local address with a zone: %v, want an error wrapping ErrAddressRefused", err)
		}
	})

	t.Run("a refused literal never reaches the system's resolver or dialler", func(t *testing.T) {
		t.Parallel()

		// Pisati's own Lookup and Connect, with literals only: each is
		// refused before anything is looked up or connected to.
		c, err := httpx.NewGuardedClient(opts)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(c.CloseIdleConnections)
		for _, url := range []string{"http://127.0.0.1:1/", "http://[::1]:1/", "http://[::ffff:127.0.0.1]:1/", "http://0.0.0.0:1/"} {
			if _, _, err := fetch(t.Context(), t, c, url); !errors.Is(err, httpx.ErrAddressRefused) {
				t.Errorf("%s: %v, want an error wrapping ErrAddressRefused", url, err)
			}
		}
	})
}

// refusedV4 reports whether the GuardedClient should refuse a, an IPv4
// address, worked out from net/netip's own classes where they cover a
// range, and otherwise from the range itself.
func refusedV4(a netip.Addr) bool {
	if a.IsUnspecified() || a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsMulticast() {
		return true
	}
	for _, p := range []string{
		"0.0.0.0/8", "100.64.0.0/10", "240.0.0.0/4", "192.0.0.0/24", "192.0.2.0/24",
		"192.88.99.0/24", "198.51.100.0/24", "203.0.113.0/24", "198.18.0.0/15",
	} {
		if netip.MustParsePrefix(p).Contains(a) {
			return true
		}
	}
	return false
}

func FuzzAddressSpellings(f *testing.F) {
	for _, s := range []string{"10.0.0.1", "127.0.0.1", "1.2.3.4", "100.64.0.0", "255.255.255.255", "198.19.0.1", "8.8.8.8"} {
		b := netip.MustParseAddr(s).As4()
		f.Add(b[0], b[1], b[2], b[3])
	}
	f.Fuzz(func(t *testing.T, b0, b1, b2, b3 byte) {
		v4 := netip.AddrFrom4([4]byte{b0, b1, b2, b3})
		want := refusedV4(v4)
		c := guarded(t, httpx.DefaultGuardedOptions(), &fakeNet{})
		for _, a := range spellings(v4) {
			_, _, err := fetch(t.Context(), t, c, urlOf(a))
			switch got := errors.Is(err, httpx.ErrAddressRefused); {
			case !got && !errors.Is(err, errNoConnection):
				t.Fatalf("%v: %v, want it refused or connected to", a, err)
			case got != want:
				t.Fatalf("%v, a spelling of %v, refused: %t, want %t", a, v4, got, want)
			}
		}
	})
}

func TestNewGuardedClient(t *testing.T) {
	t.Parallel()

	t.Run("defaults", func(t *testing.T) {
		t.Parallel()

		got := httpx.DefaultGuardedOptions()
		if got.Timeout != 10*time.Second || got.MaxBody != 1<<20 || got.MaxRedirects != 3 || got.Lookup != nil || got.Connect != nil {
			t.Errorf("DefaultGuardedOptions() = %+v, want a 10 s timeout, 1 MiB of body, 3 redirects and no test hooks", got)
		}
		if _, err := httpx.NewGuardedClient(got); err != nil {
			t.Errorf("NewGuardedClient(DefaultGuardedOptions()): %v", err)
		}
	})

	for _, tt := range []struct {
		name  string
		o     httpx.GuardedOptions
		names []string
	}{
		{name: "zero", o: httpx.GuardedOptions{}, names: []string{"Timeout", "MaxBody"}},
		{name: "negative", o: httpx.GuardedOptions{Timeout: -1, MaxBody: -1, MaxRedirects: -1}, names: []string{"Timeout", "MaxBody", "MaxRedirects"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c, err := httpx.NewGuardedClient(tt.o)
			if !errors.Is(err, httpx.ErrInvalid) || c != nil {
				t.Fatalf("NewGuardedClient(%+v) = %v, %v; want nil and an error wrapping ErrInvalid", tt.o, c, err)
			}
			for _, name := range tt.names {
				if !strings.Contains(err.Error(), name) {
					t.Errorf("NewGuardedClient(%+v)'s error %q does not name %s", tt.o, err, name)
				}
			}
		})
	}

	t.Run("no redirects is a limit", func(t *testing.T) {
		t.Parallel()

		o := httpx.DefaultGuardedOptions()
		o.MaxRedirects = 0
		if _, err := httpx.NewGuardedClient(o); err != nil {
			t.Errorf("NewGuardedClient with MaxRedirects 0: %v", err)
		}
	})

	t.Run("the zero GuardedClient sends nothing", func(t *testing.T) {
		t.Parallel()

		var c httpx.GuardedClient
		req := getRequest(t.Context(), t, "http://public.example/")
		resp, err := c.Do(t.Context(), req)
		if err == nil {
			if cerr := resp.Body.Close(); cerr != nil {
				t.Error(cerr)
			}
			t.Error("the zero GuardedClient's Do sent a request, want an error")
		}
		c.CloseIdleConnections()
	})

	t.Run("a nil request", func(t *testing.T) {
		t.Parallel()

		c := guarded(t, httpx.DefaultGuardedOptions(), &fakeNet{})
		resp, err := c.Do(t.Context(), nil)
		if err == nil {
			if cerr := resp.Body.Close(); cerr != nil {
				t.Error(cerr)
			}
			t.Error("Do(nil) sent a request, want an error")
		}
	})
}

// origin serves what TestGuardedClientLimits asks for:
//
//   - /bytes/{n}: n bytes, with their Content-Length;
//   - /chunked/{n}: n bytes, chunked;
//   - /gzip/{n}: n bytes, gzipped, to a client that accepts gzip, and
//     406 to one that does not;
//   - /header/{n}: a header of n bytes;
//   - /hop/{n}: a redirect to /hop/{n-1}, or "done" when n is 0;
//   - /to/{i}: a redirect to redirects[i].
func origin(t *testing.T, redirects ...string) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	size := func(w http.ResponseWriter, r *http.Request) (int, bool) {
		n, err := strconv.Atoi(r.PathValue("n"))
		if err != nil || n < 0 {
			http.Error(w, "bad size", http.StatusBadRequest)
			return 0, false
		}
		return n, true
	}
	write := func(w http.ResponseWriter, s string) {
		if _, err := io.WriteString(w, s); err != nil {
			t.Errorf("writing the answer: %v", err)
		}
	}
	mux.HandleFunc("GET /bytes/{n}", func(w http.ResponseWriter, r *http.Request) {
		if n, ok := size(w, r); ok {
			w.Header().Set("Content-Length", strconv.Itoa(n))
			write(w, strings.Repeat("x", n))
		}
	})
	mux.HandleFunc("GET /chunked/{n}", func(w http.ResponseWriter, r *http.Request) {
		if n, ok := size(w, r); ok {
			write(w, strings.Repeat("x", n/2))
			if err := http.NewResponseController(w).Flush(); err != nil {
				t.Errorf("flushing: %v", err)
			}
			write(w, strings.Repeat("x", n-n/2))
		}
	})
	mux.HandleFunc("GET /gzip/{n}", func(w http.ResponseWriter, r *http.Request) {
		n, ok := size(w, r)
		if !ok {
			return
		}
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			http.Error(w, "gzip only", http.StatusNotAcceptable)
			return
		}
		w.Header().Set("Content-Encoding", "gzip")
		zw := gzip.NewWriter(w)
		if _, err := io.WriteString(zw, strings.Repeat("x", n)); err != nil {
			t.Errorf("writing gzip: %v", err)
		}
		if err := zw.Close(); err != nil {
			t.Errorf("closing gzip: %v", err)
		}
	})
	mux.HandleFunc("GET /header/{n}", func(w http.ResponseWriter, r *http.Request) {
		if n, ok := size(w, r); ok {
			w.Header().Set("X-Fill", strings.Repeat("x", n))
			write(w, "done")
		}
	})
	mux.HandleFunc("GET /hop/{n}", func(w http.ResponseWriter, r *http.Request) {
		if n, ok := size(w, r); ok {
			if n == 0 {
				write(w, "done")
				return
			}
			http.Redirect(w, r, "/hop/"+strconv.Itoa(n-1), http.StatusFound)
		}
	})
	for i, to := range redirects {
		mux.HandleFunc("GET /to/"+strconv.Itoa(i), func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, to, http.StatusFound)
		})
	}
	return mux
}

// public holds the names TestGuardedClientLimits reaches: two public
// hosts, and two that resolve to refused addresses.
func public() map[string][]netip.Addr {
	return map[string][]netip.Addr{
		"public.example":  {netip.MustParseAddr("1.2.3.4")},
		"other.example":   {netip.MustParseAddr("2a00:1450:4001::1")},
		"private.example": {netip.MustParseAddr("192.168.1.1")},
		"mapped.example":  {netip.MustParseAddr("::ffff:127.0.0.1")},
	}
}

// toLoopback returns a connect that reaches srv over loopback, whatever
// address it is given.
func toLoopback(srv *httptest.Server) func(context.Context, netip.AddrPort) (net.Conn, error) {
	return func(ctx context.Context, _ netip.AddrPort) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", srv.Listener.Addr().String())
	}
}

// connect returns the client's end of a new connection to l, once the
// server has accepted it.
func (l *pipeListener) connect(ctx context.Context, _ netip.AddrPort) (net.Conn, error) {
	client, server := net.Pipe()
	select {
	case l.conns <- server:
		return client, nil
	case <-l.closed:
		return nil, net.ErrClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestGuardedClientLimits(t *testing.T) {
	t.Parallel()

	const limit = 1024
	opts := httpx.GuardedOptions{Timeout: 10 * time.Second, MaxBody: limit, MaxRedirects: 3}
	// over serves origin, redirecting /to/{i} to redirects[i], over
	// loopback, which the client reaches by any name public resolves.
	over := func(t *testing.T, o httpx.GuardedOptions, redirects ...string) (*httpx.GuardedClient, *fakeNet) {
		t.Helper()
		srv := serve(t, origin(t, redirects...))
		n := &fakeNet{names: public(), connect: toLoopback(srv)}
		return guarded(t, o, n), n
	}

	t.Run("only http and https", func(t *testing.T) {
		t.Parallel()

		c, n := over(t, opts)
		for _, url := range []string{"ftp://public.example/", "file:///etc/hostname", "gopher://public.example/", "ws://public.example/"} {
			if _, _, err := fetch(t.Context(), t, c, url); !errors.Is(err, httpx.ErrAddressRefused) {
				t.Errorf("%s: %v, want an error wrapping ErrAddressRefused", url, err)
			}
		}
		if lookups, dialled := n.seen(); len(lookups) != 0 || len(dialled) != 0 {
			t.Errorf("looked up %q and connected to %v, want nothing", lookups, dialled)
		}
		if status, body, err := fetch(t.Context(), t, c, "HTTP://public.example/hop/0"); err != nil || status != http.StatusOK || body != "done" {
			t.Errorf("HTTP://public.example/hop/0 = %d %q, %v; want 200 done", status, body, err)
		}
	})

	t.Run("the body is held to MaxBody", func(t *testing.T) {
		t.Parallel()

		c, _ := over(t, opts)
		for _, kind := range []string{"bytes", "chunked"} {
			for _, tt := range []struct {
				size int
				ok   bool
			}{{0, true}, {limit - 1, true}, {limit, true}, {limit + 1, false}, {4 * limit, false}} {
				url := "http://public.example/" + kind + "/" + strconv.Itoa(tt.size)
				status, body, err := fetch(t.Context(), t, c, url)
				switch {
				case status != http.StatusOK:
					t.Errorf("%s = %d, %v; want 200", url, status, err)
				case tt.ok && (err != nil || len(body) != tt.size):
					t.Errorf("%s read %d bytes, %v; want all %d", url, len(body), err, tt.size)
				case !tt.ok && (!errors.Is(err, httpx.ErrTooLarge) || len(body) != limit):
					t.Errorf("%s read %d bytes, %v; want %d and an error wrapping ErrTooLarge", url, len(body), err, limit)
				}
			}
		}
	})

	t.Run("a gzipped body is held to MaxBody once decoded", func(t *testing.T) {
		t.Parallel()

		c, _ := over(t, opts)
		for _, tt := range []struct {
			size int
			ok   bool
		}{{limit, true}, {limit + 1, false}, {1 << 20, false}} {
			url := "http://public.example/gzip/" + strconv.Itoa(tt.size)
			status, body, err := fetch(t.Context(), t, c, url)
			switch {
			case status != http.StatusOK:
				t.Errorf("%s = %d, %v; want 200, gzipped", url, status, err)
			case tt.ok && (err != nil || len(body) != tt.size):
				t.Errorf("%s read %d bytes, %v; want all %d", url, len(body), err, tt.size)
			case !tt.ok && (!errors.Is(err, httpx.ErrTooLarge) || len(body) != limit):
				t.Errorf("%s read %d bytes, %v; want %d and an error wrapping ErrTooLarge", url, len(body), err, limit)
			}
		}
	})

	t.Run("a caller's Accept-Encoding is refused", func(t *testing.T) {
		t.Parallel()

		c, n := over(t, opts)
		for _, value := range []string{"gzip", "identity", "br, gzip"} {
			req := getRequest(t.Context(), t, "http://public.example/gzip/16")
			req.Header.Set("Accept-Encoding", value)
			resp, err := c.Do(t.Context(), req)
			if err == nil {
				if cerr := resp.Body.Close(); cerr != nil {
					t.Error(cerr)
				}
			}
			if !errors.Is(err, httpx.ErrInvalid) {
				t.Errorf("Accept-Encoding: %s: %v, want an error wrapping ErrInvalid", value, err)
			}
		}
		if lookups, dialled := n.seen(); len(lookups) != 0 || len(dialled) != 0 {
			t.Errorf("looked up %q and connected to %v, want nothing", lookups, dialled)
		}
	})

	t.Run("a MaxBody as large as an int64", func(t *testing.T) {
		t.Parallel()

		o := opts
		o.MaxBody = math.MaxInt64
		c, _ := over(t, o)
		for _, kind := range []string{"bytes", "chunked"} {
			url := "http://public.example/" + kind + "/100"
			if status, body, err := fetch(t.Context(), t, c, url); err != nil || status != http.StatusOK || len(body) != 100 {
				t.Errorf("%s = %d, %d bytes, %v; want 200 and 100 bytes", url, status, len(body), err)
			}
		}
	})

	t.Run("a response header is held to 64 KiB", func(t *testing.T) {
		t.Parallel()

		c, _ := over(t, opts)
		url := "http://public.example/header/" + strconv.Itoa(60<<10)
		if status, body, err := fetch(t.Context(), t, c, url); err != nil || status != http.StatusOK || body != "done" {
			t.Errorf("a 60 KiB header: %d %q, %v; want 200 done", status, body, err)
		}
		url = "http://public.example/header/" + strconv.Itoa(64<<10)
		if _, _, err := fetch(t.Context(), t, c, url); err == nil || !strings.Contains(err.Error(), "exceeded 65536 bytes") {
			t.Errorf("a 64 KiB header: %v, want an error that the headers exceeded 65536 bytes", err)
		}
	})

	t.Run("reading past the limit keeps failing", func(t *testing.T) {
		t.Parallel()

		// One byte over: the body's own reader is at its end once the
		// limit is passed, and must not be what a later Read reports.
		c, _ := over(t, opts)
		req := getRequest(t.Context(), t, "http://public.example/bytes/"+strconv.Itoa(limit+1))
		resp, err := c.Do(t.Context(), req)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadAll(resp.Body); !errors.Is(err, httpx.ErrTooLarge) {
			t.Errorf("ReadAll: %v, want an error wrapping ErrTooLarge", err)
		}
		if n, err := resp.Body.Read(make([]byte, 16)); n != 0 || !errors.Is(err, httpx.ErrTooLarge) {
			t.Errorf("Read after the limit = %d, %v; want 0 and an error wrapping ErrTooLarge", n, err)
		}
		if err := resp.Body.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	t.Run("a body closed unread", func(t *testing.T) {
		t.Parallel()

		c, _ := over(t, opts)
		req := getRequest(t.Context(), t, "http://public.example/bytes/2048")
		resp, err := c.Do(t.Context(), req)
		if err != nil {
			t.Fatal(err)
		}
		if err := resp.Body.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	t.Run("MaxRedirects", func(t *testing.T) {
		t.Parallel()

		for _, tt := range []struct {
			max, hops int
			ok        bool
		}{{3, 0, true}, {3, 3, true}, {3, 4, false}, {0, 0, true}, {0, 1, false}, {1, 1, true}, {1, 2, false}} {
			o := opts
			o.MaxRedirects = tt.max
			c, _ := over(t, o)
			url := "http://public.example/hop/" + strconv.Itoa(tt.hops)
			status, body, err := fetch(t.Context(), t, c, url)
			if tt.ok && (err != nil || status != http.StatusOK || body != "done") {
				t.Errorf("%s with MaxRedirects %d = %d %q, %v; want 200 done", url, tt.max, status, body, err)
			}
			if !tt.ok && !errors.Is(err, httpx.ErrTooManyRedirects) {
				t.Errorf("%s with MaxRedirects %d: %v, want an error wrapping ErrTooManyRedirects", url, tt.max, err)
			}
		}
	})

	t.Run("every redirect is checked", func(t *testing.T) {
		t.Parallel()

		for _, to := range []string{
			"http://10.0.0.1/", "http://[::1]:8080/", "http://private.example/", "http://mapped.example/",
			"ftp://public.example/", "file:///etc/hostname",
		} {
			c, n := over(t, opts, to)
			url := "http://public.example/to/0"
			if _, _, err := fetch(t.Context(), t, c, url); !errors.Is(err, httpx.ErrAddressRefused) {
				t.Errorf("a redirect to %s: %v, want an error wrapping ErrAddressRefused", to, err)
			}
			if _, dialled := n.seen(); !slices.Equal(dialled, []netip.AddrPort{netip.MustParseAddrPort("1.2.3.4:80")}) {
				t.Errorf("a redirect to %s connected to %v, want 1.2.3.4:80 alone", to, dialled)
			}
		}
	})

	t.Run("a redirect to a new host connects to its address", func(t *testing.T) {
		t.Parallel()

		c, n := over(t, opts, "http://other.example:8080/hop/0")
		url := "http://public.example/to/0"
		if status, body, err := fetch(t.Context(), t, c, url); err != nil || status != http.StatusOK || body != "done" {
			t.Errorf("%s = %d %q, %v; want 200 done", url, status, body, err)
		}
		want := []netip.AddrPort{netip.MustParseAddrPort("1.2.3.4:80"), netip.MustParseAddrPort("[2a00:1450:4001::1]:8080")}
		if _, dialled := n.seen(); !slices.Equal(dialled, want) {
			t.Errorf("connected to %v, want %v", dialled, want)
		}
	})

	t.Run("Do sends with its own context", func(t *testing.T) {
		t.Parallel()

		c, _ := over(t, opts)
		done, cancel := context.WithCancel(t.Context())
		cancel()
		req := getRequest(done, t, "http://public.example/hop/0")
		resp, err := c.Do(t.Context(), req)
		if err != nil {
			t.Fatalf("Do with a live context and a request whose own has ended: %v", err)
		}
		if err := resp.Body.Close(); err != nil {
			t.Error(err)
		}
		if _, _, err := fetch(done, t, c, "http://public.example/hop/0"); !errors.Is(err, context.Canceled) {
			t.Errorf("Do with an ended context: %v, want context.Canceled", err)
		}
	})

	t.Run("the timeout bounds the whole exchange", func(t *testing.T) {
		t.Parallel()

		for _, tt := range []struct {
			name string
			h    http.HandlerFunc
		}{
			{name: "no answer", h: func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }},
			{name: "a stalled body", h: func(w http.ResponseWriter, r *http.Request) {
				if _, err := io.WriteString(w, "partial"); err != nil {
					t.Errorf("writing: %v", err)
				}
				if err := http.NewResponseController(w).Flush(); err != nil {
					t.Errorf("flushing: %v", err)
				}
				<-r.Context().Done()
			}},
		} {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				synctest.Test(t, func(t *testing.T) {
					ln := servePipe(t, &http.Server{Handler: tt.h, ReadHeaderTimeout: time.Minute})
					c := guarded(t, opts, &fakeNet{names: public(), connect: ln.connect})
					start := time.Now()
					_, _, err := fetch(t.Context(), t, c, "http://public.example/")
					if elapsed := time.Since(start); err == nil || elapsed != opts.Timeout {
						t.Errorf("after %v: %v; want an error at %v", elapsed, err, opts.Timeout)
					}
				})
			})
		}
	})

	t.Run("an address that never connects leaves time for the next", func(t *testing.T) {
		t.Parallel()

		synctest.Test(t, func(t *testing.T) {
			ln := servePipe(t, &http.Server{Handler: origin(t), ReadHeaderTimeout: time.Minute})
			stuck := netip.MustParseAddr("2a00:1450:4001::1")
			n := &fakeNet{
				names: map[string][]netip.Addr{"public.example": {stuck, netip.MustParseAddr("1.2.3.4")}},
				connect: func(ctx context.Context, to netip.AddrPort) (net.Conn, error) {
					if to.Addr() == stuck {
						<-ctx.Done()
						return nil, ctx.Err()
					}
					return ln.connect(ctx, to)
				},
			}
			c := guarded(t, opts, n)
			start := time.Now()
			status, body, err := fetch(t.Context(), t, c, "http://public.example/hop/0")
			if err != nil || status != http.StatusOK || body != "done" {
				t.Errorf("= %d %q, %v; want 200 done", status, body, err)
			}
			// Two addresses share the timeout.
			if elapsed := time.Since(start); elapsed != opts.Timeout/2 {
				t.Errorf("answered after %v, want %v", elapsed, opts.Timeout/2)
			}
		})
	})
}

// TestGuardedClientIgnoresProxyEnvironment cannot run in parallel, as it
// sets the environment. net/http reads the proxy variables once a
// process, the first time a transport asks for them, and as a test that
// is not parallel, this runs before any test that is.
func TestGuardedClientIgnoresProxyEnvironment(t *testing.T) {
	for _, name := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy"} {
		t.Setenv(name, "http://proxy.example:3128")
	}
	n := &fakeNet{names: map[string][]netip.Addr{
		"public.example": {netip.MustParseAddr("1.2.3.4")},
		"proxy.example":  {netip.MustParseAddr("1.2.3.9")},
	}}
	c := guarded(t, httpx.DefaultGuardedOptions(), n)
	for _, url := range []string{"http://public.example/", "https://public.example/"} {
		if _, _, err := fetch(t.Context(), t, c, url); !errors.Is(err, errNoConnection) {
			t.Errorf("%s: %v, want it connected to", url, err)
		}
	}
	lookups, dialled := n.seen()
	want := []netip.AddrPort{netip.MustParseAddrPort("1.2.3.4:80"), netip.MustParseAddrPort("1.2.3.4:443")}
	if !slices.Equal(lookups, []string{"public.example", "public.example"}) || !slices.Equal(dialled, want) {
		t.Errorf("looked up %q and connected to %v, want public.example and %v, never the proxy", lookups, dialled, want)
	}
}
