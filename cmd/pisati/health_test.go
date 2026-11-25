// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package main

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeHealthz serves /healthz on 127.0.0.1 at a free port, answering with
// the status status holds, and returns the port and each request it was
// sent, as its method, path and Host: the address the client called. It
// stops when the test ends.
func fakeHealthz(t *testing.T, status *atomic.Int32) (int, *atomic.Pointer[[]string]) {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	asked := &atomic.Pointer[[]string]{}
	asked.Store(&[]string{})
	srv := &http.Server{
		ReadHeaderTimeout: time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			paths := append(*asked.Load(), r.Method+" "+r.URL.Path+" "+r.Host)
			asked.Store(&paths)
			w.WriteHeader(int(status.Load()))
		}),
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	t.Cleanup(func() {
		if err := srv.Close(); err != nil {
			t.Error(err)
		}
		if err := <-done; !errors.Is(err, http.ErrServerClosed) {
			t.Error(err)
		}
	})
	return portOf(t, ln), asked
}

func TestHealthCommand(t *testing.T) {
	t.Run("serve running", func(t *testing.T) {
		s := newSite(t)
		v := s.serve(t)
		if r := s.run(t, "health"); r.code != 0 || r.stdout != "" || r.stderr != "" {
			t.Errorf("pisati health = %d, %q, %q; want 0 and nothing printed", r.code, r.stdout, r.stderr)
		}
		if code := v.stop(); code != 0 {
			t.Errorf("serve exited %d", code)
		}
	})

	t.Run("nothing listening", func(t *testing.T) {
		s := newSite(t)
		r := s.run(t, "health")
		want := fmt.Sprintf("healthz: nothing accepts connections on 127.0.0.1:%d. Check that pisati serve is running. "+
			"This is the address that server.internal_listen (PISATI_SERVER_INTERNAL_LISTEN) sets.", s.internal)
		if r.code != 1 || r.line(t) != want || r.stdout != "" {
			t.Errorf("pisati health = %d, %q, %q; want 1, %q", r.code, r.stdout, r.stderr, want)
		}
	})

	t.Run("nothing listening at an unspecified address", func(t *testing.T) {
		s := newSite(t)
		s.env["PISATI_SERVER_INTERNAL_LISTEN"] = fmt.Sprintf(":%d", s.internal)
		r := s.run(t, "health")
		want := fmt.Sprintf("healthz: nothing accepts connections on 127.0.0.1:%[1]d. Check that pisati serve is running. "+
			"server.internal_listen (PISATI_SERVER_INTERNAL_LISTEN) is :%[1]d, which means every address of this machine, "+
			"so pisati health calls 127.0.0.1.", s.internal)
		if r.code != 1 || r.line(t) != want {
			t.Errorf("pisati health = %d, %q; want 1, %q", r.code, r.stderr, want)
		}
	})

	t.Run("the address it calls", func(t *testing.T) {
		var status atomic.Int32
		status.Store(http.StatusOK)
		port, asked := fakeHealthz(t, &status)
		for _, tc := range []struct {
			listen string
			code   int
			line   string
		}{
			// No host, or an unspecified one: the loopback address.
			{fmt.Sprintf(":%d", port), 0, ""},
			{fmt.Sprintf("0.0.0.0:%d", port), 0, ""},
			{fmt.Sprintf("[::]:%d", port), 0, ""},
			// A host named: that host, here the one the fake listens on.
			{fmt.Sprintf("127.0.0.1:%d", port), 0, ""},
			// IPv6 loopback, where nothing listens on the port.
			{fmt.Sprintf("[::1]:%d", port), 1, fmt.Sprintf("healthz: nothing accepts connections on [::1]:%d. Check that "+
				"pisati serve is running. This is the address that server.internal_listen (PISATI_SERVER_INTERNAL_LISTEN) sets.\n", port)},
		} {
			s := newSite(t)
			s.env["PISATI_SERVER_INTERNAL_LISTEN"] = tc.listen
			r := s.run(t, "health")
			if r.code != tc.code || r.stderr != tc.line || r.stdout != "" {
				t.Errorf("internal_listen %s: pisati health = %d, %q, %q; want %d, %q", tc.listen, r.code, r.stdout, r.stderr, tc.code, tc.line)
			}
		}
		// The fake is reached through each unspecified address on some
		// systems, so the Host each request names is what shows the
		// address health called.
		one := fmt.Sprintf("GET /healthz 127.0.0.1:%d", port)
		if got := strings.Join(*asked.Load(), ", "); got != strings.Repeat(one+", ", 3)+one {
			t.Errorf("the fake was sent %s, want %s four times", got, one)
		}
	})

	t.Run("a status other than 200", func(t *testing.T) {
		var status atomic.Int32
		status.Store(http.StatusServiceUnavailable)
		port, _ := fakeHealthz(t, &status)
		s := newSite(t)
		s.env["PISATI_SERVER_INTERNAL_LISTEN"] = fmt.Sprintf("127.0.0.1:%d", port)
		r := s.run(t, "health")
		want := fmt.Sprintf("healthz: 127.0.0.1:%d answered GET /healthz with status 503, not 200: pisati serve runs, "+
			"but its database did not answer. The log of pisati serve says why. This is the address that "+
			"server.internal_listen (PISATI_SERVER_INTERNAL_LISTEN) sets.", port)
		if r.code != 1 || r.line(t) != want {
			t.Errorf("pisati health = %d, %q; want 1, %q", r.code, r.stderr, want)
		}

		// Another program, answering another status.
		status.Store(http.StatusNotFound)
		r = s.run(t, "health")
		want = fmt.Sprintf("healthz: 127.0.0.1:%[1]d answered GET /healthz with status 404, not 200. pisati serve answers "+
			"only 200 or 503 there, so another program may be listening at 127.0.0.1:%[1]d. This is the address that "+
			"server.internal_listen (PISATI_SERVER_INTERNAL_LISTEN) sets.", port)
		if r.code != 1 || r.line(t) != want {
			t.Errorf("pisati health = %d, %q; want 1, %q", r.code, r.stderr, want)
		}
	})

	t.Run("no answer within 4 seconds", func(t *testing.T) {
		// A listener that accepts connections and never answers.
		var lc net.ListenConfig
		ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		accepted := make(chan net.Conn, 1)
		go func() {
			conn, err := ln.Accept()
			if err == nil {
				accepted <- conn
			}
			close(accepted)
		}()
		t.Cleanup(func() {
			if err := ln.Close(); err != nil {
				t.Error(err)
			}
			for conn := range accepted {
				if err := conn.Close(); err != nil {
					t.Error(err)
				}
			}
		})
		port := portOf(t, ln)
		s := newSite(t)
		s.env["PISATI_SERVER_INTERNAL_LISTEN"] = fmt.Sprintf("127.0.0.1:%d", port)
		start := time.Now()
		r := s.run(t, "health")
		took := time.Since(start)
		want := fmt.Sprintf("healthz: 127.0.0.1:%d did not answer GET /healthz within 4 seconds. This is the address that "+
			"server.internal_listen (PISATI_SERVER_INTERNAL_LISTEN) sets.", port)
		if r.code != 1 || r.line(t) != want {
			t.Errorf("pisati health = %d, %q; want 1, %q", r.code, r.stderr, want)
		}
		if took < 4*time.Second || took >= 5*time.Second {
			t.Errorf("pisati health took %v, want 4 s and not much more", took)
		}
	})

	t.Run("the configuration refused", func(t *testing.T) {
		s := newSite(t)
		delete(s.env, "PISATI_SERVER_BASE_URL")
		r := s.run(t, "health")
		if line := r.line(t); r.code != 1 || !strings.HasPrefix(line, "config: server.base_url") || !strings.HasSuffix(line, ".") {
			t.Errorf("pisati health = %d, %q; want 1, one config: line naming server.base_url", r.code, r.stderr)
		}
	})
}
