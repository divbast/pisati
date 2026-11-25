// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

// Command pisati is Pisati: it serves the blog, and manages its database
// and its keys.
//
// Usage:
//
//	pisati serve                                 serve until stopped
//	pisati migrate                               update the database's schema
//	pisati backup <dest>                         copy the database to a new file
//	pisati restore <src>                         replace the database with a copy
//	pisati keys rewrap --new-main-key-file=PATH  encrypt the data keys with a new main key
//	pisati keys rotate                           make a new data key
//	pisati health                                check the running serve
//	pisati version                               print the version
//
// Every command but version reads the whole configuration first, as serve
// does, and refuses to run when serve would refuse to start. serve logs to
// stdout; the other commands log to stderr, so that stdout carries only a
// command's result. The errors that end a command are reported on stderr,
// one line for each problem, each starting "pisati: ".
//
// serve stops on SIGTERM or SIGINT: it accepts no new work, gives what is
// running one deadline, 8 s, to finish, and exits 0, even when that
// deadline cut work off. A second SIGTERM or SIGINT before then stops it
// at once, with exit 1.
//
// Exit codes:
//
//	0  done, healthy, or stopped by one signal
//	1  refused or failed, with the reasons on stderr, or stopped at once
//	   by a second signal
//	2  the command line is wrong
package main

import (
	"context"
	"crypto/rand"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// version is set when the binary is built, with -ldflags "-X
// main.version=…".
var version = "dev"

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	halt := make(chan struct{})
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	// It lives as long as the process: the first signal ends run's
	// context, and the second closes halt, which stops serve at once.
	go func() {
		<-signals
		cancel()
		<-signals
		close(halt)
	}()
	code := run(ctx, Host{
		Args:     os.Args[1:],
		Lookup:   os.LookupEnv,
		EnvNames: envNames(os.Environ()),
		Clock:    time.Now,
		Rand:     rand.Reader,
		Stdout:   os.Stdout,
		Stderr:   os.Stderr,
		Halt:     halt,
	})
	cancel()
	os.Exit(code)
}

// envNames returns the name of each NAME=value in environ.
func envNames(environ []string) []string {
	names := make([]string, 0, len(environ))
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		names = append(names, name)
	}
	return names
}
