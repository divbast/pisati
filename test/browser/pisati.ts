// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

// pisati serve, run for one browser test: started from the binary just
// browser builds, with an environment of its own, on free loopback ports,
// and stopped as an engine stops the image.

import { spawn, type ChildProcess } from "node:child_process";
import { once } from "node:events";
import { mkdir, open } from "node:fs/promises";
import { createServer, type Server } from "node:net";
import { join } from "node:path";

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

/** pisati serve, started, and the error it reported since, if any. */
interface Started {
  readonly child: ChildProcess;
  readonly fault: () => unknown;
}

/**
 * Starts pisati serve from binary with env alone, writing to the file
 * descriptor log. It fails with what kept the process from starting,
 * such as a binary that is not there, which would otherwise be an error
 * event no one hears, and end the worker. Once started, the one error a
 * process reports is a signal it could not be sent: fault keeps it, so
 * that the stop that then fails can say why.
 */
async function start(
  binary: string,
  env: Readonly<Record<string, string>>,
  log: number,
): Promise<Started> {
  const child = spawn(binary, ["serve"], { env, stdio: ["ignore", log, log] });
  let fault: unknown;
  child.on("error", (error: unknown) => {
    fault = error;
  });
  try {
    await once(child, "spawn");
  } catch (error) {
    throw new Error(`pisati serve could not start from ${binary}`, { cause: error });
  }
  return { child, fault: () => fault };
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
export async function serve(
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
    const env = {
      CONFIGURATION_DIRECTORY: configuration,
      PISATI_SERVER_BASE_URL: url,
      PISATI_SERVER_LISTEN: `127.0.0.1:${publicPort}`,
      PISATI_SERVER_INTERNAL_LISTEN: `127.0.0.1:${internalPort}`,
      PISATI_STORAGE_STATE_DIR: state,
    };
    const { child, fault } = await start(binary, env, log.fd);
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
      await stop(child).catch((error: unknown) => {
        const reported = fault();
        throw reported === undefined
          ? error
          : new Error("pisati serve could not be stopped", { cause: reported });
      });
    }
  } finally {
    await log.close();
  }
}
