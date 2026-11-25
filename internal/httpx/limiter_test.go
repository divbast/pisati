// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package httpx_test

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/divbast/pisati/internal/httpx"
)

func TestLimiterPresets(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name      string
		got, want httpx.LimiterOptions
	}{
		{
			name: "PageLimiterOptions", got: httpx.PageLimiterOptions(),
			want: httpx.LimiterOptions{Rate: 10, Burst: 40, MaxEntries: 65536, Idle: 10 * time.Minute},
		},
		{
			name: "CSPReportLimiterOptions", got: httpx.CSPReportLimiterOptions(),
			want: httpx.LimiterOptions{Rate: 1, Burst: 5, MaxEntries: 65536, Idle: 10 * time.Minute},
		},
	} {
		if tt.got != tt.want {
			t.Errorf("%s() = %+v, want %+v", tt.name, tt.got, tt.want)
		}
		if _, err := httpx.NewLimiter(tt.got, time.Now); err != nil {
			t.Errorf("NewLimiter(%s()): %v", tt.name, err)
		}
	}
}

func TestNewLimiter(t *testing.T) {
	t.Parallel()

	good := httpx.LimiterOptions{Rate: 1, Burst: 5, MaxEntries: 8, Idle: 5 * time.Second}
	with := func(change func(*httpx.LimiterOptions)) httpx.LimiterOptions {
		o := good
		change(&o)
		return o
	}
	tests := []struct {
		name string
		o    httpx.LimiterOptions
		// names are what the error must name, one for each problem.
		names []string
	}{
		{name: "zero", o: httpx.LimiterOptions{}, names: []string{"Rate", "Burst", "MaxEntries", "Idle"}},
		{name: "zero rate", o: with(func(o *httpx.LimiterOptions) { o.Rate = 0 }), names: []string{"Rate"}},
		{name: "negative rate", o: with(func(o *httpx.LimiterOptions) { o.Rate = -1 }), names: []string{"Rate"}},
		{name: "NaN rate", o: with(func(o *httpx.LimiterOptions) { o.Rate = httpx.Rate(math.NaN()) }), names: []string{"Rate"}},
		{name: "infinite rate", o: with(func(o *httpx.LimiterOptions) { o.Rate = httpx.Rate(math.Inf(1)) }), names: []string{"Rate"}},
		{name: "a rate over one a nanosecond", o: with(func(o *httpx.LimiterOptions) { o.Rate = 2e9 }), names: []string{"Rate"}},
		{
			name: "a rate a /48's bucket cannot keep",
			o:    with(func(o *httpx.LimiterOptions) { o.Rate = 2e8 }), names: []string{"Rate"},
		},
		{
			// A /64's token each 12.5 ns rounds to 13, so its bucket fills
			// in 13 ns; a /48's each 1.5625 ns rounds to 2, and its 8
			// tokens take 16 ns.
			name: "idle as long as a /64's bucket takes to fill, but not a /48's",
			o:    httpx.LimiterOptions{Rate: 8e7, Burst: 1, MaxEntries: 8, Idle: 13}, names: []string{"Idle"},
		},
		{
			name: "a rate whose interval a Duration cannot hold",
			o: httpx.LimiterOptions{
				Rate: httpx.Rate(math.Ldexp(1953125, -54)), Burst: 1, MaxEntries: 8, Idle: math.MaxInt64,
			},
			names: []string{"Idle"},
		},
		{name: "zero burst", o: with(func(o *httpx.LimiterOptions) { o.Burst = 0 }), names: []string{"Burst"}},
		{name: "negative burst", o: with(func(o *httpx.LimiterOptions) { o.Burst = -1 }), names: []string{"Burst"}},
		{name: "zero entries", o: with(func(o *httpx.LimiterOptions) { o.MaxEntries = 0 }), names: []string{"MaxEntries"}},
		{name: "no room for an IPv6 client's two buckets", o: with(func(o *httpx.LimiterOptions) { o.MaxEntries = 1 }), names: []string{"MaxEntries"}},
		{name: "zero idle", o: with(func(o *httpx.LimiterOptions) { o.Idle = 0 }), names: []string{"Idle"}},
		{name: "negative idle", o: with(func(o *httpx.LimiterOptions) { o.Idle = -time.Second }), names: []string{"Idle"}},
		{
			name: "idle shorter than a bucket takes to fill",
			o:    with(func(o *httpx.LimiterOptions) { o.Idle = 5*time.Second - time.Nanosecond }), names: []string{"Idle"},
		},
		{
			name: "a slow rate",
			o:    with(func(o *httpx.LimiterOptions) { o.Rate = 1e-300 }), names: []string{"Idle"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			l, err := httpx.NewLimiter(tt.o, time.Now)
			if !errors.Is(err, httpx.ErrInvalid) || l != nil {
				t.Fatalf("NewLimiter(%+v) = %v, %v; want nil and an error wrapping ErrInvalid", tt.o, l, err)
			}
			for _, name := range tt.names {
				if !strings.Contains(err.Error(), name) {
					t.Errorf("NewLimiter(%+v)'s error %q does not name %s", tt.o, err, name)
				}
			}
		})
	}

	t.Run("a nil clock", func(t *testing.T) {
		t.Parallel()

		if l, err := httpx.NewLimiter(good, nil); !errors.Is(err, httpx.ErrInvalid) || l != nil {
			t.Errorf("NewLimiter(good, nil) = %v, %v; want nil and an error wrapping ErrInvalid", l, err)
		}
	})

	t.Run("idle exactly as long as a bucket takes to fill", func(t *testing.T) {
		t.Parallel()

		if _, err := httpx.NewLimiter(good, time.Now); err != nil {
			t.Errorf("NewLimiter(%+v): %v", good, err)
		}
	})

	t.Run("the zero Limiter is a bug", func(t *testing.T) {
		t.Parallel()

		defer func() {
			if recover() == nil {
				t.Error("Middleware on the zero Limiter did not panic")
			}
		}()
		var l httpx.Limiter
		l.Middleware(httpx.NewClientIP(nil))
	})

	t.Run("a nil ClientIP is a bug", func(t *testing.T) {
		t.Parallel()

		l, err := httpx.NewLimiter(good, time.Now)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if recover() == nil {
				t.Error("Middleware(nil) did not panic")
			}
		}()
		l.Middleware(nil)
	})
}

// limited serves requests through l, each from the address it is given,
// to a handler that answers 200 "ok" and counts the requests it sees.
type limited struct {
	t       *testing.T
	h       http.Handler
	reached atomic.Int64
}

// newLimited returns a limited for l, whose clients are their peers, with
// no proxy trusted.
func newLimited(t *testing.T, l *httpx.Limiter) *limited {
	t.Helper()
	return newLimitedBy(t, l, httpx.NewClientIP(nil))
}

// newLimitedBy returns a limited for l, whose clients ip finds.
func newLimitedBy(t *testing.T, l *httpx.Limiter, ip httpx.ClientIP) *limited {
	t.Helper()
	lt := &limited{t: t}
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		lt.reached.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	lt.h = l.Middleware(ip)(ok)
	return lt
}

// from sends one request from remote, and returns the answer.
func (lt *limited) from(remote string) *httptest.ResponseRecorder {
	r := httptest.NewRequestWithContext(lt.t.Context(), http.MethodGet, "/", nil)
	r.RemoteAddr = remote
	w := httptest.NewRecorder()
	lt.h.ServeHTTP(w, r)
	return w
}

// burst sends n requests from remote, and fails the test unless each is
// answered 200.
func (lt *limited) burst(remote string, n int) {
	lt.t.Helper()
	for i := range n {
		if w := lt.from(remote); w.Code != http.StatusOK {
			lt.t.Fatalf("request %d of %d from %s = %d, want 200", i+1, n, remote, w.Code)
		}
	}
}

// refused fails the test unless the next request from remote is answered
// 429, with a Retry-After of retry seconds and a generic body.
func (lt *limited) refused(remote, retry string) {
	lt.t.Helper()
	before := lt.reached.Load()
	w := lt.from(remote)
	if w.Code != http.StatusTooManyRequests {
		lt.t.Fatalf("a request from %s = %d, want 429", remote, w.Code)
	}
	if got := w.Header().Get("Retry-After"); got != retry {
		lt.t.Errorf("429 from %s has Retry-After %q, want %q", remote, got, retry)
	}
	if body := w.Body.String(); body != "Too Many Requests\n" {
		lt.t.Errorf("429 body = %q, want a generic one", body)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		lt.t.Errorf("429 Content-Type = %q, want text/plain; charset=utf-8", ct)
	}
	if lt.reached.Load() != before {
		lt.t.Error("a refused request reached the handler")
	}
}

func TestLimiter(t *testing.T) {
	t.Parallel()

	pages := httpx.LimiterOptions{Rate: 10, Burst: 40, MaxEntries: 64, Idle: time.Minute}
	// inBubble runs f in a synctest bubble with a Limiter of o on the
	// bubble's clock.
	inBubble := func(t *testing.T, o httpx.LimiterOptions, f func(lt *limited)) {
		t.Helper()
		synctest.Test(t, func(t *testing.T) {
			l, err := httpx.NewLimiter(o, time.Now)
			if err != nil {
				t.Fatal(err)
			}
			f(newLimited(t, l))
		})
	}

	t.Run("a burst, then a token each interval", func(t *testing.T) {
		t.Parallel()

		inBubble(t, pages, func(lt *limited) {
			lt.burst("192.0.2.1:1", 40)
			lt.refused("192.0.2.1:1", "1")
			// A refused request takes no token.
			time.Sleep(100 * time.Millisecond)
			lt.burst("192.0.2.1:1", 1)
			lt.refused("192.0.2.1:1", "1")
			time.Sleep(time.Second)
			lt.burst("192.0.2.1:1", 10)
			lt.refused("192.0.2.1:1", "1")
		})
	})

	t.Run("a bucket fills no further than its burst", func(t *testing.T) {
		t.Parallel()

		inBubble(t, pages, func(lt *limited) {
			lt.burst("192.0.2.1:1", 40)
			time.Sleep(30 * time.Second)
			lt.burst("192.0.2.1:1", 40)
			lt.refused("192.0.2.1:1", "1")
		})
	})

	t.Run("Retry-After is whole seconds, rounded up", func(t *testing.T) {
		t.Parallel()

		// One token each 4 s.
		inBubble(t, httpx.LimiterOptions{Rate: 0.25, Burst: 1, MaxEntries: 8, Idle: time.Minute}, func(lt *limited) {
			lt.burst("192.0.2.1:1", 1)
			lt.refused("192.0.2.1:1", "4")
			time.Sleep(1500 * time.Millisecond)
			lt.refused("192.0.2.1:1", "3")
			time.Sleep(2499 * time.Millisecond)
			lt.refused("192.0.2.1:1", "1")
			time.Sleep(time.Millisecond)
			lt.burst("192.0.2.1:1", 1)
		})
	})

	t.Run("the CSP report profile", func(t *testing.T) {
		t.Parallel()

		inBubble(t, httpx.CSPReportLimiterOptions(), func(lt *limited) {
			lt.burst("192.0.2.1:1", 5)
			lt.refused("192.0.2.1:1", "1")
			time.Sleep(time.Second)
			lt.burst("192.0.2.1:1", 1)
			lt.refused("192.0.2.1:1", "1")
		})
	})

	t.Run("each client has a bucket of its own", func(t *testing.T) {
		t.Parallel()

		inBubble(t, pages, func(lt *limited) {
			lt.burst("192.0.2.1:1", 40)
			lt.refused("192.0.2.1:1", "1")
			lt.burst("192.0.2.2:1", 40)
			lt.burst("[2001:db8:0:1::1]:1", 40)
			lt.burst("[2001:db8:0:2::1]:1", 40)
		})
	})

	t.Run("an IPv6 /64 shares one bucket", func(t *testing.T) {
		t.Parallel()

		inBubble(t, pages, func(lt *limited) {
			lt.burst("[2001:db8:0:1::1]:1", 20)
			lt.burst("[2001:db8:0:1:ffff:ffff:ffff:ffff]:2", 20)
			lt.refused("[2001:db8:0:1:1234::5]:3", "1")
			lt.burst("[2001:db8:0:2::1]:1", 1)
		})
	})

	t.Run("an IPv4 address shares one bucket however it is spelt", func(t *testing.T) {
		t.Parallel()

		inBubble(t, pages, func(lt *limited) {
			lt.burst("192.0.2.1:1", 20)
			lt.burst("[::ffff:192.0.2.1]:2", 20)
			lt.refused("192.0.2.1:3", "1")
			lt.burst("192.0.2.0:1", 1)
		})
	})

	t.Run("requests with no address share one bucket", func(t *testing.T) {
		t.Parallel()

		inBubble(t, pages, func(lt *limited) {
			lt.burst("pipe", 20)
			lt.burst("", 20)
			lt.refused("pipe", "1")
			lt.burst("192.0.2.1:1", 1)
		})
	})

	t.Run("an IPv6 /48 shares a bucket eight times as large", func(t *testing.T) {
		t.Parallel()

		inBubble(t, pages, func(lt *limited) {
			for n := range 8 {
				lt.burst(fmt.Sprintf("[2001:db8:1:%x::1]:1", n), 40)
			}
			// A /64 of its own, but the /48's 320 are spent.
			lt.refused("[2001:db8:1:ff::1]:1", "1")
			// Another /48 has its own.
			lt.burst("[2001:db8:2:1::1]:1", 40)
			// The /48 gains a token each 12.5 ms.
			time.Sleep(12500 * time.Microsecond)
			lt.burst("[2001:db8:1:ff::1]:1", 1)
			lt.refused("[2001:db8:1:ff::1]:1", "1")
		})
	})

	t.Run("a /48's refusal takes nothing from the /64", func(t *testing.T) {
		t.Parallel()

		inBubble(t, pages, func(lt *limited) {
			for n := range 8 {
				lt.burst(fmt.Sprintf("[2001:db8:1:%x::1]:1", n), 40)
			}
			for range 40 {
				lt.refused("[2001:db8:1:ff::1]:1", "1")
			}
			// Enough for 40 more from the /48, and 5 from a /64 that the
			// refusals had emptied.
			time.Sleep(500 * time.Millisecond)
			lt.burst("[2001:db8:1:ff::1]:1", 40)
			lt.refused("[2001:db8:1:ff::1]:1", "1")
		})
	})

	t.Run("a /64's refusal takes nothing from the /48", func(t *testing.T) {
		t.Parallel()

		inBubble(t, pages, func(lt *limited) {
			lt.burst("[2001:db8:1::1]:1", 40)
			for range 280 {
				lt.refused("[2001:db8:1::1]:1", "1")
			}
			for n := 1; n <= 7; n++ {
				lt.burst(fmt.Sprintf("[2001:db8:1:%x::1]:1", n), 40)
			}
			lt.refused("[2001:db8:1:8::1]:1", "1")
		})
	})

	t.Run("Retry-After is the longer wait of the buckets that refuse", func(t *testing.T) {
		t.Parallel()

		// A /64 gains a token each 4 s, and its /48 one each 0.5 s.
		inBubble(t, httpx.LimiterOptions{Rate: 0.25, Burst: 1, MaxEntries: 64, Idle: time.Minute}, func(lt *limited) {
			for n := range 8 {
				lt.burst(fmt.Sprintf("[2001:db8:1:%x::1]:1", n), 1)
			}
			// Refused by the /48 alone.
			lt.refused("[2001:db8:1:ff::1]:1", "1")
			// Refused by both.
			lt.refused("[2001:db8:1:0::1]:1", "4")
		})
	})

	t.Run("a ClientIP that finds an invalid address is taken to find none", func(t *testing.T) {
		t.Parallel()

		synctest.Test(t, func(t *testing.T) {
			l, err := httpx.NewLimiter(pages, time.Now)
			if err != nil {
				t.Fatal(err)
			}
			invalid := newLimitedBy(t, l, func(*http.Request) (netip.Addr, bool) { return netip.Addr{}, true })
			none := newLimitedBy(t, l, func(*http.Request) (netip.Addr, bool) { return netip.Addr{}, false })
			// One bucket, charged once a request.
			invalid.burst("192.0.2.1:1", 20)
			none.burst("192.0.2.2:1", 20)
			invalid.refused("192.0.2.1:1", "1")
			none.refused("192.0.2.2:1", "1")
			if got := l.Len(); got != 1 {
				t.Errorf("Len() = %d, want 1: the bucket of requests with no address", got)
			}
		})
	})

	t.Run("a clock that goes back takes no token", func(t *testing.T) {
		t.Parallel()

		clock := newManualClock()
		l, err := httpx.NewLimiter(pages, clock.now)
		if err != nil {
			t.Fatal(err)
		}
		lt := newLimited(t, l)
		lt.burst("192.0.2.1:1", 39)
		clock.add(-time.Hour)
		lt.burst("192.0.2.1:1", 1)
		lt.refused("192.0.2.1:1", "1")
	})

	t.Run("a clock that goes back freezes no bucket", func(t *testing.T) {
		t.Parallel()

		clock := newManualClock()
		l, err := httpx.NewLimiter(pages, clock.now)
		if err != nil {
			t.Fatal(err)
		}
		lt := newLimited(t, l)
		lt.burst("192.0.2.1:1", 40)
		clock.add(-time.Hour)
		lt.refused("192.0.2.1:1", "1")
		// Time counts again from where the clock went back to.
		clock.add(100 * time.Millisecond)
		lt.burst("192.0.2.1:1", 1)
		lt.refused("192.0.2.1:1", "1")
	})
}

// manualClock is a clock a test sets, safe for concurrent use.
type manualClock struct {
	mu sync.Mutex
	at time.Time
}

func newManualClock() *manualClock {
	return &manualClock{at: time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)}
}

func (c *manualClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *manualClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// addrOf returns the address of the n-th client TestLimiterBounded
// invents: IPv4 for even n, and for odd n, IPv6 in a /48 of its own, so
// that it has two buckets, its /64's and its /48's.
func addrOf(n int) string {
	if n%2 == 0 {
		return fmt.Sprintf("198.18.%d.%d:1", n>>8, n&0xff)
	}
	return fmt.Sprintf("[2001:db8:%x::1]:1", n)
}

func TestLimiterBounded(t *testing.T) {
	t.Parallel()

	const maxEntries = 64
	o := httpx.LimiterOptions{Rate: 1, Burst: 2, MaxEntries: maxEntries, Idle: 10 * time.Second}

	t.Run("ten times as many clients as entries", func(t *testing.T) {
		t.Parallel()

		synctest.Test(t, func(t *testing.T) {
			l, err := httpx.NewLimiter(o, time.Now)
			if err != nil {
				t.Fatal(err)
			}
			lt := newLimited(t, l)
			// A client that spends its burst, then is pushed out by the
			// clients after it.
			lt.burst("192.0.2.1:1", 2)
			lt.refused("192.0.2.1:1", "1")
			for n := range 10 * maxEntries {
				// Every new client is let in, however full the table.
				lt.burst(addrOf(n), 1)
				if got := l.Len(); got > maxEntries {
					t.Fatalf("after %d clients, Len() = %d, over MaxEntries %d", n+2, got, maxEntries)
				}
			}
			if got := l.Len(); got != maxEntries {
				t.Errorf("Len() = %d, want MaxEntries %d", got, maxEntries)
			}
			// The clients seen last keep their buckets: each has one token
			// left of its two.
			last := addrOf(10*maxEntries - 1)
			lt.burst(last, 1)
			lt.refused(last, "1")
			// The client seen longest ago was forgotten, and starts afresh.
			lt.burst("192.0.2.1:1", 2)
			lt.refused("192.0.2.1:1", "1")
		})
	})

	t.Run("an IPv6 client's two buckets both count", func(t *testing.T) {
		t.Parallel()

		synctest.Test(t, func(t *testing.T) {
			l, err := httpx.NewLimiter(o, time.Now)
			if err != nil {
				t.Fatal(err)
			}
			lt := newLimited(t, l)
			lt.burst(addrOf(1), 1)
			if got := l.Len(); got != 2 {
				t.Errorf("after one IPv6 client, Len() = %d, want 2, its /64's bucket and its /48's", got)
			}
			lt.burst(addrOf(0), 1)
			if got := l.Len(); got != 3 {
				t.Errorf("after an IPv4 client too, Len() = %d, want 3", got)
			}
		})
	})

	t.Run("idle clients are forgotten", func(t *testing.T) {
		t.Parallel()

		synctest.Test(t, func(t *testing.T) {
			l, err := httpx.NewLimiter(o, time.Now)
			if err != nil {
				t.Fatal(err)
			}
			lt := newLimited(t, l)
			for n := range maxEntries {
				lt.burst(addrOf(n), 1)
			}
			time.Sleep(o.Idle - time.Second)
			// Seen a second before the rest go idle.
			lt.burst(addrOf(0), 1)
			lt.burst(addrOf(1), 1)
			time.Sleep(time.Second)
			// Each request forgets at least one idle client while there
			// is one, so maxEntries requests forget them all.
			for range maxEntries {
				lt.from("192.0.2.1:1")
			}
			if got := l.Len(); got != 4 {
				t.Errorf("after the others went idle, Len() = %d, want 4: the two clients seen since, one with two "+
					"buckets, and 192.0.2.1", got)
			}
		})
	})

	t.Run("a /48 that is refused makes no buckets", func(t *testing.T) {
		t.Parallel()

		synctest.Test(t, func(t *testing.T) {
			pages := httpx.LimiterOptions{Rate: 10, Burst: 40, MaxEntries: 16, Idle: time.Minute}
			l, err := httpx.NewLimiter(pages, time.Now)
			if err != nil {
				t.Fatal(err)
			}
			lt := newLimited(t, l)
			// A client that has spent its burst, and must not get it back
			// by being pushed out.
			lt.burst("192.0.2.1:1", 40)
			lt.refused("192.0.2.1:1", "1")
			// One /48 spends its own burst through eight /64s, then
			// floods fresh /64s, each refused by the /48's bucket.
			for n := range 8 {
				lt.burst(fmt.Sprintf("[2001:db8:1:%x::1]:1", n), 40)
			}
			before := l.Len()
			for n := range 1000 {
				lt.refused(fmt.Sprintf("[2001:db8:1:%x:1::1]:1", n), "1")
			}
			if got := l.Len(); got != before {
				t.Errorf("after the flood, Len() = %d, want %d: no bucket for a refused /64", got, before)
			}
			lt.refused("192.0.2.1:1", "1")
		})
	})

	t.Run("a refused client's bucket is used, and kept", func(t *testing.T) {
		t.Parallel()

		synctest.Test(t, func(t *testing.T) {
			l, err := httpx.NewLimiter(httpx.LimiterOptions{Rate: 10, Burst: 40, MaxEntries: 4, Idle: time.Minute}, time.Now)
			if err != nil {
				t.Fatal(err)
			}
			lt := newLimited(t, l)
			lt.burst("192.0.2.1:1", 40)
			lt.burst("192.0.2.2:1", 1)
			lt.burst("192.0.2.3:1", 1)
			// Refused, and so used last: the next new clients push out
			// 192.0.2.2 and 192.0.2.3 before it.
			lt.refused("192.0.2.1:1", "1")
			for _, a := range []string{"192.0.2.4:1", "192.0.2.5:1", "192.0.2.6:1"} {
				lt.burst(a, 1)
			}
			lt.refused("192.0.2.1:1", "1")
		})
	})

	t.Run("a clock that goes back stalls no forgetting", func(t *testing.T) {
		t.Parallel()

		clock := newManualClock()
		l, err := httpx.NewLimiter(o, clock.now)
		if err != nil {
			t.Fatal(err)
		}
		lt := newLimited(t, l)
		lt.burst("192.0.2.1:1", 1)
		clock.add(-time.Hour)
		lt.burst("192.0.2.2:1", 1)
		// Idle has passed for both since each was seen, counting from
		// where the clock went back to.
		clock.add(o.Idle)
		for range maxEntries {
			lt.from("192.0.2.3:1")
		}
		if got := l.Len(); got != 1 {
			t.Errorf("Len() = %d, want 1: 192.0.2.3 alone", got)
		}
	})

	t.Run("many clients at once", func(t *testing.T) {
		t.Parallel()

		synctest.Test(t, func(t *testing.T) {
			l, err := httpx.NewLimiter(o, time.Now)
			if err != nil {
				t.Fatal(err)
			}
			lt := newLimited(t, l)
			var wg sync.WaitGroup
			var refused atomic.Int64
			for g := range 8 {
				wg.Go(func() {
					for n := range 2 * maxEntries {
						if lt.from(addrOf(g*2*maxEntries+n)).Code != http.StatusOK {
							refused.Add(1)
						}
					}
				})
			}
			wg.Wait()
			if got := l.Len(); got != maxEntries {
				t.Errorf("Len() = %d, want MaxEntries %d", got, maxEntries)
			}
			if refused.Load() != 0 {
				t.Errorf("%d first requests from new clients were refused, want none", refused.Load())
			}
		})
	})

	t.Run("the zero Limiter holds nothing", func(t *testing.T) {
		t.Parallel()

		var l httpx.Limiter
		if got := l.Len(); got != 0 {
			t.Errorf("the zero Limiter's Len() = %d, want 0", got)
		}
	})
}

func FuzzLimiterOptions(f *testing.F) {
	for _, o := range []httpx.LimiterOptions{
		httpx.PageLimiterOptions(), httpx.CSPReportLimiterOptions(),
		{Rate: 0.25, Burst: 1, MaxEntries: 16, Idle: 4 * time.Second},
		{Rate: httpx.Rate(math.Ldexp(1953125, -54)), Burst: 1, MaxEntries: 16, Idle: math.MaxInt64},
		{Rate: 1.25e8, Burst: 3, MaxEntries: 16, Idle: time.Second},
		{Rate: 3, Burst: 7, MaxEntries: 16, Idle: 2334 * time.Millisecond},
	} {
		f.Add(float64(o.Rate), o.Burst, int64(o.Idle))
	}
	start := time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)
	f.Fuzz(func(t *testing.T, rate float64, burst int, idle int64) {
		o := httpx.LimiterOptions{Rate: httpx.Rate(rate), Burst: burst, MaxEntries: 16, Idle: time.Duration(idle)}
		l, err := httpx.NewLimiter(o, func() time.Time { return start })
		if err != nil {
			if !errors.Is(err, httpx.ErrInvalid) || l != nil {
				t.Fatalf("NewLimiter(%+v) = %v, %v; want nil and an error wrapping ErrInvalid", o, l, err)
			}
			return
		}
		// Accepted, so a token takes at least a nanosecond and a bucket
		// fills within Idle: at one instant, a client has exactly Burst
		// requests, and is then told to wait no longer than Idle / Burst.
		lt := newLimited(t, l)
		check := func(spend func(), refused string, buckets int) {
			spend()
			w := lt.from(refused)
			if w.Code != http.StatusTooManyRequests {
				t.Fatalf("with %+v, the request past the burst = %d, want 429", o, w.Code)
			}
			after, err := strconv.ParseInt(w.Header().Get("Retry-After"), 10, 64)
			limit := int64(math.Ceil(float64(o.Idle) / float64(buckets*o.Burst) / float64(time.Second)))
			if err != nil || after < 1 || after > limit {
				t.Fatalf("with %+v, Retry-After = %q, want 1 to %d", o, w.Header().Get("Retry-After"), limit)
			}
		}
		if burst > 32 {
			if w := lt.from("192.0.2.1:1"); w.Code != http.StatusOK {
				t.Fatalf("with %+v, a first request = %d, want 200", o, w.Code)
			}
			return
		}
		check(func() { lt.burst("192.0.2.1:1", burst) }, "192.0.2.1:1", 1)
		// A /48 holds eight bursts, spent by eight /64s in it.
		check(func() {
			for n := range 8 {
				lt.burst(fmt.Sprintf("[2001:db8:1:%x::1]:1", n), burst)
			}
		}, "[2001:db8:1:ff::1]:1", 8)
	})
}

func TestLimiterRefusalIsNotStored(t *testing.T) {
	t.Parallel()

	for name, o := range map[string]httpx.LimiterOptions{
		"pages":       httpx.PageLimiterOptions(),
		"CSP reports": httpx.CSPReportLimiterOptions(),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			synctest.Test(t, func(t *testing.T) {
				l, err := httpx.NewLimiter(o, time.Now)
				if err != nil {
					t.Fatal(err)
				}
				lt := newLimited(t, l)
				lt.burst("192.0.2.1:1", o.Burst)
				w := lt.from("192.0.2.1:1")
				if w.Code != http.StatusTooManyRequests {
					t.Fatalf("the request past the burst = %d, want 429", w.Code)
				}
				// The refusal answers this client alone, for now: no
				// cache may keep it.
				if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
					t.Errorf("the 429 has Cache-Control %q, want no-store", cc)
				}
			})
		})
	}
}
