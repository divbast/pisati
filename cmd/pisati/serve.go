// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"golang.org/x/sync/errgroup"

	"github.com/divbast/pisati/internal/app"
	"github.com/divbast/pisati/internal/config"
	"github.com/divbast/pisati/internal/crypto"
	"github.com/divbast/pisati/internal/httpx"
	"github.com/divbast/pisati/internal/jobs"
	"github.com/divbast/pisati/internal/store"
	"github.com/divbast/pisati/internal/telemetry"
	"github.com/divbast/pisati/internal/web"
)

const (
	// drain is the one deadline a stop has, from the signal: the two
	// listeners, the job worker and the session sweeper finish what they
	// have within it, and telemetry sends what it holds in the time left.
	drain = 8 * time.Second
	// sessionSweepEvery is how often serve deletes the sessions that have
	// expired, after a first sweep as it starts.
	sessionSweepEvery = 10 * time.Minute
	// pingWithin bounds /healthz's check of the database.
	pingWithin = 2 * time.Second
)

// serveCmd is pisati serve.
type serveCmd struct{}

// Run serves until ctx ends.
func (serveCmd) Run(ctx context.Context, h Host) error {
	return serve(ctx, h)
}

// serve checks and wires everything, and then serves the public listener,
// the internal listener, the job worker and the session sweeper together
// until ctx ends or one of them fails. Nothing listens or runs until every
// check has passed, so a refusal never leaves Pisati half started. A stop
// during the start is no failure: serve returns nil then.
//
// Once ctx ends it stops: see running.serve. Then it closes the database
// and releases the state directory's lock.
func serve(ctx context.Context, h Host) (err error) {
	cfg, creds, err := loadConfig(ctx, h)
	if err != nil {
		return ignoreStop(ctx, err)
	}
	log := telemetry.NewLogger(h.Stdout, cfg.Log)
	// The SDK's own errors go to log rather than to Go's log package, and
	// so to stderr in another format. It is the one global setting Pisati
	// makes, and it makes it here alone.
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		log.WarnContext(ctx, "OpenTelemetry reported an error", "error", err)
	}))

	st, _, _, err := openState(ctx, cfg.Storage.StateDir)
	if err != nil {
		return ignoreStop(ctx, err)
	}
	var r *running
	defer func() {
		err = errors.Join(err, st.close())
		if r != nil {
			log.InfoContext(ctx, "stopped")
		}
	}()
	r, err = start(ctx, h, cfg, creds, st, log)
	if err != nil {
		return ignoreStop(ctx, err)
	}
	return r.serve(ctx)
}

// ignoreStop returns nil when err is only ctx's own, ctx having ended:
// serve was stopped while it started, which changes nothing on disk.
func ignoreStop(ctx context.Context, err error) error {
	if stopped(ctx, err) {
		return nil
	}
	return err
}

// running is everything serve runs, once start has made it.
type running struct {
	log                  *slog.Logger
	tel                  telemetry.Providers
	public, internal     *http.Server
	publicLn, internalLn net.Listener
	worker               *jobs.Worker
	sessions             *app.Sessions
}

// start makes everything serve runs, in the database st has open: the
// keyring, the job worker, with the leases a stopped run left released,
// the sessions, telemetry, both listeners' handlers, and, last, the two
// listeners. On an error it leaves nothing open but st.
func start(ctx context.Context, h Host, cfg config.Config, creds fs.FS, st *state, log *slog.Logger) (_ *running, err error) {
	key, err := loadMainKey(ctx, cfg.Keys.MainKey, creds, st.dir, log)
	if err != nil {
		return nil, err
	}
	keyring, err := openKeyring(ctx, h, st, cfg.Keys.MainKey, key)
	if err != nil {
		return nil, err
	}
	reencrypt, err := crypto.NewReencrypter(keyring)
	if err != nil {
		return nil, err
	}
	opts := jobs.DefaultOptions()
	opts.Drain = drain
	worker, err := jobs.NewWorker(st.db, map[jobs.Kind]jobs.Handler{reencrypt.Kind(): reencrypt.Handle}, h.Clock, h.Rand, log, opts)
	if err != nil {
		return nil, err
	}
	// Under the lock, and before the worker runs: every lease is one a run
	// that stopped left.
	if _, err := worker.ReleaseOrphans(ctx); err != nil {
		return nil, err
	}
	sessions, err := app.NewSessions(st.db, h.Clock, h.Rand, cfg.Server.BaseURL, log)
	if err != nil {
		return nil, err
	}

	tel, err := telemetry.NewProviders(ctx, cfg.Telemetry, version, log)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			stop, cancel := context.WithTimeout(context.WithoutCancel(ctx), drain)
			defer cancel()
			err = errors.Join(err, tel.Shutdown(stop))
		}
	}()
	public, err := publicHandler(ctx, h, cfg, tel, sessions, log)
	if err != nil {
		return nil, err
	}
	internal := internalHandler(st.db, tel, cfg.Telemetry.Pprof, h.Clock, log)

	var lc net.ListenConfig
	publicLn, err := listen(ctx, &lc, cfg.Server.Listen, "server.listen")
	if err != nil {
		return nil, err
	}
	internalLn, err := listen(ctx, &lc, cfg.Server.InternalListen, "server.internal_listen")
	if err != nil {
		return nil, errors.Join(err, publicLn.Close())
	}
	return &running{
		log:        log,
		tel:        tel,
		public:     httpx.NewServer(cfg.Server.Listen, public, httpx.DefaultTimeouts(), log),
		internal:   httpx.NewServer(cfg.Server.InternalListen, internal, httpx.DefaultTimeouts(), log),
		publicLn:   publicLn,
		internalLn: internalLn,
		worker:     worker,
		sessions:   sessions,
	}, nil
}

// listen listens on addr, the address the setting named setting gives.
func listen(ctx context.Context, lc *net.ListenConfig, addr config.ListenAddr, setting string) (net.Listener, error) {
	ln, err := lc.Listen(ctx, "tcp", addr.String())
	if err != nil {
		return nil, fmt.Errorf("cannot listen on %s, the address set by %s: %s", addr, setting, reason(err))
	}
	return ln, nil
}

// publicHandler returns the public listener's handler: the site, inside
// the chain every request passes, outermost first.
func publicHandler(ctx context.Context, h Host, cfg config.Config, tel telemetry.Providers, sessions *app.Sessions, log *slog.Logger) (http.Handler, error) {
	base := cfg.Server.BaseURL
	assets, err := web.NewAssets(ctx, web.Dist(), base, cfg.Dev)
	if err != nil {
		return nil, err
	}
	clientIP := httpx.NewClientIP(cfg.Server.TrustedProxies)
	pages, err := httpx.NewLimiter(httpx.PageLimiterOptions(), h.Clock)
	if err != nil {
		return nil, err
	}
	reports, err := httpx.NewLimiter(httpx.CSPReportLimiterOptions(), h.Clock)
	if err != nil {
		return nil, err
	}
	site, err := web.NewSite(assets, base, reports, clientIP, log)
	if err != nil {
		return nil, err
	}
	return httpx.Chain(site.Handler(),
		httpx.RequestID(),
		httpx.Recover(log),
		tel.HTTP(h.Clock, cfg.Server.TrustedProxies),
		httpx.AccessLog(log, h.Clock),
		httpx.SecurityHeaders(base, cfg.Server.HSTS, cfg.Dev),
		httpx.CrossOrigin(base),
		pages.Middleware(clientIP),
		web.SessionMiddleware(sessions, log),
	), nil
}

// internalHandler returns the internal listener's handler: /healthz,
// /metrics and, when pprof is on, the profiler, on a mux of their own,
// which answers everything else 404 with no body.
func internalHandler(db *store.Store, tel telemetry.Providers, pprof config.Switch, clock func() time.Time, log *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /healthz", healthz(db, log))
	mux.Handle("GET /metrics", tel.Metrics())
	telemetry.Pprof(mux, pprof)
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	return httpx.Chain(mux, httpx.RequestID(), httpx.Recover(log), httpx.AccessLog(log, clock))
}

// healthz answers 200 ok while the database answers a ping within
// pingWithin, and otherwise 503 unavailable, logging why: the reason goes
// to the log alone, never into the answer.
func healthz(db *store.Store, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), pingWithin)
		defer cancel()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		status, body := http.StatusOK, "ok"
		if err := db.DB().PingContext(ctx); err != nil {
			if r.Context().Err() == nil {
				log.ErrorContext(ctx, "health check found the database unavailable", "error", err)
			}
			status, body = http.StatusServiceUnavailable, "unavailable"
		}
		w.WriteHeader(status)
		io.WriteString(w, body) //nolint:errcheck // the status is sent, and a failed write means the client has gone: no one is left to answer
	})
}

// serve runs both listeners, the job worker and the session sweeper under
// one errgroup, until ctx ends or one of them fails. Then each stops
// taking work, and they finish what they have together, within one
// deadline, drain, from that moment: the servers' Shutdown and the
// worker's own drain both start then. A server still busy at the deadline
// is closed, cutting its connections. Then telemetry sends what it holds,
// within what is left of the deadline.
//
// It returns nil once stopped, even when the deadline cut work off, and
// the error of a listener that failed.
func (r *running) serve(ctx context.Context) error {
	g, gctx := errgroup.WithContext(ctx)
	// The one deadline, set once, as the stop begins. It is the runtime's
	// clock that ends a context, so the deadline is read from it.
	stopAt := sync.OnceValue(func() time.Time {
		r.log.InfoContext(ctx, "stopping")
		return time.Now().Add(drain)
	})
	r.log.InfoContext(ctx, "serving started",
		"public", r.publicLn.Addr().String(), "internal", r.internalLn.Addr().String(), "version", version)
	for _, l := range []struct {
		srv     *http.Server
		ln      net.Listener
		setting string
	}{
		{r.public, r.publicLn, "server.listen"},
		{r.internal, r.internalLn, "server.internal_listen"},
	} {
		g.Go(func() error {
			return serveOn(l.srv, l.ln, l.setting)
		})
		g.Go(func() error {
			<-gctx.Done()
			deadline, cancel := context.WithDeadline(context.WithoutCancel(ctx), stopAt())
			defer cancel()
			return shutdown(deadline, l.srv, l.setting, r.log)
		})
	}
	g.Go(func() error { return r.worker.Run(gctx) })
	g.Go(func() error { return r.sessions.SweepEvery(gctx, sessionSweepEvery) })
	err := g.Wait()

	deadline, cancel := context.WithDeadline(context.WithoutCancel(ctx), stopAt())
	defer cancel()
	if ferr := r.tel.Shutdown(deadline); ferr != nil {
		r.log.WarnContext(deadline, "queued traces not sent before stop", "error", ferr)
	}
	return err
}

// serveOn serves srv on ln until srv is shut down, and returns why it
// stopped otherwise: ln, the address setting gives, failed.
func serveOn(srv *http.Server, ln net.Listener, setting string) error {
	err := srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return fmt.Errorf("stopped serving on %s, the address set by %s: %s", ln.Addr(), setting, reason(err))
}

// shutdown shuts srv down within ctx's deadline, and closes the
// connections still open when it passes, which is no failure.
func shutdown(ctx context.Context, srv *http.Server, setting string, log *slog.Logger) error {
	err := srv.Shutdown(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	log.WarnContext(ctx, "connections closed at the stop deadline", "listener", setting, "deadline", drain)
	return srv.Close()
}
