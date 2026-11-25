// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"syscall"
	"time"

	"github.com/divbast/pisati/internal/config"
)

// healthWithin bounds pisati health's one request: an engine's health
// check gives the whole probe 10 s.
const healthWithin = 4 * time.Second

// healthCmd is pisati health.
type healthCmd struct{}

// Run asks the running serve's internal listener for /healthz. It prints
// nothing when the answer is 200. Otherwise it prints why, as lines of
// "<check>: <reason>", and exits 1: with no shell in the image, an
// engine's health log is the only record of why. A refused configuration
// is a "config: " line for each problem; anything else, a stop by a signal
// among it, one "healthz: " line.
func (healthCmd) Run(ctx context.Context, h Host) error {
	err := untilHalt(h, func() error {
		cfg, _, err := loadConfig(ctx, h)
		if ce, ok := errors.AsType[*configError](err); ok {
			for _, line := range problems(ce) {
				say(h.Stderr, "config: %s\n", line)
			}
			return errReported
		}
		if err != nil {
			return err
		}
		why, err := probe(ctx, cfg.Server.InternalListen)
		if err != nil {
			return err
		}
		if why != "" {
			say(h.Stderr, "healthz: %s\n", why)
			return errReported
		}
		return nil
	})
	switch {
	case errors.Is(err, errHalted):
		say(h.Stderr, "healthz: pisati health stopped at once, before it finished, because a second SIGTERM or SIGINT "+
			"signal arrived.\n")
		return errReported
	case stopped(ctx, err):
		say(h.Stderr, "healthz: pisati health stopped before it finished, because a SIGTERM or SIGINT signal arrived.\n")
		return errReported
	}
	return err
}

// healthAddr returns the address pisati health calls for the internal
// listener a: the host a names, or 127.0.0.1 when a names none or one
// that stands for every address, as in :8081, 0.0.0.0:8081 or [::]:8081.
func healthAddr(a config.ListenAddr) string {
	host, _, err := net.SplitHostPort(a.String())
	if ip, perr := netip.ParseAddr(host); err != nil || host == "" || (perr == nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, strconv.Itoa(int(a.Port())))
}

// probe sends GET /healthz to the internal listener a within
// healthWithin, and returns why the answer is not 200, or "" when it is.
// Each reason names the address called, and ends with a sentence on where
// that address comes from. A stop of ctx is the one failure it leaves to
// the caller: it then returns ctx's error, with no reason.
func probe(ctx context.Context, a config.ListenAddr) (string, error) {
	addr := healthAddr(a)
	about := fmt.Sprintf(" %s is the address that %s sets.", addr, internalSetting)
	if scope := addressScope(listenHost(a)); scope != "" {
		about = fmt.Sprintf(" %s is %s, which stands for %s, so pisati health calls %s.", internalSetting, a, scope, addr)
	}
	stop := ctx
	ctx, cancel := context.WithTimeout(ctx, healthWithin)
	defer cancel()
	target := url.URL{Scheme: "http", Host: addr, Path: "/healthz"}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), http.NoBody)
	if err != nil {
		return fmt.Sprintf("pisati health cannot make the request GET /healthz to %s: %s.%s", addr, reason(err), about), nil
	}
	// A transport of its own: no proxy, whatever the environment says,
	// and no connection kept open after the one request.
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	switch {
	case err != nil && stop.Err() != nil:
		return "", stop.Err()
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Sprintf("%s did not answer GET /healthz within %d seconds.%s", addr, int(healthWithin/time.Second), about), nil
	case errors.Is(err, syscall.ECONNREFUSED):
		return fmt.Sprintf("nothing accepts connections on %s. Check that pisati serve is running.%s", addr, about), nil
	case err != nil:
		return fmt.Sprintf("the request GET /healthz to %s did not complete: %s.%s", addr, reason(err), about), nil
	}
	status := resp.StatusCode
	//nolint:errcheck // the body is never read: closing it frees the connection, and its error changes nothing about the answer
	resp.Body.Close()
	switch status {
	case http.StatusOK:
		return "", nil
	case http.StatusServiceUnavailable:
		return fmt.Sprintf("%s answered GET /healthz with status 503, not 200: pisati serve runs, but its database "+
			"did not answer. The log of pisati serve says why.%s", addr, about), nil
	default:
		return fmt.Sprintf("%[1]s answered GET /healthz with status %[2]d, not 200. pisati serve answers 200 there when it "+
			"is healthy, and 503 when its database does not answer, so another process may be listening at %[1]s.%[3]s",
			addr, status, about), nil
	}
}
