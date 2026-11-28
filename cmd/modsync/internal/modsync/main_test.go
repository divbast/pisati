// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package modsync_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain fails the package when its tests leave a goroutine running:
// each go command modsync starts, and the goroutines os/exec copies its
// output with, must end before Run returns.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
