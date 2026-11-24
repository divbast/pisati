// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

import { expect, it } from "vite-plus/test";

import { devCertificate, devServerURL, scriptTargets } from "./vite.config.ts";

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
