// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

// Forced colours, as Chromium emulates them: the system's colours paint
// every button and field alike, so the pressed theme button and a field
// left empty must stand out by their borders instead.

import type { Locator } from "@playwright/test";

import { expect, settle, test } from "./fixtures.ts";

test.use({ forcedColors: "active" });

/** The width of item's block-start border, in CSS pixels. */
async function borderWidthOf(item: Locator): Promise<number> {
  const width = await item.evaluate((element) => getComputedStyle(element).borderBlockStartWidth);
  expect(width).toMatch(/^[\d.]+px$/u);
  return Number(width.replace(/px$/u, ""));
}

test("the sign-in page marks what colour cannot", async ({ page, browserName }) => {
  test.skip(browserName !== "chromium", "forced colours are checked under Chromium's emulation");
  await page.goto("/login");
  await settle(page);
  expect(await page.evaluate(() => matchMedia("(forced-colors: active)").matches)).toBe(true);

  // The pressed button, Auto, has a thicker border than the others.
  const auto = await borderWidthOf(page.getByRole("button", { name: "Auto" }));
  const light = await borderWidthOf(page.getByRole("button", { name: "Light" }));
  expect(auto).toBeGreaterThan(light);

  // A field the reader has typed into and emptied again is invalid, as
  // it is after a try to send the form, each with a dashed border three
  // times the width of a valid one's. Leaving each field, rather than
  // sending the form, keeps the browser's own message out of the image.
  const username = page.getByLabel("Username");
  const password = page.getByLabel("Password");
  const valid = await borderWidthOf(username);
  await username.fill("reader");
  await username.fill("");
  await password.fill("a passphrase");
  await password.fill("");
  await password.blur();
  await settle(page);
  await Promise.all(
    [username, password].map(async (field) => {
      await expect(field).toHaveCSS("border-block-start-style", "dashed");
      expect(await borderWidthOf(field)).toBeCloseTo(valid * 3);
    }),
  );
  await expect(page).toHaveScreenshot("sign-in-forced-colors.png", { fullPage: true });
});
