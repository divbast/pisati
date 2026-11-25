// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package acceptance_test

import (
	"net/http"
	"testing"

	"github.com/divbast/ci/harness"
)

// promType starts each metric's TYPE line in Prometheus' text format.
const promType = "# TYPE "

// TestMetricsOnlyInternal checks /metrics is served on the internal port
// alone: there it answers 200 in Prometheus' text format, once the site
// has had a request to count, and the app port answers it with its 404
// page. The profiler is off, as it is unless configured, so the internal
// port answers its path with an empty 404, as it does every path but
// /healthz and /metrics.
func TestMetricsOnlyInternal(t *testing.T) {
	t.Parallel()
	e, image := underTest(t)
	ctx := t.Context()

	s, err := e.Serve(ctx, t, &harness.ServeSpec{
		Run:        serving(image, stateMount(t, e)),
		AppPort:    appPort,
		HealthPort: healthPort,
	})
	checks := harness.NewChecks(t)
	if err != nil {
		checks.True("the container serves", false, err.Error())
		return
	}
	internal := s.Health()

	harness.StatusIs(t, s, "GET / on the app port answers 200", http.StatusOK, "/")
	resp, err := internal.Get(ctx, "/metrics", nil)
	harness.Answers(t, "GET /metrics on the internal port answers 200", http.StatusOK, resp, err)
	if err == nil {
		harness.Contains(t, "GET /metrics on the internal port is in Prometheus' text format", promType, string(resp.Body))
	}

	resp, err = s.Get(ctx, "/metrics", nil)
	harness.Answers(t, "GET /metrics on the app port answers 404", http.StatusNotFound, resp, err)
	if err == nil {
		isPage(checks, "GET /metrics on the app port", resp)
		harness.Lacks(t, "GET /metrics on the app port holds no metric", promType, string(resp.Body))
	}

	resp, err = internal.Get(ctx, "/debug/pprof/", nil)
	harness.Answers(t, "GET /debug/pprof/ on the internal port answers 404", http.StatusNotFound, resp, err)
	if err == nil {
		harness.Equal(checks, "GET /debug/pprof/ on the internal port has an empty body", "", string(resp.Body))
	}
}
