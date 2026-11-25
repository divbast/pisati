// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

// The browser suite, which just browser runs: every page type in
// Chromium, Firefox and WebKit, each run natively on the machine it is
// on, against a Pisati of each test's own (fixtures.ts).

import { defineConfig, devices } from "@playwright/test";

import type { Options } from "./fixtures.ts";

export default defineConfig<Options>({
  testDir: ".",
  testMatch: "*.spec.ts",
  // One baseline set for each platform, approved apart: each image is
  // named for its page, its browser and the platform it was taken on.
  snapshotPathTemplate: "{testDir}/baselines/{arg}-{projectName}-{platform}{ext}",
  fullyParallel: true,
  forbidOnly: true,
  // A test that passes only on a second try has failed once, and says so.
  retries: 0,
  reporter: "list",
  use: {
    pisatiBinary: process.env["BROWSER_PISATI"] ?? "",
  },
  projects: [
    { name: "chromium", use: { ...devices["Desktop Chrome"] } },
    { name: "firefox", use: { ...devices["Desktop Firefox"] } },
    { name: "webkit", use: { ...devices["Desktop Safari"] } },
  ],
});
