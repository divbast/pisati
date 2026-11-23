// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package httpx

import (
	"net/http"

	"github.com/divbast/pisati/internal/config"
)

// CrossOrigin refuses a browser's request from another origin by any
// method but GET, HEAD or OPTIONS: one its Sec-Fetch-Site says is
// cross-site or same-site, or, from a browser too old to send that, one
// whose Origin is not its Host. It is http.CrossOriginProtection, with
// base's origin trusted too, since a reverse proxy may pass another Host.
// A refused request gets 403 with a generic body, and never reaches the
// handler. A request with neither header comes from outside a browser,
// and passes.
//
// The zero BaseURL is a bug, since config.Load never returns one, and
// CrossOrigin panics on it.
func CrossOrigin(base config.BaseURL) Middleware {
	p := http.NewCrossOriginProtection()
	// Every BaseURL but the zero one is an origin, which this accepts.
	if err := p.AddTrustedOrigin(base.String()); err != nil {
		panic("httpx: CrossOrigin needs a base URL: " + err.Error())
	}
	p.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
	}))
	return p.Handler
}
