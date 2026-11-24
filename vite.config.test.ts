// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

import { expect, it, vi } from "vite-plus/test";

import type { ConfigEnv } from "vite-plus";

import paths from "./internal/config/testdata/dev-certificate-paths.json";
import viteURLs from "./internal/config/testdata/dev-vite-urls.json";

import { certificateFiles, devCertificate, devHTTPS, scriptTargets } from "./vite.config.ts";
import { devServerURL } from "./vite.devserver.ts";

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

it("finds the dev server where PISATI_DEV_VITE_URL points, an empty one counting as unset", () => {
  expect(devServerURL({}).href).toBe("http://localhost:5173/");
  expect(devServerURL({ PISATI_DEV_VITE_URL: "" }).href).toBe("http://localhost:5173/");
  expect(devServerURL({ PISATI_DEV_VITE_URL: "https://[::1]:5174" }).href).toBe(
    "https://[::1]:5174/",
  );
});

// A case of the table Pisati's config test reads too: its URL whole, or,
// when it holds user information, in parts, so that no credential-shaped
// string sits in the table; base is the base URL beside it, and a case
// whose verdict hangs on that is pisatiOnly, as Vite never reads it.
interface DevViteURL {
  readonly url?: string | undefined;
  readonly scheme?: string | undefined;
  readonly user?: string | undefined;
  readonly password?: string | undefined;
  readonly host?: string | undefined;
  readonly base?: string | undefined;
  readonly pisatiOnly?: boolean | undefined;
  readonly valid: boolean;
}

const devViteURLs: readonly DevViteURL[] = viteURLs;

// The URL a case gives, assembled from its parts when it has them.
function assembled({ url, scheme, user, password, host }: DevViteURL): string {
  if (user === undefined) {
    return url ?? "";
  }
  return `${scheme ?? ""}://${user}${password === undefined ? "" : `:${password}`}@${host ?? ""}`;
}

// What of a case's user information an error must not hold: the user,
// the password and the two as a URL joins them, for each it has.
function userInfo({ user, password }: DevViteURL): string[] {
  return [
    ...(user === undefined || user === "" ? [] : [user]),
    ...(password === undefined ? [] : [password, `${user ?? ""}:${password}`]),
  ];
}

// Each URL of the table, but for those whose verdict hangs on the base
// URL, so that Vite refuses PISATI_DEV_VITE_URL exactly when Pisati
// refuses dev.vite_url, and never repeats it, as it may hold a password.
it.each(devViteURLs.filter(({ pisatiOnly }) => pisatiOnly !== true))(
  "takes PISATI_DEV_VITE_URL %j as Pisati takes dev.vite_url",
  (testCase: DevViteURL) => {
    const url = assembled(testCase);
    const check = (): URL => devServerURL({ PISATI_DEV_VITE_URL: url });
    if (testCase.valid) {
      expect(check).not.toThrow();
      return;
    }
    expect(check).toThrow("PISATI_DEV_VITE_URL: ");
    for (const part of [url, ...userInfo(testCase)]) {
      expect(check).not.toThrow(part);
    }
  },
);

// The rule holds for every command: the configuration refuses to load,
// here with the table's URL that holds a password.
it("refuses to load the configuration with a URL Pisati refuses", async () => {
  const withPassword = devViteURLs.find(({ password }) => password !== undefined);
  expect(withPassword).toBeDefined();
  vi.stubEnv("PISATI_DEV_VITE_URL", assembled(withPassword ?? { valid: false }));
  vi.resetModules();
  try {
    const loading = import("./vite.config.ts");
    await expect(loading).rejects.toThrow("it holds user information");
    const message = await loading.then(() => "", String);
    for (const part of userInfo(withPassword ?? { valid: false })) {
      expect(message).not.toContain(part);
    }
  } finally {
    vi.unstubAllEnvs();
    vi.resetModules();
  }
});

it("serves https from the certificate and key the variables name", () => {
  const https = new URL("https://localhost:5174");
  const both = {
    PISATI_DEV_VITE_CERT_FILE: "/certs/c.pem",
    PISATI_DEV_VITE_KEY_FILE: "/certs/k.pem",
  };
  expect(devCertificate(https, both)).toEqual({ cert: "/certs/c.pem", key: "/certs/k.pem" });
  // Used only with https, and otherwise left alone.
  expect(devCertificate(new URL("http://localhost:5173"), both)).toBeUndefined();
  expect(devCertificate(new URL("http://localhost:5173"), {})).toBeUndefined();
});

it.each<[string, Readonly<Record<string, string>>, RegExp]>([
  [
    "no key",
    { PISATI_DEV_VITE_CERT_FILE: "/certs/c.pem" },
    /^(?!.*CERT_FILE).*PISATI_DEV_VITE_KEY_FILE/u,
  ],
  [
    "an empty certificate",
    { PISATI_DEV_VITE_CERT_FILE: "", PISATI_DEV_VITE_KEY_FILE: "/k.pem" },
    /PISATI_DEV_VITE_CERT_FILE/u,
  ],
  ["neither", {}, /PISATI_DEV_VITE_CERT_FILE.*PISATI_DEV_VITE_KEY_FILE/u],
])("refuses an https dev server with %s, naming what is missing", (_case, env, message) => {
  expect(() => devCertificate(new URL("https://localhost:5174"), env)).toThrow(message);
});

// Each path of the table Pisati's config test reads too, so that Pisati and
// Vite agree on what a certificate or key path may be: a valid one is
// accepted in either variable, and any other refused, whenever it is set
// and whatever the dev server, without its value in the error, since it
// may be the key itself, pasted by mistake.
it.each(paths)("takes %j as Pisati does", ({ path, valid }: Readonly<(typeof paths)[number]>) => {
  for (const name of ["PISATI_DEV_VITE_CERT_FILE", "PISATI_DEV_VITE_KEY_FILE"]) {
    const check = (): void => {
      certificateFiles({ [name]: path });
    };
    if (valid) {
      expect(check).not.toThrow();
      continue;
    }
    expect(check).toThrow(name);
    if (path !== "") {
      expect(check).not.toThrow(path);
    }
  }
});

it("accepts no certificate variables at all", () => {
  expect(() => {
    certificateFiles({});
  }).not.toThrow();
});

// Only vp dev starts the dev server, so only it needs the certificate
// files and reads them: vp build, Vitest's test mode and vp preview never do.
it.each<[string, ConfigEnv]>([
  ["vp build", { command: "build", mode: "production" }],
  ["vp test", { command: "serve", mode: "test" }],
  ["vp preview", { command: "serve", mode: "production", isPreview: true }],
])("never needs or reads the certificate for %s", (_case, configEnv) => {
  const reads: string[] = [];
  const record = (path: string): string => {
    reads.push(path);
    return path;
  };
  expect(devHTTPS(configEnv, new URL("https://localhost:5174"), {}, record)).toBeUndefined();
  expect(reads).toEqual([]);
});

function read(path: string): string {
  return `read ${path}`;
}

it("reads the certificate for vp dev, and refuses to start without it", () => {
  const dev: ConfigEnv = { command: "serve", mode: "development" };
  const https = new URL("https://localhost:5174");
  expect(() => devHTTPS(dev, https, {}, read)).toThrow("PISATI_DEV_VITE_CERT_FILE");
  expect(
    devHTTPS(
      dev,
      https,
      { PISATI_DEV_VITE_CERT_FILE: "/c.pem", PISATI_DEV_VITE_KEY_FILE: "/k.pem" },
      read,
    ),
  ).toEqual({ cert: "read /c.pem", key: "read /k.pem" });
  expect(devHTTPS(dev, new URL("http://localhost:5173"), {}, read)).toBeUndefined();
});

// The rule holds for every command: the configuration itself, which vp
// check, vp test and vp build load as vp dev does, refuses to load.
it("refuses to load the configuration with a malformed certificate variable", async () => {
  vi.stubEnv("PISATI_DEV_VITE_KEY_FILE", "certs/localhost-key.pem");
  vi.resetModules();
  try {
    await expect(import("./vite.config.ts")).rejects.toThrow("PISATI_DEV_VITE_KEY_FILE");
  } finally {
    vi.unstubAllEnvs();
    vi.resetModules();
  }
});
