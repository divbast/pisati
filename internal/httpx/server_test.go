// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package httpx_test

import (
	"bufio"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/divbast/pisati/internal/httpx"
)

func TestDefaultTimeouts(t *testing.T) {
	t.Parallel()

	want := httpx.Timeouts{
		ReadHeader:     10 * time.Second,
		Read:           30 * time.Second,
		Write:          60 * time.Second,
		Idle:           120 * time.Second,
		MaxHeaderBytes: 64 << 10,
	}
	if got := httpx.DefaultTimeouts(); got != want {
		t.Errorf("DefaultTimeouts() = %+v, want %+v", got, want)
	}
}

func TestNewServer(t *testing.T) {
	t.Parallel()

	d := httpx.DefaultTimeouts()
	tests := []struct {
		name string
		in   httpx.Timeouts
		want httpx.Timeouts
	}{
		{name: "defaults", in: d, want: d},
		{
			name: "every field set",
			in:   httpx.Timeouts{ReadHeader: 1, Read: 2, Write: 3, Idle: 4, MaxHeaderBytes: 5},
			want: httpx.Timeouts{ReadHeader: 1, Read: 2, Write: 3, Idle: 4, MaxHeaderBytes: 5},
		},
		{name: "zero", in: httpx.Timeouts{}, want: d},
		{
			name: "some set",
			in:   httpx.Timeouts{Read: 5 * time.Second, MaxHeaderBytes: 4096},
			want: httpx.Timeouts{ReadHeader: d.ReadHeader, Read: 5 * time.Second, Write: d.Write, Idle: d.Idle, MaxHeaderBytes: 4096},
		},
		{name: "negative", in: httpx.Timeouts{ReadHeader: -1, Read: -1, Write: -1, Idle: -1, MaxHeaderBytes: -1}, want: d},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			log, buf := newLog()
			h := http.NotFoundHandler()
			srv := httpx.NewServer(listenAddr(t, "127.0.0.1:8080"), h, tt.in, log)
			got := httpx.Timeouts{
				ReadHeader: srv.ReadHeaderTimeout, Read: srv.ReadTimeout, Write: srv.WriteTimeout,
				Idle: srv.IdleTimeout, MaxHeaderBytes: srv.MaxHeaderBytes,
			}
			if got != tt.want {
				t.Errorf("NewServer(%+v) has %+v, want %+v", tt.in, got, tt.want)
			}
			if srv.Addr != "127.0.0.1:8080" || srv.Handler == nil {
				t.Errorf("NewServer has Addr %q and Handler %v, want 127.0.0.1:8080 and h", srv.Addr, srv.Handler)
			}

			srv.ErrorLog.Print("http: a line from the server")
			records := buf.records(t)
			if len(records) != 1 || records[0]["level"] != "ERROR" || records[0]["msg"] != "http: a line from the server" {
				t.Errorf("ErrorLog wrote %s, want one ERROR record through log", buf)
			}
		})
	}
}

// stamp is when something happened in a bubble, and the error it met.
type stamp struct {
	at  time.Time
	err error
}

func TestServerTimeouts(t *testing.T) {
	t.Parallel()

	d := httpx.DefaultTimeouts()
	// inBubble runs f in a synctest bubble, passing it serve, which serves
	// h with the default Timeouts on a new in-memory listener, and returns
	// the listener. Every channel f waits on must be made inside f: the
	// bubble's clock moves only while each of its goroutines waits on
	// something of the bubble's own.
	inBubble := func(t *testing.T, f func(t *testing.T, serve func(h http.Handler) *pipeListener)) {
		t.Helper()
		synctest.Test(t, func(t *testing.T) {
			log, _ := newLog()
			f(t, func(h http.Handler) *pipeListener {
				return servePipe(t, httpx.NewServer(listenAddr(t, "127.0.0.1:8080"), h, d, log))
			})
		})
	}
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, err := io.WriteString(w, "ok"); err != nil {
			t.Errorf("writing ok: %v", err)
		}
	})

	t.Run("a slow header is cut off at ReadHeader", func(t *testing.T) {
		t.Parallel()

		inBubble(t, func(t *testing.T, serve func(http.Handler) *pipeListener) {
			start := time.Now()
			conn := serve(ok).dial(t)
			writeAll(t, conn, "GET / HTTP/1.1\r\nHost: blog.example\r\n")
			_, err := conn.Read(make([]byte, 1))
			if took := time.Since(start); !errors.Is(err, io.EOF) || took != d.ReadHeader {
				t.Errorf("an unfinished header was cut off after %v with %v, want %v and io.EOF", took, err, d.ReadHeader)
			}
		})
	})

	t.Run("a slow body is cut off at Read", func(t *testing.T) {
		t.Parallel()

		inBubble(t, func(t *testing.T, serve func(http.Handler) *pipeListener) {
			start := time.Now()
			read := make(chan stamp, 1)
			conn := serve(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				_, err := io.ReadAll(r.Body)
				read <- stamp{at: time.Now(), err: err}
			})).dial(t)
			writeAll(t, conn, "POST / HTTP/1.1\r\nHost: blog.example\r\nContent-Length: 10\r\n\r\nx")
			got := <-read
			if took := got.at.Sub(start); !errors.Is(got.err, os.ErrDeadlineExceeded) || took != d.Read {
				t.Errorf("an unfinished body was cut off after %v with %v, want %v and os.ErrDeadlineExceeded", took, got.err, d.Read)
			}
		})
	})

	t.Run("a slow reader is cut off at Write", func(t *testing.T) {
		t.Parallel()

		inBubble(t, func(t *testing.T, serve func(http.Handler) *pipeListener) {
			start := time.Now()
			wrote := make(chan stamp, 1)
			conn := serve(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, err := w.Write(make([]byte, 1<<20))
				wrote <- stamp{at: time.Now(), err: err}
			})).dial(t)
			writeAll(t, conn, "GET / HTTP/1.1\r\nHost: blog.example\r\n\r\n")
			// The client reads nothing, so the response cannot be written.
			got := <-wrote
			if took := got.at.Sub(start); !errors.Is(got.err, os.ErrDeadlineExceeded) || took != d.Write {
				t.Errorf("an unread response was cut off after %v with %v, want %v and os.ErrDeadlineExceeded", took, got.err, d.Write)
			}
		})
	})

	t.Run("an idle connection is closed at Idle", func(t *testing.T) {
		t.Parallel()

		inBubble(t, func(t *testing.T, serve func(http.Handler) *pipeListener) {
			start := time.Now()
			conn := serve(ok).dial(t)
			br := bufio.NewReader(conn)
			writeAll(t, conn, "GET / HTTP/1.1\r\nHost: blog.example\r\n\r\n")
			if status, body := readResponse(t, br); status != http.StatusOK || body != "ok" {
				t.Fatalf("GET / = %d %q, want 200 \"ok\"", status, body)
			}
			answered := time.Now()
			_, err := br.ReadByte()
			if took := time.Since(answered); !errors.Is(err, io.EOF) || took != d.Idle {
				t.Errorf("an idle connection was closed after %v with %v, want %v and io.EOF", took, err, d.Idle)
			}
			if answered != start {
				t.Errorf("the answer took %v of the bubble's time, want none", answered.Sub(start))
			}
		})
	})

	t.Run("a large header is refused at MaxHeaderBytes", func(t *testing.T) {
		t.Parallel()

		for _, tt := range []struct {
			name  string
			bytes int
			want  int
		}{
			{name: "within", bytes: d.MaxHeaderBytes / 2, want: http.StatusOK},
			{name: "twice over", bytes: 2 * d.MaxHeaderBytes, want: http.StatusRequestHeaderFieldsTooLarge},
		} {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				inBubble(t, func(t *testing.T, serve func(http.Handler) *pipeListener) {
					conn := serve(ok).dial(t)
					// The server stops reading a header that is too large,
					// so it is written apart from the reading.
					sent := make(chan error, 1)
					go func() {
						_, err := io.WriteString(conn, "GET / HTTP/1.1\r\nHost: blog.example\r\nX-Large: "+
							strings.Repeat("a", tt.bytes)+"\r\n\r\n")
						sent <- err
					}()
					if status, _ := readResponse(t, bufio.NewReader(conn)); status != tt.want {
						t.Errorf("a header of %d bytes was answered %d, want %d", tt.bytes, status, tt.want)
					}
					if err := conn.Close(); err != nil {
						t.Errorf("closing the client's end: %v", err)
					}
					if err := <-sent; err != nil && !errors.Is(err, io.ErrClosedPipe) {
						t.Errorf("writing the header: %v", err)
					}
				})
			})
		}
	})
}
