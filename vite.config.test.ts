// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

import { expect, it } from "vite-plus/test";

import { scriptTargets } from "./vite.config.ts";

it("targets the oldest version of each engine, comparing versions as numbers", () => {
  expect(
    scriptTargets([
      "chrome 157",
      "chrome 132",
      "and_chr 157",
      "safari 17.10",
      "safari 17.4",
      "ios_saf 18.5-18.6",
      "ios_saf 18.2",
      "opera 115",
      "samsung 28",
      "safari TP",
      "op_mini all",
    ]),
  ).toEqual(["chrome132", "ios18.2", "opera115", "safari17.4", "samsung28"]);
});
