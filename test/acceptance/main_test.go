// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

// Package acceptance_test runs the acceptance suite against the image IMAGE
// names, under the engine ENGINE names, docker unless it says podman.
// Every container it starts runs under exactly the confinement the image's
// contract claims, through divbast's harness, so no check can pass under a
// looser one. With IMAGE unset there is nothing to test, and every test
// skips.
package acceptance_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/divbast/ci/harness"
	"go.uber.org/goleak"
)

// sweepWithin bounds the sweep of what an earlier run left behind.
const sweepWithin = 2 * time.Minute

// TestMain removes every container, network and volume a run that died
// left behind, before any test starts its own. Once the tests are done, it
// fails the run if they left a goroutine running, then removes the probe
// volumes this run made. Without an image there is nothing to sweep for,
// and no engine is asked.
func TestMain(m *testing.M) {
	if os.Getenv("IMAGE") == "" {
		goleak.VerifyTestMain(m)
		return
	}
	if err := sweep(); err != nil {
		fmt.Fprintln(os.Stderr, "acceptance:", err)
		os.Exit(1)
	}
	goleak.VerifyTestMain(m, goleak.Cleanup(func(code int) {
		if err := removeProbeVolumes(); err != nil {
			fmt.Fprintln(os.Stderr, "acceptance:", err)
			code = max(code, 1)
		}
		os.Exit(code)
	}))
}

// removeProbeVolumes is [harness.Engine.RemoveProbeVolumes] under the
// engine $ENGINE names.
func removeProbeVolumes() error {
	e, err := harness.EngineFromEnv()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), sweepWithin)
	defer cancel()
	return e.RemoveProbeVolumes(ctx)
}

// sweep is [harness.Engine.Sweep] under the engine $ENGINE names.
func sweep() error {
	e, err := harness.EngineFromEnv()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), sweepWithin)
	defer cancel()
	if err := e.Sweep(ctx); err != nil {
		return fmt.Errorf("removing what an earlier run left behind: %w", err)
	}
	return nil
}

// underTest returns the engine $ENGINE names and the image $IMAGE names,
// and skips t when IMAGE is unset, before either is needed: the engine
// defaults to docker, and the contract fails rather than skips with no
// image.
func underTest(t *testing.T) (e harness.Engine, image string) {
	t.Helper()
	image = os.Getenv("IMAGE")
	if image == "" {
		t.Skip("IMAGE is unset, so there is no image to test: just acceptance sets it, and ENGINE, to the image just build wrote")
	}
	e, err := harness.EngineFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	return e, image
}
