// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

// What every browser test runs against: a Pisati of its own, from the
// binary just browser builds from the tree, serving the production build
// of the front end that go:embed compiled into it. Each test gets a new
// one, with a state directory of its own and free loopback ports, so that
// no two tests share a database, a limiter's buckets or a port, and the
// browser visits the base URL Pisati is set to.

import { existsSync } from "node:fs";
import { mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

import AxeBuilder from "@axe-core/playwright";
import { test as base, expect, type Page, type TestInfo } from "@playwright/test";

import { serve, type Pisati } from "./pisati.ts";

/** The options the configuration sets for every test. */
export interface Options {
  /** The pisati binary to serve, which just browser builds. */
  readonly pisatiBinary: string;
}

/** Attaches what Pisati logged in dir to the test, if it logged at all. */
async function attachLog(testInfo: TestInfo, dir: string): Promise<void> {
  const path = join(dir, "pisati.log");
  if (!existsSync(path)) {
    return;
  }
  await testInfo.attach("pisati.log", { body: await readFile(path), contentType: "text/plain" });
}

/**
 * The browser tests' test: each gets a Pisati of its own, pisati, whose
 * URL is the test's baseURL, so that page.goto("/login") visits it.
 */
export const test = base.extend<{ pisati: Pisati }, Options>({
  pisatiBinary: ["", { option: true, scope: "worker" }],
  pisati: async ({ pisatiBinary }, use, testInfo) => {
    if (pisatiBinary === "") {
      throw new Error("no pisati binary: run the suite with just browser, which builds one");
    }
    const dir = await mkdtemp(join(tmpdir(), "pisati-browser-"));
    try {
      await serve(pisatiBinary, dir, use);
      if (testInfo.status !== testInfo.expectedStatus) {
        await attachLog(testInfo, dir);
      }
    } catch (error) {
      // Pisati failed, starting or stopping, and the test's own status
      // does not show that yet: what Pisati logged says why.
      await attachLog(testInfo, dir);
      throw error;
    } finally {
      await rm(dir, { recursive: true, force: true });
    }
  },
  baseURL: async ({ pisati }, use) => {
    await use(pisati.url);
  },
});

export { expect };

/** The theme cookie's name, which the theme switch sets. */
export const themeCookie = "pisati-theme";

/**
 * Waits until no animation or transition runs on page, so that what is
 * checked is the page at rest, once something a test did has set
 * something moving: a button under the pointer changes its colour as the
 * pointer arrives.
 */
export async function settle(page: Page): Promise<void> {
  await page.evaluate(async () => {
    await Promise.allSettled(document.getAnimations().map((animation) => animation.finished));
  });
}

/**
 * Fails the test unless nothing on page moves: no animation or transition
 * runs. A page appears in its final colours, from its first paint, so a
 * test can check it as soon as it has loaded, with nothing to wait for.
 */
export async function expectAtRest(page: Page): Promise<void> {
  const moving = await page.evaluate(() =>
    document.getAnimations().map((animation) => {
      const target = animation.effect instanceof KeyframeEffect ? animation.effect.target : null;
      return `${animation.constructor.name} on ${target?.nodeName ?? "nothing"}`;
    }),
  );
  expect(moving).toEqual([]);
}

/**
 * Fails the test on any axe-core violation on page, naming each rule and
 * the elements that break it, once it has checked that page is at rest
 * (expectAtRest): a colour axe-core measures mid-change is no colour the
 * reader sees. Every rule axe-core runs by default is checked: WCAG 2.2 A
 * and AA, and its best practices.
 */
export async function expectNoAxeViolations(page: Page): Promise<void> {
  await expectAtRest(page);
  const results = await new AxeBuilder({ page }).analyze();
  const found = results.violations.map(({ id, nodes }) => ({
    id,
    nodes: nodes.map(({ target, failureSummary }) => ({
      target: target.join(" "),
      summary: failureSummary ?? "",
    })),
  }));
  expect(found).toEqual([]);
}
