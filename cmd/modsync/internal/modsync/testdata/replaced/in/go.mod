// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

module example.com/root

go 1.27.0

require go.uber.org/goleak v1.3.0

require golang.org/x/sys v0.50.0 // indirect

replace golang.org/x/sys => example.com/sysfork v0.1.0
