// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package main

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain fails the package when its tests leave a goroutine running:
// every command, serve among them, must stop all it started before run
// returns.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
