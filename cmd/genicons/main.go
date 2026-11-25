// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

// Command genicons writes internal/web/icons/icons.go: the Lucide icons
// Pisati's pages show, as inline SVG, read from the lucide-static package
// pnpm installs, by package icongen. Run it from the repository's root,
// as `just generate` does:
//
//	go run ./cmd/genicons
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"github.com/divbast/pisati/cmd/genicons/internal/icongen"
)

const (
	// lucideDir is where pnpm installs lucide-static, from the
	// repository's root.
	lucideDir = "node_modules/lucide-static"
	// outFile is the file genicons writes, from the repository's root.
	outFile = "internal/web/icons/icons.go"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	err := icongen.Generate(ctx, os.DirFS(lucideDir), icongen.Icons(), outFile)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "genicons:", err)
		os.Exit(1)
	}
}
