// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

// The front end's one configuration: Vite's build and dev server, Vitest,
// Oxlint and Oxfmt, all run through Vite+'s vp.

import browserslist from "browserslist";
import { browserslistToTargets } from "lightningcss";
import { defineConfig } from "vite-plus";

// The browsers package.json's browserslist names, read once, so the
// stylesheets and the scripts are built for the same ones.
const browsers = browserslist();

// The script build's targets, in the form build.target takes: the oldest
// version of each engine the browsers above run. Opera, Samsung Internet
// and Android's WebView are Chromium, so Chrome's version covers them.
function scriptTargets(list: readonly string[]): string[] {
  const engines: Readonly<Record<string, string>> = {
    chrome: "chrome",
    and_chr: "chrome",
    edge: "edge",
    firefox: "firefox",
    and_ff: "firefox",
    safari: "safari",
    ios_saf: "ios",
  };
  const oldest = new Map<string, number>();
  for (const browser of list) {
    const [name = "", version = ""] = browser.split(" ");
    const engine = engines[name];
    // A range, such as iOS's "18.5-18.6", counts as its first version.
    const first = Number.parseFloat(version.split("-")[0] ?? "");
    if (engine === undefined || Number.isNaN(first)) {
      continue;
    }
    oldest.set(engine, Math.min(oldest.get(engine) ?? first, first));
  }
  return [...oldest].map(([engine, version]) => `${engine}${version}`).toSorted();
}

export default defineConfig({
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
    target: scriptTargets(browsers),
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
    port: 5173,
    strictPort: true,
    // Pages come from Pisati, at another origin, so the URLs Vite writes
    // into what it serves name the dev server itself.
    origin: "http://localhost:5173",
  },
  test: {
    environment: "happy-dom",
    include: ["web/ts/**/*.test.ts"],
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
    },
    options: {
      typeAware: true,
      typeCheck: true,
      denyWarnings: true,
      reportUnusedDisableDirectives: "error",
    },
  },
});
