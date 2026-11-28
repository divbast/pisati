// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

module example.com/root/test/acceptance

go 1.27.0

require (
	github.com/divbast/ci v0.0.0-00010101000000-000000000000
	go.uber.org/goleak v1.3.1
)

require (
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/text v0.43.0 // indirect
)

replace github.com/divbast/ci => ../../.ci
