// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package explain_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain fails the package when its tests leave a goroutine running.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
