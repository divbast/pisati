// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package httpx_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/divbast/pisati/internal/config"
)

// baseURL returns s as a config.BaseURL, failing the test if it is not one.
func baseURL(t *testing.T, s string) config.BaseURL {
	t.Helper()
	b, err := config.ParseBaseURL(s)
	if err != nil {
		t.Fatalf("ParseBaseURL(%q): %v", s, err)
	}
	return b
}

// listenAddr returns s as a config.ListenAddr, failing the test if it is
// not one.
func listenAddr(t *testing.T, s string) config.ListenAddr {
	t.Helper()
	a, err := config.ParseListenAddr(s)
	if err != nil {
		t.Fatalf("ParseListenAddr(%q): %v", s, err)
	}
	return a
}

// logBuffer holds what a JSON slog handler writes to it, and is safe for
// the server's goroutines and the test's at once.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// String returns every line logged so far.
func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// records returns each record logged so far, decoded.
func (b *logBuffer) records(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for line := range strings.Lines(b.String()) {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("the log line %q is not JSON: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

// newLog returns a logger that writes JSON records, at every level, to
// the buffer it returns.
func newLog() (*slog.Logger, *logBuffer) {
	buf := &logBuffer{}
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

// serve serves h on a new loopback test server, closed when the test ends.
func serve(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// send sends req with srv's client, and returns the answer's status,
// header and body, failing the test if there is none.
func send(t *testing.T, srv *httptest.Server, req *http.Request) (int, http.Header, string) {
	t.Helper()
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL.Path, err)
	}
	body, err := io.ReadAll(resp.Body)
	if cerr := resp.Body.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		t.Fatalf("%s %s: reading the body: %v", req.Method, req.URL.Path, err)
	}
	return resp.StatusCode, resp.Header, string(body)
}

// request returns a new request for srv, failing the test if it cannot.
func request(t *testing.T, srv *httptest.Server, method, path string, body io.Reader) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, srv.URL+path, body)
	if err != nil {
		t.Fatalf("NewRequest %s %s: %v", method, path, err)
	}
	return req
}

// steppingClock returns a clock that reads start, then a step later at
// each call after.
func steppingClock(start time.Time, step time.Duration) func() time.Time {
	var mu sync.Mutex
	now := start
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		t := now
		now = now.Add(step)
		return t
	}
}

// pipeListener is a net.Listener whose connections are in-memory pipes. A
// synctest bubble can wait on a pipe, as it cannot on a real socket, so a
// server on one can be timed with the bubble's fake clock. Make it inside
// the bubble that uses it.
type pipeListener struct {
	conns  chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func newPipeListener() *pipeListener {
	return &pipeListener{conns: make(chan net.Conn), closed: make(chan struct{})}
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (*pipeListener) Addr() net.Addr {
	return pipeAddr{}
}

// dial returns the client's end of a new connection, once the server has
// accepted it. The connection is closed when the test ends.
func (l *pipeListener) dial(t *testing.T) net.Conn {
	t.Helper()
	client, server := net.Pipe()
	select {
	case l.conns <- server:
	case <-l.closed:
		t.Fatal("dial: the listener is closed")
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Errorf("closing the client's end: %v", err)
		}
	})
	return client
}

// pipeAddr is a pipeListener's address.
type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "pipe" }

// servePipe serves srv on a new pipeListener, which it returns. When the
// test ends, it closes srv and waits for Serve to return and for every
// connection to end: a bubble's clock stops once its test function
// returns, and net/http may still be waiting out a delay before it closes
// a connection.
func servePipe(t *testing.T, srv *http.Server) *pipeListener {
	t.Helper()
	ln := newPipeListener()
	var conns sync.WaitGroup
	srv.ConnState = func(_ net.Conn, state http.ConnState) {
		switch state {
		case http.StateNew:
			conns.Add(1)
		case http.StateClosed, http.StateHijacked:
			conns.Done()
		case http.StateActive, http.StateIdle:
		}
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	t.Cleanup(func() {
		if err := srv.Close(); err != nil {
			t.Errorf("closing the server: %v", err)
		}
		if err := <-done; !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Serve returned %v, want http.ErrServerClosed", err)
		}
		conns.Wait()
	})
	return ln
}

// writeAll writes s to conn, failing the test if it cannot.
func writeAll(t *testing.T, conn net.Conn, s string) {
	t.Helper()
	if _, err := io.WriteString(conn, s); err != nil {
		t.Fatalf("writing to the server: %v", err)
	}
}

// readResponse reads one response from br, and its body, failing the test
// if it cannot.
func readResponse(t *testing.T, br *bufio.Reader) (int, string) {
	t.Helper()
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("reading the response: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	if cerr := resp.Body.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		t.Fatalf("reading the response's body: %v", err)
	}
	return resp.StatusCode, string(body)
}
