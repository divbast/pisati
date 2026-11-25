// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

// Every page type, in each theme a reader can pick, on a desktop screen
// and a phone's: the answer's status, the page's heading, no axe-core
// violation, and the page as its platform's baseline shows it.

import type { Page } from "@playwright/test";

import { expectNoAxeViolations, expect, test, themeCookie } from "./fixtures.ts";

/** A page a reader can reach by its address. */
interface Visited {
  /** The page's name in its baselines. */
  readonly name: string;
  readonly path: string;
  readonly status: number;
  readonly heading: string;
}

const visited: readonly Visited[] = [
  { name: "home", path: "/", status: 200, heading: "Pisati" },
  { name: "sign-in", path: "/login", status: 200, heading: "Sign in" },
  { name: "not-found", path: "/no-page-here", status: 404, heading: "Page not found" },
];

/** The themes a reader can pick whose colours differ. */
const themes = ["light", "dark"] as const;

type Theme = (typeof themes)[number];

/** A common phone's screen, in CSS pixels, held upright. */
const phone = { width: 390, height: 844 } as const;

/**
 * Visits visit's path in page, and fails unless it answers with its status
 * and heading, in theme, with no axe-core violation, and looks as the
 * baseline named image shows it.
 */
async function expectPage(page: Page, visit: Visited, theme: Theme, image: string): Promise<void> {
  const response = await page.goto(visit.path);
  expect(response?.status()).toBe(visit.status);
  await expect(page.locator("html")).toHaveAttribute("data-theme", theme);
  await expect(page.getByRole("heading", { level: 1 })).toHaveText(visit.heading);
  await expectNoAxeViolations(page);
  await expect(page).toHaveScreenshot(image, { fullPage: true });
}

for (const theme of themes) {
  test.describe(`in the ${theme} theme`, () => {
    // A reader who chose the theme their system already uses.
    test.use({ colorScheme: theme });

    test.beforeEach(async ({ context, baseURL }) => {
      await context.addCookies([{ name: themeCookie, value: theme, url: baseURL ?? "" }]);
    });

    for (const visit of visited) {
      test(`the ${visit.name} page`, async ({ page }) => {
        await expectPage(page, visit, theme, `${visit.name}-${theme}.png`);
      });
    }

    test.describe("at a phone's width", () => {
      test.use({ viewport: phone });

      for (const visit of visited) {
        test(`the ${visit.name} page`, async ({ page }) => {
          await expectPage(page, visit, theme, `${visit.name}-${theme}-phone.png`);
        });
      }
    });

    // Sending the sign-in form answers that signing in is not available
    // yet: the same error page as the 404's, with its own text.
    test("the page the sign-in form answers with", async ({ page }) => {
      await page.goto("/login");
      await page.getByLabel("Username").fill("reader");
      await page.getByLabel("Password").fill("a passphrase of some length");
      const answer = page.waitForResponse(
        (response) =>
          response.request().method() === "POST" && new URL(response.url()).pathname === "/login",
      );
      await page.getByRole("button", { name: "Sign in" }).click();
      expect((await answer).status()).toBe(501);
      await expect(page.getByRole("heading", { level: 1 })).toHaveText("Not available yet");
      await expectNoAxeViolations(page);
    });
  });
}
