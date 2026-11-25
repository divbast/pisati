// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package acceptance_test

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os/exec"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/divbast/ci/harness"
)

// What Pisati needs to serve, and where it keeps what it writes.
const (
	// stateDir is the state directory: the database, its lock and the
	// main key Pisati generates. The image holds it empty, as Pisati's
	// user's alone, so that a volume mounted there starts out the same.
	stateDir = "/var/lib/pisati"
	// baseURLVar is the variable of server.base_url, which every command
	// but version needs, and which has no default.
	baseURLVar = "PISATI_SERVER_BASE_URL"
	// baseURL is the site's address in every container here, as the
	// contract's Env gives it.
	baseURL = "http://localhost:8080"
)

// pisatiEnv returns the environment every serving container here runs
// with: the base URL, and nothing else.
func pisatiEnv() map[string]string {
	return map[string]string{baseURLVar: baseURL}
}

// stateMount makes a volume, which tb removes when it ends, and returns
// its mount at stateDir.
func stateMount(tb testing.TB, e harness.Engine) harness.Mount {
	tb.Helper()
	vol, err := e.Volume(tb.Context(), tb, "")
	if err != nil {
		tb.Fatal(err)
	}
	return harness.Mount{Source: vol, Target: stateDir, Volume: true}
}

// serving returns the spec of a container that serves image under the
// contract, with the base URL and state, a volume at stateDir.
func serving(image string, state harness.Mount) harness.RunSpec {
	return harness.RunSpec{
		Image:   image,
		Confine: harness.Contract(tmpfsSize),
		Env:     pisatiEnv(),
		Mounts:  []harness.Mount{state},
	}
}

// refuseWithin bounds a run that should refuse to start, so that a serve
// which starts instead fails the test rather than running until go test's
// own timeout.
const refuseWithin = harness.ServeWithin

// runToExit runs spec in the foreground, as harness.Engine.Run does, and
// returns its stdout, stderr and exit status, or an error when it could
// not be run or did not exit within refuseWithin.
func runToExit(ctx context.Context, tb testing.TB, e harness.Engine, spec *harness.RunSpec) (harness.Result, error) {
	tb.Helper()
	ctx, cancel := context.WithTimeout(ctx, refuseWithin)
	defer cancel()
	res, err := e.Run(ctx, tb, spec)
	if err != nil && ctx.Err() != nil {
		return res, fmt.Errorf("the container did not exit within %s: %w", refuseWithin, err)
	}
	return res, err
}

// stop stops c as a user does, with `<engine> stop`, and returns what the
// engine printed and how long it took.
func stop(ctx context.Context, e harness.Engine, c *harness.Container) (out []byte, took time.Duration, err error) {
	start := time.Now()
	out, err = exec.CommandContext(ctx, e.Name(), "stop", c.ID).CombinedOutput() //nolint:gosec // G204: the engine and the container are the harness's own
	return out, time.Since(start), err
}

// copied is one entry of what `<engine> cp` copies out of a container:
// its mode, and a regular file's content.
type copied struct {
	mode fs.FileMode
	data []byte
}

// maxCopied bounds what copyOut reads.
const maxCopied = 64 << 20

// copyOut copies src, a path in c, out of c with `<engine> cp`, which
// reads a volume mounted in c too, and returns every entry it copied,
// keyed by its path below src: "." for src itself. A file's mode is the
// one it has in c. c may be running or stopped.
func copyOut(ctx context.Context, e harness.Engine, c *harness.Container, src string) (map[string]copied, error) {
	cmd := exec.CommandContext(ctx, e.Name(), "cp", c.ID+":"+src, "-") //nolint:gosec // G204: the engine and the container are the harness's own, and src a constant here
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	entries, readErr := readTar(io.LimitReader(stdout, maxCopied+1), path.Base(src))
	if readErr != nil {
		// What is left unread would hold cp up.
		//nolint:errcheck // the copy has failed already, and draining what is left only lets cp exit
		io.Copy(io.Discard, stdout)
	}
	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("%s cp %s:%s: %w: %s", e.Name(), c, src, err, strings.TrimSpace(stderr.String()))
	}
	return entries, readErr
}

// readTar reads the tar stream r, as `<engine> cp` writes it, whose
// entries are under base, src's own name.
func readTar(r io.Reader, base string) (map[string]copied, error) {
	entries := make(map[string]copied)
	tr := tar.NewReader(r)
	total := 0
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return entries, nil
		}
		if err != nil {
			return nil, fmt.Errorf("reading what cp copied: %w", err)
		}
		name := path.Clean(strings.TrimPrefix(hdr.Name, "./"))
		rel, ok := strings.CutPrefix(name, base)
		if !ok || (rel != "" && !strings.HasPrefix(rel, "/")) {
			return nil, fmt.Errorf("cp copied %q, which is not under %q", hdr.Name, base)
		}
		rel = strings.TrimPrefix(rel, "/")
		if rel == "" {
			rel = "."
		}
		var data []byte
		if hdr.Typeflag == tar.TypeReg {
			data, err = io.ReadAll(tr)
			if err != nil {
				return nil, fmt.Errorf("reading %s from what cp copied: %w", rel, err)
			}
		}
		total += len(data)
		if total > maxCopied {
			return nil, fmt.Errorf("cp copied more than %d bytes", maxCopied)
		}
		entries[rel] = copied{mode: hdr.FileInfo().Mode(), data: data}
	}
}
