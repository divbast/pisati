// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

// The theme toggle: with the script, it changes the theme where the page
// stands; without it, the form posts and comes back to the same page.
// Either way the choice is remembered, and Auto follows the system.

import type { BrowserContext, Page } from "@playwright/test";

import { expect, settle, test, themeCookie } from "./fixtures.ts";

const themeNames = { light: "Light", dark: "Dark", auto: "Auto" } as const;

type Theme = keyof typeof themeNames;

/** Fails unless page shows theme, with only its button pressed. */
async function expectTheme(page: Page, theme: Theme): Promise<void> {
  await expect(page.locator("html")).toHaveAttribute("data-theme", theme);
  await Promise.all(
    Object.entries(themeNames).map(async ([other, name]: readonly [string, string]) => {
      await expect(page.getByRole("button", { name })).toHaveAttribute(
        "aria-pressed",
        String(other === theme),
      );
    }),
  );
}

/** The theme cookie context holds, or none. */
async function cookieOf(context: BrowserContext): Promise<string | undefined> {
  const cookies = await context.cookies();
  return cookies.find(({ name }) => name === themeCookie)?.value;
}

/** The page's background colour, once nothing on it moves. */
async function canvasOf(page: Page): Promise<string> {
  await settle(page);
  return page.evaluate(() => getComputedStyle(document.documentElement).backgroundColor);
}

test("the theme buttons change the theme where the page stands", async ({ page, context }) => {
  await page.goto("/login");
  await expectTheme(page, "auto");
  // A mark the page loses if it is loaded again.
  await page.locator("main").evaluate((main) => {
    main.dataset["stays"] = "";
  });
  const before = await canvasOf(page);

  const posted = page.waitForResponse(
    (response) => new URL(response.url()).pathname === "/_/theme",
  );
  await page.getByRole("button", { name: "Dark" }).click();
  expect((await posted).request().method()).toBe("POST");
  await expectTheme(page, "dark");
  await expect(page.locator("main")).toHaveAttribute("data-stays", "");
  await expect(page).toHaveURL("/login");
  expect(await canvasOf(page)).not.toBe(before);
  expect(await cookieOf(context)).toBe("dark");

  // The server remembers the choice.
  await page.reload();
  await expectTheme(page, "dark");
});

test.describe("without the script", () => {
  test.use({ javaScriptEnabled: false });

  test("the theme form posts and comes back to the same page", async ({ page, context }) => {
    await page.goto("/login");
    await expectTheme(page, "auto");
    // The page the post is sent on to, which is where the reader lands.
    const landed = page.waitForResponse((response) => response.request().redirectedFrom() !== null);
    await page.getByRole("button", { name: "Light" }).click();
    const back = await landed;
    expect(back.request().redirectedFrom()?.method()).toBe("POST");
    expect(new URL(back.url()).pathname).toBe("/login");
    await page.waitForLoadState();
    await expect(page).toHaveURL("/login");
    await expectTheme(page, "light");
    expect(await cookieOf(context)).toBe("light");
  });
});

test.describe("Auto", () => {
  for (const scheme of ["light", "dark"] as const) {
    test(`follows a ${scheme} system`, async ({ page }) => {
      await page.emulateMedia({ colorScheme: scheme });
      await page.goto("/");
      await expectTheme(page, "auto");
      const auto = await canvasOf(page);

      await page.getByRole("button", { name: themeNames[scheme] }).click();
      await expectTheme(page, scheme);
      expect(await canvasOf(page)).toBe(auto);
    });
  }
});
