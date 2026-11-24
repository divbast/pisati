// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

// The front end's one configuration: Vite's build and dev server, Vitest,
// Oxlint and Oxfmt, all run through Vite+'s vp.

import browserslist from "browserslist";
import { readFileSync } from "node:fs";

import { browserslistToTargets } from "lightningcss";
import { defineConfig, loadEnv, type Plugin, type UserConfig } from "vite-plus";

// The browsers package.json's browserslist names, read once, so the
// stylesheets and the scripts are built for the same ones.
const browsers = browserslist();

// The engines build.target names, for each browser browserslist names
// that runs one. Opera and Samsung Internet run Chromium releases older
// than the Chrome of the same day, so they keep engines of their own.
const engines: Readonly<Record<string, string>> = {
  chrome: "chrome",
  and_chr: "chrome",
  edge: "edge",
  firefox: "firefox",
  and_ff: "firefox",
  safari: "safari",
  ios_saf: "ios",
  opera: "opera",
  samsung: "samsung",
};

// A browser version as major and minor numbers, compared as numbers, so
// that 17.10 comes after 17.4.
type Version = readonly [major: number, minor: number];

// The version browserslist writes, such as "132", "18.2" or the range
// "18.5-18.6", whose first version counts; none for one it gives no
// number, such as "TP".
function parseVersion(text: string): Version | undefined {
  const match = /^(\d+)(?:\.(\d+))?/u.exec(text);
  if (match === null) {
    return undefined;
  }
  return [Number(match[1]), Number(match[2] ?? "0")];
}

function older(a: Version, b: Version): boolean {
  return a[0] < b[0] || (a[0] === b[0] && a[1] < b[1]);
}

// The script build's targets, in the form build.target takes: the oldest
// version of each engine the browsers run.
export function scriptTargets(list: readonly string[]): string[] {
  const oldest = new Map<string, Version>();
  for (const browser of list) {
    const [name = "", text = ""] = browser.split(" ");
    const engine = engines[name];
    const version = parseVersion(text);
    if (engine === undefined || version === undefined) {
      continue;
    }
    const seen = oldest.get(engine);
    if (seen === undefined || older(version, seen)) {
      oldest.set(engine, version);
    }
  }
  return [...oldest]
    .map(
      ([engine, [major, minor]]: readonly [string, Version]) =>
        `${engine}${major}${minor === 0 ? "" : `.${minor}`}`,
    )
    .toSorted();
}

// Where the Vite dev server listens: the URL PISATI_DEV_VITE_URL gives,
// which Pisati's dev mode loads from, or Vite's usual address when it is
// unset or empty, as Pisati's config reads an empty one. Vite reads only
// the environment, never pisati.toml, so a dev server anywhere else needs
// the variable, for both.
export function devServerURL(env: Readonly<Record<string, string | undefined>>): URL {
  const url = env["PISATI_DEV_VITE_URL"];
  return new URL(url === undefined || url === "" ? "http://localhost:5173" : url);
}

// The certificate and key files an https dev server serves with, which
// PISATI_DEV_VITE_CERT_FILE and PISATI_DEV_VITE_KEY_FILE name, as they do
// for Pisati's config; none for an http one, which needs neither and
// leaves them unused. An https one with either missing or empty is an
// error naming what is missing.
export function devCertificate(
  url: URL,
  env: Readonly<Record<string, string | undefined>>,
): { cert: string; key: string } | undefined {
  if (url.protocol !== "https:") {
    return undefined;
  }
  const cert = env["PISATI_DEV_VITE_CERT_FILE"] ?? "";
  const key = env["PISATI_DEV_VITE_KEY_FILE"] ?? "";
  const missing = [
    ...(cert === "" ? ["PISATI_DEV_VITE_CERT_FILE"] : []),
    ...(key === "" ? ["PISATI_DEV_VITE_KEY_FILE"] : []),
  ];
  if (missing.length > 0) {
    throw new Error(
      `PISATI_DEV_VITE_URL is https (${url.origin}), so the dev server needs a certificate and its key: set ${missing.join(" and ")}`,
    );
  }
  return { cert, key };
}

const devEnv = loadEnv("development", false, "PISATI_DEV_");
const devServer = devServerURL(devEnv);

// Gives the dev server https from the certificate files, read only when
// it starts, so that vp check, vp build, vp test and vp preview never
// depend on them.
function devHTTPS(): Plugin {
  return {
    name: "pisati:dev-https",
    config(_config, { command, mode, isPreview }): UserConfig | undefined {
      // Vitest runs a dev server too, in the test mode.
      if (command !== "serve" || mode === "test" || isPreview === true) {
        return undefined;
      }
      const files = devCertificate(devServer, devEnv);
      if (files === undefined) {
        return undefined;
      }
      return {
        server: { https: { cert: readFileSync(files.cert), key: readFileSync(files.key) } },
      };
    },
  };
}

const targets = scriptTargets(browsers);

export default defineConfig({
  plugins: [devHTTPS()],
  // Pisati serves the build under /static/, so every URL the build writes,
  // a stylesheet's url() among them, starts there.
  base: "/static/",
  publicDir: false,
  css: {
    transformer: "lightningcss",
    lightningcss: { targets: browserslistToTargets(browsers) },
  },
  build: {
    outDir: "internal/web/dist",
    // Vite's own emptying would delete the placeholder that keeps
    // go:embed compiling on a fresh clone: just fe-build clears the rest.
    emptyOutDir: false,
    // .vite/manifest.json, from which Pisati names each entry's hashed file.
    manifest: true,
    target: targets,
    // The stylesheets are built for every browser above, Samsung
    // Internet's included, before Vite minifies them; the minifier takes
    // its targets in build.target's form, which has no Samsung Internet.
    cssTarget: targets.filter((target) => !target.startsWith("samsung")),
    cssMinify: "lightningcss",
    // Every file is served from /static/ under the page's policy, never
    // inlined as a data: URL, which the policy refuses.
    assetsInlineLimit: 0,
    // Every browser above preloads modules itself.
    modulePreload: { polyfill: false },
    rolldownOptions: {
      // The stylesheet is an entry of its own, so a page links it with no
      // script at all.
      input: ["web/css/layers.css", "web/ts/main.ts"],
    },
  },
  server: {
    // URL keeps an IPv6 address's brackets, which listening does not take.
    host: devServer.hostname.replace(/^\[(.*)\]$/u, "$1"),
    port: Number(devServer.port || (devServer.protocol === "https:" ? "443" : "80")),
    strictPort: true,
    origin: devServer.origin,
  },
  test: {
    environment: "happy-dom",
    include: ["web/ts/**/*.test.ts", "vite.config.test.ts"],
    // The stylesheets as they are written, for the tests that read them.
    css: { include: [/\/web\/css\/[^/]+\.css(?:\?raw)?$/u] },
  },
  fmt: {
    // Oxfmt formats the front end's code, stylesheets and JSON. The
    // Markdown and YAML are the rest of the repository's, formatted by
    // hand.
    ignorePatterns: ["**/*.md", "**/*.yml", "**/*.yaml"],
  },
  lint: {
    categories: {
      correctness: "error",
      suspicious: "error",
      perf: "error",
      pedantic: "error",
    },
    rules: {
      // The project's own parameters are readonly. The DOM's and the
      // libraries' types, Document, FormData or Vite's config among them,
      // are mutable by nature and cannot be, so the rule lets them be.
      "typescript/prefer-readonly-parameter-types": [
        "error",
        {
          allow: [
            {
              from: "lib",
              name: [
                "AbortSignal",
                "Document",
                "FormData",
                "HTMLButtonElement",
                "HTMLElement",
                "HTMLFormElement",
                "ReadonlyMap",
                "RegExp",
                "RequestInit",
                "Response",
                "SubmitEvent",
                "URL",
              ],
            },
            // Vite's types, as Vite+ ships them.
            {
              from: "package",
              package: "@voidzero-dev/vite-plus-core",
              name: ["ConfigEnv", "UserConfig"],
            },
          ],
        },
      ],
    },
    options: {
      typeAware: true,
      typeCheck: true,
      denyWarnings: true,
      reportUnusedDisableDirectives: "error",
    },
  },
});
