// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

module example.com/root/test/acceptance

go 1.27.0

require (
	github.com/divbast/ci v0.0.0-00010101000000-000000000000
	go.uber.org/goleak v1.3.0
)

require golang.org/x/sys v0.50.0 // indirect

replace github.com/divbast/ci => ../../.ci

replace go.uber.org/goleak v1.3.0 => example.com/goleakfork v1.0.0
