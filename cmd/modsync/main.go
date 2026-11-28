// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

// Command modsync keeps test/acceptance, the acceptance suite's module, in
// step with the repository's root module, by package modsync: for every
// module both require, test/acceptance gets the root's version and the
// root go.sum's lines for it. Run it from the repository's root:
//
//	go run ./cmd/modsync          bring test/acceptance in step
//	go run ./cmd/modsync -check   write nothing, and exit 1 naming each difference
//
// Renovate runs the first after each Go update of the root module, and
// just lint runs the second.
//
// Exit codes:
//
//	0  in step, or brought in step
//	1  out of step, under -check, or failed, with the reasons on stderr
//	2  the command line is wrong
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/divbast/pisati/cmd/modsync/internal/modsync"
)

// goTimeout bounds each go command modsync runs. go mod edit reads and
// writes one file, and takes milliseconds.
const goTimeout = time.Minute

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx)
	stop()
	os.Exit(code)
}

// run runs modsync's command line in the working directory, which is the
// repository's root, with the go command PATH names, found once here.
func run(ctx context.Context) int {
	goPath, err := exec.LookPath("go")
	if err != nil {
		fmt.Fprintln(os.Stderr, "modsync:", err)
		return 1
	}
	goCmd, err := modsync.NewGo(goPath, goTimeout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "modsync:", err)
		return 1
	}
	repo, err := os.OpenRoot(".")
	if err != nil {
		fmt.Fprintln(os.Stderr, "modsync:", err)
		return 1
	}
	code := modsync.Run(ctx, os.Args[1:], repo, goCmd, os.Stderr)
	if err := repo.Close(); err != nil {
		fmt.Fprintln(os.Stderr, "modsync:", err)
		return 1
	}
	return code
}
