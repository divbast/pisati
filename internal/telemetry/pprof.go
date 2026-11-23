// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package telemetry

import (
	"net/http"
	// Importing it registers its handlers on http.DefaultServeMux, in its
	// init. That is harmless: Pisati never serves DefaultServeMux, as every
	// server it runs is given a handler of its own.
	"net/http/pprof"
	"strconv"

	"github.com/divbast/pisati/internal/config"
)

// Pprof mounts net/http/pprof's handlers on mux under /debug/pprof/ when s
// is config.On: for GET, as that package mounts them, and for POST too on
// /debug/pprof/symbol, to which go tool pprof posts the addresses it wants
// named. When s is config.Off it mounts nothing, and mux answers every
// path under /debug/pprof/ as it answers any path it does not serve. It
// never uses http.DefaultServeMux.
//
// A nil mux, or a Switch outside config's, is a bug, and Pprof panics on
// it.
func Pprof(mux *http.ServeMux, s config.Switch) {
	if mux == nil {
		panic("telemetry: Pprof needs a mux")
	}
	switch s {
	case config.Off:
		return
	case config.On:
		mux.HandleFunc("GET /debug/pprof/", pprof.Index)
		mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
		// GET and POST, the two methods Symbol reads, and not every method:
		// a symbol pattern for every method would conflict with the GET
		// index pattern, which the mux refuses.
		mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("POST /debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)
		return
	}
	panic("telemetry: Pprof given an unknown switch, " + strconv.Itoa(int(s)))
}
