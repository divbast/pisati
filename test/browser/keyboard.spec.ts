// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

// The keyboard alone: the order focus moves in, the skip link, and the
// theme buttons.

import type { Locator, Page } from "@playwright/test";

import { expect, test } from "./fixtures.ts";

/**
 * The key that moves focus to the next item. Safari's Tab skips links
 * unless its reader asks otherwise, and Option-Tab reaches every item,
 * so in WebKit Option-Tab is the reader's key for the whole order.
 */
function nextKey(browserName: string): string {
  return browserName === "webkit" ? "Alt+Tab" : "Tab";
}

/**
 * Presses key once for each of items, and fails unless each press moves
 * focus to the next of them. Each press needs the one before it, so they
 * go one at a time.
 */
async function expectFocusOrder(page: Page, key: string, items: readonly Locator[]): Promise<void> {
  const [item, ...rest] = items;
  if (item === undefined) {
    return;
  }
  await page.keyboard.press(key);
  await expect(item).toBeFocused();
  await expectFocusOrder(page, key, rest);
}

/** What every page's header holds, in order. */
function header(page: Page): Locator[] {
  return [
    page.getByRole("link", { name: "Skip to content" }),
    page.getByRole("link", { name: "Pisati" }),
    page.getByRole("button", { name: "Light" }),
    page.getByRole("button", { name: "Dark" }),
    page.getByRole("button", { name: "Auto" }),
  ];
}

/** A page, and what its main content holds that focus reaches, in order. */
interface Ordered {
  readonly name: string;
  readonly path: string;
  readonly content: (page: Page) => Locator[];
}

const ordered: readonly Ordered[] = [
  {
    name: "home",
    path: "/",
    content: (page) => [page.getByRole("link", { name: "Sign in" })],
  },
  {
    name: "sign-in",
    path: "/login",
    content: (page) => [
      page.getByLabel("Username"),
      page.getByLabel("Password"),
      page.getByRole("button", { name: "Sign in" }),
    ],
  },
  {
    name: "not-found",
    path: "/no-page-here",
    content: (page) => [page.getByRole("link", { name: "Go to the home page" })],
  },
];

for (const { name, path, content } of ordered) {
  test(`focus moves through the ${name} page in reading order, and back`, async ({
    page,
    browserName,
  }) => {
    await page.goto(path);
    const order = [...header(page), ...content(page)];
    const key = nextKey(browserName);
    await expectFocusOrder(page, key, order);
    await expectFocusOrder(page, `Shift+${key}`, order.toReversed().slice(1));
  });
}

test("the skip link shows when focused, and moves focus to the content", async ({
  page,
  browserName,
}) => {
  await page.goto("/login");
  const skip = page.getByRole("link", { name: "Skip to content" });
  await page.keyboard.press(nextKey(browserName));
  await expect(skip).toBeFocused();
  await expect(skip).toBeInViewport({ ratio: 1 });
  await expect(page).toHaveScreenshot("sign-in-skip-link.png");

  await page.keyboard.press("Enter");
  await page.keyboard.press(nextKey(browserName));
  await expect(page.getByLabel("Username")).toBeFocused();
});

test("the theme buttons work from the keyboard", async ({ page, browserName }) => {
  await page.goto("/");
  const dark = page.getByRole("button", { name: "Dark" });
  const light = page.getByRole("button", { name: "Light" });
  const key = nextKey(browserName);
  await expectFocusOrder(page, key, header(page).slice(0, 4));
  await expect(dark).toBeFocused();
  await page.keyboard.press("Enter");
  await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
  await expect(dark).toHaveAttribute("aria-pressed", "true");

  await page.keyboard.press(`Shift+${key}`);
  await expect(light).toBeFocused();
  await page.keyboard.press("Space");
  await expect(page.locator("html")).toHaveAttribute("data-theme", "light");
  await expect(light).toHaveAttribute("aria-pressed", "true");
});
