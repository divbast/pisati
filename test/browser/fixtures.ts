// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

// What every browser test runs against: a Pisati of its own, from the
// binary just browser builds from the tree, serving the production build
// of the front end that go:embed compiled into it. Each test gets a new
// one, with a state directory of its own and free loopback ports, so that
// no two tests share a database, a limiter's buckets or a port, and the
// browser visits the base URL Pisati is set to.

import { spawn, type ChildProcess } from "node:child_process";
import { once } from "node:events";
import { mkdir, mkdtemp, open, readFile, rm } from "node:fs/promises";
import { createServer, type Server } from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";

import AxeBuilder from "@axe-core/playwright";
import { test as base, expect, type Page, type TestInfo } from "@playwright/test";

/** The options the configuration sets for every test. */
export interface Options {
  /** The pisati binary to serve, which just browser builds. */
  readonly pisatiBinary: string;
}

/** A Pisati serving one test. */
export interface Pisati {
  /** Where it serves its pages, and the base URL it is set to. */
  readonly url: string;
}

/** How long Pisati has to answer /healthz once started. */
const startWithin = 10_000;
/** How long Pisati has to stop once told: its 8 s drain, and a margin. */
const stopWithin = 15_000;
/** How often /healthz is asked while Pisati starts. */
const pollEvery = 25;

/** Listens on a free loopback port, which the server holds until closed. */
async function listen(): Promise<Server> {
  const server = createServer();
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  return server;
}

/** The port server listens on. */
function portOf(server: Server): number {
  const address = server.address();
  if (address === null || typeof address === "string") {
    throw new Error("a loopback listener has no port");
  }
  return address.port;
}

/**
 * Two loopback ports that were free a moment ago, and are not the same:
 * both are held while the other is found. Pisati's configuration takes
 * no port 0, so the suite picks its own, as the Go tests do.
 */
async function freePorts(): Promise<[number, number]> {
  const servers = [await listen(), await listen()] as const;
  const ports: [number, number] = [portOf(servers[0]), portOf(servers[1])];
  await Promise.all(
    servers.map(async (server) => {
      server.close();
      await once(server, "close");
    }),
  );
  return ports;
}

/** Resolves once delay milliseconds have passed. */
async function sleep(delay: number): Promise<void> {
  await new Promise((resolve) => {
    setTimeout(resolve, delay);
  });
}

/** Whether child has exited. */
function exited(child: ChildProcess): boolean {
  return child.exitCode !== null || child.signalCode !== null;
}

/** Whether healthz answers 200 now. */
async function healthy(healthz: string): Promise<boolean> {
  try {
    const response = await fetch(healthz, { signal: AbortSignal.timeout(1000) });
    return response.status === 200;
  } catch {
    return false;
  }
}

/**
 * Waits until healthz answers 200, failing when child exits first or
 * the deadline, a time as Date.now() gives it, passes.
 */
async function waitUntilHealthy(
  child: ChildProcess,
  healthz: string,
  deadline: number,
): Promise<void> {
  if (await healthy(healthz)) {
    return;
  }
  if (exited(child)) {
    throw new Error(`pisati serve exited before it was healthy, with ${describeExit(child)}`);
  }
  if (Date.now() > deadline) {
    throw new Error(`pisati serve was not healthy within ${startWithin} ms`);
  }
  await sleep(pollEvery);
  await waitUntilHealthy(child, healthz, deadline);
}

/** How child ended, as a failure message names it. */
function describeExit(child: ChildProcess): string {
  return child.signalCode === null ? `exit code ${child.exitCode}` : `signal ${child.signalCode}`;
}

/**
 * Stops child with SIGTERM, as an engine stops the image, and fails
 * unless it exits 0 within stopWithin; one that does not is killed. A
 * child that has exited already has nothing to stop: whoever found it
 * gone said so.
 */
async function stop(child: ChildProcess): Promise<void> {
  if (exited(child)) {
    return;
  }
  const exit = once(child, "exit");
  child.kill("SIGTERM");
  const timer = AbortSignal.timeout(stopWithin);
  const late = once(timer, "abort");
  await Promise.race([exit, late]);
  if (!exited(child)) {
    child.kill("SIGKILL");
    await exit;
    throw new Error(`pisati serve did not stop within ${stopWithin} ms of SIGTERM`);
  }
  if (child.exitCode !== 0) {
    throw new Error(`pisati serve stopped with ${describeExit(child)}`);
  }
}

/**
 * Runs pisati serve from binary in dir, for use, then stops it. Its
 * environment is only what it is given here: the base URL, both
 * listeners on loopback, the state directory, and an empty configuration
 * directory, so that it reads nothing from /etc/pisati or the suite's
 * own environment. What it logs goes to pisati.log in dir.
 */
async function serve(
  binary: string,
  dir: string,
  use: (p: Pisati) => Promise<void>,
): Promise<void> {
  const [publicPort, internalPort] = await freePorts();
  const url = `http://127.0.0.1:${publicPort}`;
  const state = join(dir, "state");
  const configuration = join(dir, "configuration");
  await mkdir(state, { mode: 0o700 });
  await mkdir(configuration, { mode: 0o700 });
  const log = await open(join(dir, "pisati.log"), "w", 0o600);
  try {
    const child = spawn(binary, ["serve"], {
      env: {
        CONFIGURATION_DIRECTORY: configuration,
        PISATI_SERVER_BASE_URL: url,
        PISATI_SERVER_LISTEN: `127.0.0.1:${publicPort}`,
        PISATI_SERVER_INTERNAL_LISTEN: `127.0.0.1:${internalPort}`,
        PISATI_STORAGE_STATE_DIR: state,
      },
      stdio: ["ignore", log.fd, log.fd],
    });
    try {
      await waitUntilHealthy(
        child,
        `http://127.0.0.1:${internalPort}/healthz`,
        Date.now() + startWithin,
      );
      await use({ url });
      if (exited(child)) {
        throw new Error(`pisati serve stopped during the test, with ${describeExit(child)}`);
      }
    } finally {
      await stop(child);
    }
  } finally {
    await log.close();
  }
}

/** Attaches what Pisati logged to the test when it did not go as expected. */
async function attachLog(testInfo: TestInfo, dir: string): Promise<void> {
  if (testInfo.status === testInfo.expectedStatus) {
    return;
  }
  await testInfo.attach("pisati.log", {
    body: await readFile(join(dir, "pisati.log")),
    contentType: "text/plain",
  });
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
    } finally {
      await attachLog(testInfo, dir);
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
 * checked is the page at rest: Chromium, for one, moves the theme
 * buttons from its own button colour to the stylesheet's as a page loads.
 */
export async function settle(page: Page): Promise<void> {
  await page.evaluate(async () => {
    await Promise.allSettled(document.getAnimations().map((animation) => animation.finished));
  });
}

/**
 * Fails the test on any axe-core violation on page once it is at rest,
 * naming each rule and the elements that break it. Every rule axe-core
 * runs by default is checked: WCAG 2.2 A and AA, and its best practices.
 */
export async function expectNoAxeViolations(page: Page): Promise<void> {
  await settle(page);
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
