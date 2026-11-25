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
// is a "config: " line for each problem; anything else, one "healthz: "
// line.
func (healthCmd) Run(ctx context.Context, h Host) error {
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
	if why := probe(ctx, cfg.Server.InternalListen); why != "" {
		say(h.Stderr, "healthz: %s\n", why)
		return errReported
	}
	return nil
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
// Each reason names the address called, and the address a gives when
// that is another.
func probe(ctx context.Context, a config.ListenAddr) string {
	addr := healthAddr(a)
	where := fmt.Sprintf("%s, the address that %s sets", addr, internalSetting)
	if set := a.String(); set != addr {
		where = fmt.Sprintf("%s, for the address %s that %s sets", addr, set, internalSetting)
	}
	ctx, cancel := context.WithTimeout(ctx, healthWithin)
	defer cancel()
	target := url.URL{Scheme: "http", Host: addr, Path: "/healthz"}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), http.NoBody)
	if err != nil {
		return fmt.Sprintf("cannot make the request GET /healthz to %s: %s.", addr, reason(err))
	}
	// A transport of its own: no proxy, whatever the environment says,
	// and no connection kept open after the one request.
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Sprintf("%s, did not answer GET /healthz within %d seconds.", where, int(healthWithin/time.Second))
	case errors.Is(err, syscall.ECONNREFUSED):
		return fmt.Sprintf("nothing accepts connections on %s. Check that pisati serve is running.", where)
	case err != nil:
		return fmt.Sprintf("the request GET /healthz to %s, did not complete: %s.", where, reason(err))
	}
	status := resp.StatusCode
	//nolint:errcheck // the body is never read: closing it frees the connection, and its error changes nothing about the answer
	resp.Body.Close()
	switch status {
	case http.StatusOK:
		return ""
	case http.StatusServiceUnavailable:
		return fmt.Sprintf("%s, answered GET /healthz with status 503, not 200: pisati serve runs, but its database "+
			"did not answer. The log of pisati serve says why.", where)
	default:
		return fmt.Sprintf("%s, answered GET /healthz with status %d, not 200. Check that it is the internal "+
			"address of pisati serve.", where, status)
	}
}
