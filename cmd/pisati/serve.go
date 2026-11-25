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
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"go.opentelemetry.io/otel"
	"golang.org/x/sync/errgroup"

	"github.com/divbast/pisati/internal/app"
	"github.com/divbast/pisati/internal/config"
	"github.com/divbast/pisati/internal/crypto"
	"github.com/divbast/pisati/internal/explain"
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
	// checkWithin bounds /healthz's read of the database.
	checkWithin = 2 * time.Second
)

// serveCmd is pisati serve.
type serveCmd struct{}

// Run serves until ctx ends, and stops at once, whatever serve is doing,
// when h.Halt closes, as a second SIGTERM or SIGINT does: see life.halt.
// serve then goes on without Run, and ends with the process, which exits
// just after.
func (serveCmd) Run(ctx context.Context, h Host) error {
	l := &life{}
	done := make(chan error, 1)
	go func() { done <- serve(ctx, h, l) }()
	select {
	case err := <-done:
		return err
	case <-h.Halt:
		return l.halt(ctx)
	}
}

// serve checks and wires everything, and then serves the public listener,
// the internal listener, the job worker and the session sweeper together
// until ctx ends or one of them fails. Nothing listens or runs until every
// check has passed, so a refusal never leaves Pisati half started. A stop
// during the start is no failure: serve returns nil then. It records in l
// what it opens, as it opens it, for a halt to close.
//
// Once ctx ends it stops: see running.serve. Then it closes the database
// and releases the state directory's lock.
func serve(ctx context.Context, h Host, l *life) (err error) {
	cfg, creds, err := loadConfig(ctx, h)
	if err != nil {
		return ignoreStop(ctx, err)
	}
	if err := sameAddress(cfg.Server); err != nil {
		return err
	}
	log := l.logger(telemetry.NewLogger(h.Stdout, cfg.Log).Handler())
	// The SDK's own errors go to log rather than to Go's log package, and
	// so to stderr in another format. It is the one global setting Pisati
	// makes, and it makes it here alone.
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		log.WarnContext(ctx, "OpenTelemetry reported an error", "error", err)
	}))

	warnWithoutProxy(ctx, cfg.Server, log)

	st, err := openServingState(ctx, cfg.Storage.StateDir, l)
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
	r, err = start(ctx, h, cfg, creds, st, log, l)
	if err != nil {
		return ignoreStop(ctx, err)
	}
	return r.serve(ctx, h, l)
}

// What the warning without a trusted proxy says.
const (
	noProxyEffect = "server.base_url is an https URL. Pisati serves only http, so a proxy in front of Pisati takes " +
		"the visitors' https connections, and sends their requests on to Pisati. Without a trusted proxy, Pisati " +
		"sees the proxy's address as the address of every visitor. Every visitor then shares the rate limit that " +
		"Pisati sets for one visitor: once the visitors together send more requests than one visitor may, Pisati " +
		"answers them all with status 429, Too Many Requests"
	noProxyFix = "Set server.trusted_proxies to the proxy's address, such as 192.0.2.1, or to the range of " +
		"addresses it sends from, such as 192.0.2.0/24"
)

// warnWithoutProxy logs one warning when srv's base URL is https and it
// trusts no proxy: Pisati serves no https itself, so every visitor comes
// through a proxy that Pisati does not believe.
func warnWithoutProxy(ctx context.Context, srv config.Server, log *slog.Logger) {
	if srv.BaseURL.IsHTTPS() && len(srv.TrustedProxies) == 0 {
		log.WarnContext(ctx, "no trusted proxy is set", "setting", "server.trusted_proxies (PISATI_SERVER_TRUSTED_PROXIES)",
			"effect", noProxyEffect, "fix", noProxyFix)
	}
}

// openServingState opens the state directory dir as openState does, and
// records the lock and the database in l as soon as each is taken, so that
// a halt releases the lock explicitly whatever serve is doing.
func openServingState(ctx context.Context, dir string, l *life) (_ *state, err error) {
	if err := checkStateDir(dir); err != nil {
		return nil, err
	}
	lock, err := lockState(ctx, dir)
	if err != nil {
		return nil, err
	}
	l.opened(lock.Release)
	db, err := store.Open(ctx, store.DatabasePath(dir))
	if err != nil {
		return nil, errors.Join(err, lock.Release())
	}
	l.opened(db.Close)
	st := &state{dir: dir, lock: lock, db: db}
	if _, _, err := db.Migrate(ctx, lock); err != nil {
		return nil, errors.Join(err, st.close())
	}
	return st, nil
}

// sameAddress refuses two listeners on the same port whose hosts, as
// written, are the same, or one of which stands for every address. It
// refuses them before anything is opened, the same on every system,
// though some would let both listen. It looks no host name up, so two
// names of one address are not refused here: the second listener then
// finds the address in use, and listen says so.
func sameAddress(srv config.Server) error {
	public, internal := srv.Listen, srv.InternalListen
	if public.Port() != internal.Port() {
		return nil
	}
	a, b := listenHost(public), listenHost(internal)
	if a == b {
		return fmt.Errorf("%s and %s are both %s. Pisati needs a different address for each. Set one of them to "+
			"another port", publicSetting, internalSetting, public)
	}
	var every []string
	for _, l := range []config.ListenAddr{public, internal} {
		if scope := addressScope(listenHost(l)); scope != "" {
			every = append(every, fmt.Sprintf("%s stands for %s", l, scope))
		}
	}
	switch len(every) {
	case 0:
		return nil
	case 2:
		// The second without its verb: "and [::]:8080 for every address".
		every[1] = strings.Replace(every[1], " stands for ", " for ", 1)
	}
	// The rule, not an overlap: 0.0.0.0 and [::1] share no address, and
	// are refused all the same, the same on every system.
	return fmt.Errorf("%s is %s and %s is %s, both on port %d. %s. When one of the two stands for every address or "+
		"every IPv4 address, Pisati needs them on different ports. Set one of them to another port", publicSetting,
		public, internalSetting, internal, public.Port(), strings.Join(every, ", and "))
}

// addressScope says which addresses of this machine host stands for when
// it names none of them: every address for none, or for [::], which
// listens on IPv4 too, and every IPv4 address for 0.0.0.0. It is "" for a
// host that names an address or a name.
func addressScope(host string) string {
	ip, err := netip.ParseAddr(host)
	switch {
	case host == "", err == nil && ip.IsUnspecified() && ip.Is6():
		return "every address of this machine"
	case err == nil && ip.IsUnspecified():
		return "every IPv4 address of this machine"
	}
	return ""
}

// listenHost returns the host a listens on: "" for none.
func listenHost(a config.ListenAddr) string {
	host, _, err := net.SplitHostPort(a.String())
	if err != nil {
		return ""
	}
	return host
}

// life is what serve has opened so far, so that a halt can close it from
// outside serve's goroutine, whatever serve is doing then, and the
// logger serve logs through, which a halt shuts. Its methods are safe for
// concurrent use.
type life struct {
	mu sync.Mutex
	// log is serve's logger, through gate; direct is the same handler
	// without it, for the halt's own record. Both are nil until serve has
	// read its configuration.
	log, direct *slog.Logger
	shut        *atomic.Bool
	// closers close what serve has opened, in the order it opened it.
	closers  []func() error
	stopOnce sync.Once
}

// logger returns the logger serve logs through, to h, until a halt, after
// which it drops every record.
func (l *life) logger(h slog.Handler) *slog.Logger {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.shut = &atomic.Bool{}
	l.direct = slog.New(h)
	l.log = slog.New(gate{inner: h, shut: l.shut})
	return l.log
}

// opened records close, which closes what serve has just opened, for a
// halt to call. Each close must be safe to call again, and alongside
// serve's own call: serve closes all it opened as well.
func (l *life) opened(close func() error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closers = append(l.closers, close)
}

// stopping logs "stopping", once, as the stop begins: from the first
// signal, or a failure among serve's goroutines, whichever comes first.
// A halt logs none: its own record says why serve stops.
func (l *life) stopping(ctx context.Context) {
	l.stopOnce.Do(func() {
		l.mu.Lock()
		log := l.log
		l.mu.Unlock()
		if log != nil {
			log.InfoContext(ctx, "stopping")
		}
	})
}

// halt stops serve at once, whatever it is doing. It logs its one record,
// and no "stopping" of its own, after which the log takes no record more.
// It then closes everything serve has opened,
// in the reverse order: the servers, cutting their connections, telemetry,
// without waiting for it to send what it holds, the database, and the
// state directory's lock, released explicitly. It returns a *haltError
// that says whether the halt was logged, and holds every failure in
// closing.
func (l *life) halt(ctx context.Context) error {
	l.mu.Lock()
	direct, shut, closers := l.direct, l.shut, slices.Clone(l.closers)
	l.mu.Unlock()
	if direct != nil {
		shut.Store(true)
		direct.ErrorContext(ctx, "stop cut short: a second SIGTERM or SIGINT signal arrived, so Pisati stops at once, "+
			"without waiting for the work it was doing to finish")
	}
	errs := make([]error, 0, len(closers))
	for _, closeIt := range slices.Backward(closers) {
		errs = append(errs, closeIt())
	}
	return &haltError{logged: direct != nil, closing: errors.Join(errs...)}
}

// gate is a slog.Handler that passes each record to inner until shut is
// set, and drops it after.
type gate struct {
	inner slog.Handler
	shut  *atomic.Bool
}

func (g gate) Enabled(ctx context.Context, level slog.Level) bool {
	return !g.shut.Load() && g.inner.Enabled(ctx, level)
}

func (g gate) Handle(ctx context.Context, r slog.Record) error {
	if g.shut.Load() {
		return nil
	}
	return g.inner.Handle(ctx, r)
}

func (g gate) WithAttrs(attrs []slog.Attr) slog.Handler {
	return gate{inner: g.inner.WithAttrs(attrs), shut: g.shut}
}

func (g gate) WithGroup(name string) slog.Handler {
	return gate{inner: g.inner.WithGroup(name), shut: g.shut}
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
	log *slog.Logger
	tel telemetry.Providers
	// telStopping is taken by whichever stops tel first: the flush, or a
	// halt.
	telStopping          *atomic.Bool
	public, internal     *http.Server
	publicLn, internalLn net.Listener
	worker               *jobs.Worker
	sessions             *app.Sessions
}

// start makes everything serve runs, in the database st has open: the
// keyring, the job worker, with the leases a stopped run left released,
// the sessions, telemetry, both listeners' handlers, and, last, the two
// listeners. On an error it leaves nothing open but st.
func start(ctx context.Context, h Host, cfg config.Config, creds fs.FS, st *state, log *slog.Logger, l *life) (_ *running, err error) {
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
	// Whichever stops telemetry first, the flush or a halt, takes this:
	// Shutdown runs once, and a second call waits for the first, which a
	// halt must not.
	telStopping := &atomic.Bool{}
	l.opened(func() error {
		if !telStopping.CompareAndSwap(false, true) {
			return nil
		}
		// An ended context: Shutdown stops both providers at once,
		// without waiting for an export.
		now, cancel := context.WithCancel(context.WithoutCancel(ctx))
		cancel()
		if err := tel.Shutdown(now); !errors.Is(err, context.Canceled) {
			return err
		}
		return nil
	})
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
		log:         log,
		tel:         tel,
		telStopping: telStopping,
		public:      httpx.NewServer(cfg.Server.Listen, public, httpx.DefaultTimeouts(), log),
		internal:    httpx.NewServer(cfg.Server.InternalListen, internal, httpx.DefaultTimeouts(), log),
		publicLn:    publicLn,
		internalLn:  internalLn,
		worker:      worker,
		sessions:    sessions,
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
		public, err = listen(ctx, &lc, srv.Listen, publicSetting, srv.InternalListen, internalSetting)
		return err
	})
	both.Go(func() (err error) {
		internal, err = listen(ctx, &lc, srv.InternalListen, internalSetting, srv.Listen, publicSetting)
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
// other is the other listener's address, which the setting otherSetting
// gives: on the same port, it may hold addr itself, under another name.
func listen(ctx context.Context, lc *net.ListenConfig, addr config.ListenAddr, setting string,
	other config.ListenAddr, otherSetting string,
) (net.Listener, error) {
	ln, err := lc.Listen(ctx, "tcp", addr.String())
	switch {
	case errors.Is(err, syscall.EADDRINUSE) && other.Port() == addr.Port():
		// Pisati's own listener for other may hold addr under another name,
		// which no lookup at start would show: the advice holds either way.
		return nil, explain.New(err, "Pisati cannot listen on %[1]s, the address that %[2]s sets, because that address is in "+
			"use already. %[4]s is %[3]s, on the same port, and Pisati's own listener there may be what uses it. Set "+
			"one of the two settings to another port. If Pisati still cannot listen on %[1]s after that change, "+
			"another process uses it: stop that process, or set %[2]s to another address", addr, setting, other,
			otherSetting)
	case errors.Is(err, syscall.EADDRINUSE):
		return nil, explain.New(err, "Pisati cannot listen on %s, the address that %s sets, because another process already "+
			"uses it. Stop that process, or set %s to another address", addr, setting, setting)
	case errors.Is(err, syscall.EACCES):
		return nil, explain.New(err, "Pisati cannot listen on %s, the address that %s sets, because the operating "+
			"system does not let the user that runs Pisati listen there. Set %s to an address and port that this user "+
			"may listen on. On most systems, only privileged users may listen on a port below 1024, and a security "+
			"policy such as SELinux or AppArmor can refuse others", addr, setting, setting)
	case errors.Is(err, syscall.EADDRNOTAVAIL):
		return nil, explain.New(err, "Pisati cannot listen on %s, the address that %s sets, because this machine has "+
			"no such address. Set %s to an address of this machine, or leave its host out, as in :%d, to listen on "+
			"every address", addr, setting, setting, addr.Port())
	case err != nil:
		return nil, explain.New(err, "Pisati cannot listen on %s, the address that %s sets: %s. Check that the host in "+
			"%s is a name or an address of this machine", addr, setting, reason(err), setting)
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

// healthz answers 200 ok while Pisati reads its database from disk within
// checkWithin (store.Check), and otherwise 503 unavailable, logging why:
// the reason goes to the log alone, never into the answer.
func healthz(db *store.Store, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), checkWithin)
		defer cancel()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		status, body := http.StatusOK, "ok"
		if err := db.Check(ctx); err != nil {
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
// It records the servers in l, and the end of the group's work, for a halt
// to close: the halt does not wait for the job worker, whose running jobs
// the next start's ReleaseOrphans makes ready again.
//
// It returns nil once stopped, even when the deadline cut work off, and
// every error of a listener that failed, joined.
func (r *running) serve(ctx context.Context, h Host, l *life) error {
	// Ended by a halt too, so that the worker and the sweeper stop taking
	// work then, even had no signal ended ctx first.
	serving, stopServing := context.WithCancel(ctx)
	defer stopServing()
	l.opened(r.public.Close)
	l.opened(r.internal.Close)
	l.opened(func() error {
		stopServing()
		return nil
	})
	g, gctx := newGroup(serving)
	stopAt := sync.OnceValue(func() time.Time {
		l.stopping(ctx)
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

	err := g.Wait()
	if !r.telStopping.CompareAndSwap(false, true) {
		return err // a halt stopped it
	}
	deadline, cancel := context.WithDeadline(context.WithoutCancel(ctx), stopAt())
	defer cancel()
	if ferr := r.tel.Shutdown(deadline); ferr != nil {
		r.log.WarnContext(deadline, "telemetry's traces and metrics not both shut down, so traces still held may not have "+
			"reached the collector that telemetry.otlp_endpoint names", "error", ferr)
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
	return explain.New(err, "Pisati stopped serving on %s, the address that %s sets, because accepting connections there failed: %s",
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
