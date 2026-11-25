// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package main

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"syscall"
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
	// The job worker keeps the last second of it to record the outcomes
	// of the jobs it cut off (jobs.Options.Reserve).
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
	return r.serve(ctx, h)
}

// ignoreStop returns nil when err is only ctx's own, ctx having ended:
// serve was stopped while it started, which changes nothing on disk.
func ignoreStop(ctx context.Context, err error) error {
	if stopped(ctx, err) {
		return nil
	}
	return err
}

// group runs functions on goroutines of their own, under one
// errgroup.Group, and keeps every error they return, not only the first:
// independent failures are reported together. The first error ends the
// group's context, as errgroup's does. Go is called from one goroutine.
type group struct {
	eg *errgroup.Group
	// errs holds a slot for each function, in the order Go was called,
	// so that the joined errors come in that order whichever failed
	// first. Each goroutine writes its own slot alone.
	errs []*error
}

// newGroup returns a group, and the context that ends with ctx or with
// the group's first error.
func newGroup(ctx context.Context) (*group, context.Context) {
	eg, gctx := errgroup.WithContext(ctx)
	return &group{eg: eg}, gctx
}

// Go runs f on a goroutine of its own.
func (g *group) Go(f func() error) {
	slot := new(error)
	g.errs = append(g.errs, slot)
	g.eg.Go(func() error {
		*slot = f()
		return *slot
	})
}

// Wait waits for every function Go ran to return, and returns all their
// errors, joined in the order Go ran them, or nil when none failed.
func (g *group) Wait() error {
	first := g.eg.Wait()
	errs := make([]error, 0, len(g.errs))
	for _, slot := range g.errs {
		errs = append(errs, *slot)
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	// Every error is in a slot: first is one of them, and nil here.
	return first
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
	key, origin, err := loadMainKey(ctx, cfg.Keys.MainKey, creds, st.dir, log)
	if err != nil {
		return nil, err
	}
	keyring, err := openKeyring(ctx, h, st, cfg.Keys.MainKey, key, origin)
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
	publicLn, internalLn, err := listenBoth(ctx, cfg.Server)
	if err != nil {
		return nil, err
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

// The settings the two listeners' addresses come from, and their
// variables, for the errors that name them.
const (
	publicSetting   = "server.listen (PISATI_SERVER_LISTEN)"
	internalSetting = "server.internal_listen (PISATI_SERVER_INTERNAL_LISTEN)"
)

// listenBoth listens on both of srv's addresses. When either cannot be
// used, it returns every refusal, and closes the listener it did open.
func listenBoth(ctx context.Context, srv config.Server) (public, internal net.Listener, err error) {
	var lc net.ListenConfig
	both, _ := newGroup(ctx)
	both.Go(func() (err error) {
		public, err = listen(ctx, &lc, srv.Listen, publicSetting)
		return err
	})
	both.Go(func() (err error) {
		internal, err = listen(ctx, &lc, srv.InternalListen, internalSetting)
		return err
	})
	if err := both.Wait(); err != nil {
		for _, ln := range []net.Listener{public, internal} {
			if ln != nil {
				err = errors.Join(err, ln.Close())
			}
		}
		return nil, nil, err
	}
	return public, internal, nil
}

// listen listens on addr, the address the setting named setting gives.
func listen(ctx context.Context, lc *net.ListenConfig, addr config.ListenAddr, setting string) (net.Listener, error) {
	ln, err := lc.Listen(ctx, "tcp", addr.String())
	switch {
	case errors.Is(err, syscall.EADDRINUSE):
		return nil, explain(err, "cannot listen on %s, the address that %s sets, because another program already uses it. "+
			"Stop that program, or set %s to another address", addr, setting, setting)
	case err != nil:
		return nil, explain(err, "cannot listen on %s, the address that %s sets: %s", addr, setting, reason(err))
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
				log.ErrorContext(ctx, "health check: the database did not answer", "error", err)
			}
			status, body = http.StatusServiceUnavailable, "unavailable"
		}
		w.WriteHeader(status)
		io.WriteString(w, body) //nolint:errcheck // the status is sent, and a failed write means the client has gone: no one is left to answer
	})
}

// serve runs both listeners, the job worker and the session sweeper under
// one group, until ctx ends or one of them fails. Then each stops taking
// work, and they finish what they have together, within one deadline set
// as the stop begins: drain from h.Clock's time. The servers' Shutdown
// and the worker's own drain both run to it, and a server still busy at
// the deadline is closed, cutting its connections. Then telemetry sends
// what it holds, within what is left of the deadline.
//
// When h.Halt closes, serve stops at once instead: it closes both servers,
// stops telemetry, and returns without waiting for the job worker, whose
// running jobs the next start's ReleaseOrphans makes ready again.
//
// It returns nil once stopped, even when the deadline cut work off;
// errReported when h.Halt cut the stop short, which it logs; and every
// error of a listener that failed, joined.
func (r *running) serve(ctx context.Context, h Host) error {
	// Ended by the halt too, so that the worker and the sweeper stop
	// taking work then, even had no signal ended ctx first.
	serving, stopServing := context.WithCancel(ctx)
	defer stopServing()
	g, gctx := newGroup(serving)
	stopAt := sync.OnceValue(func() time.Time {
		r.log.InfoContext(ctx, "stopping")
		return h.Clock().Add(drain)
	})
	r.log.InfoContext(ctx, "serving started",
		"public", r.publicLn.Addr().String(), "internal", r.internalLn.Addr().String(), "version", version)
	for _, l := range []struct {
		srv     *http.Server
		ln      net.Listener
		setting string
	}{
		{r.public, r.publicLn, publicSetting},
		{r.internal, r.internalLn, internalSetting},
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

	// Waited for apart from the halt, which must not wait for it. On a
	// halt this goroutine ends once the group does, within the drain.
	waited := make(chan error, 1)
	go func() { waited <- g.Wait() }()
	select {
	case err := <-waited:
		deadline, cancel := context.WithDeadline(context.WithoutCancel(ctx), stopAt())
		defer cancel()
		if ferr := r.tel.Shutdown(deadline); ferr != nil {
			r.log.WarnContext(deadline, "telemetry did not stop cleanly: what it held may not have been sent", "error", ferr)
		}
		return err
	case <-h.Halt:
		stopServing()
		return r.halt(ctx)
	}
}

// halt stops serving at once, as a second SIGTERM or SIGINT asks: it
// closes both servers, cutting their connections, and stops telemetry
// without waiting for it to send what it holds. It returns errReported,
// having logged the halt, or the servers' errors in closing.
func (r *running) halt(ctx context.Context) error {
	r.log.ErrorContext(ctx, "stop cut short: a second SIGTERM or SIGINT signal arrived, so Pisati stops now, "+
		"without waiting for requests and jobs to finish")
	if err := errors.Join(r.public.Close(), r.internal.Close()); err != nil {
		return err
	}
	// A context already ended: Shutdown stops both providers without
	// waiting for an export.
	now, cancel := context.WithCancel(context.WithoutCancel(ctx))
	cancel()
	if err := r.tel.Shutdown(now); err != nil && !errors.Is(err, context.Canceled) {
		r.log.WarnContext(ctx, "telemetry did not stop cleanly: what it held may not have been sent", "error", err)
	}
	return errReported
}

// serveOn serves srv on ln until srv is shut down, and returns why it
// stopped otherwise: ln, the address setting gives, failed.
func serveOn(srv *http.Server, ln net.Listener, setting string) error {
	err := srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return explain(err, "stopped serving on %s, the address that %s sets, because accepting connections there failed: %s",
		ln.Addr(), setting, reason(err))
}

// shutdown shuts srv down within ctx's deadline, and closes the
// connections still open when it passes, which is no failure.
func shutdown(ctx context.Context, srv *http.Server, setting string, log *slog.Logger) error {
	err := srv.Shutdown(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	log.WarnContext(ctx, "connections still open at the stop deadline were closed", "listener", setting, "waited", drain.String())
	return srv.Close()
}
