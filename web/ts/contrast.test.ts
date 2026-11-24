// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

// The colour tokens in theme.css meet WCAG 2.2's contrast minimums, in
// both schemes, with more contrast asked for or not, and at every accent
// hue a blog may set: 4.5:1 for text (1.4.3) and 3:1 for what marks out a
// control or its state (1.4.11).

import { expect, it } from "vite-plus/test";

import theme from "../css/theme.css?raw";

type Oklch = readonly [lightness: number, chroma: number, hue: number | "accent"];
type Scheme = "light" | "dark";
type Tokens = ReadonlyMap<string, Readonly<Record<Scheme, Oklch>> | { alias: string }>;

const oklch = String.raw`oklch\(\s*([\d.]+)%\s+([\d.]+)\s+(var\(--accent-hue\)|[\d.]+)\s*\)`;
const pair = new RegExp(
  String.raw`--color-([a-z-]+):\s*light-dark\(\s*${oklch}\s*,\s*${oklch}\s*\)`,
  "gu",
);
const alias = /--color-([a-z-]+):\s*var\(--color-([a-z-]+)\)/gu;

function colour(l: string, c: string, h: string): Oklch {
  return [Number(l) / 100, Number(c), h.startsWith("var(") ? "accent" : Number(h)];
}

// The tokens a block of theme.css sets.
function tokens(css: string): Tokens {
  const found = new Map<string, Readonly<Record<Scheme, Oklch>> | { alias: string }>();
  for (const [, name = "", l1 = "", c1 = "", h1 = "", l2 = "", c2 = "", h2 = ""] of css.matchAll(
    pair,
  )) {
    found.set(name, { light: colour(l1, c1, h1), dark: colour(l2, c2, h2) });
  }
  for (const [, name = "", target = ""] of css.matchAll(alias)) {
    found.set(name, { alias: target });
  }
  return found;
}

const more = "@media (prefers-contrast: more)";
const base = tokens(theme.slice(0, theme.indexOf(more)));
const contrasted = new Map([...base, ...tokens(theme.slice(theme.indexOf(more)))]);

// Linear sRGB of an oklch colour, by Björn Ottosson's OKLab matrices.
function linear([l, c, h]: readonly [number, number, number]): [number, number, number] {
  const a = c * Math.cos((h * Math.PI) / 180);
  const b = c * Math.sin((h * Math.PI) / 180);
  const lms = [
    (l + 0.3963377774 * a + 0.2158037573 * b) ** 3,
    (l - 0.1055613458 * a - 0.0638541728 * b) ** 3,
    (l - 0.0894841775 * a - 1.291485548 * b) ** 3,
  ] as const;
  return [
    4.0767416621 * lms[0] - 3.3077115913 * lms[1] + 0.2309699292 * lms[2],
    -1.2684380046 * lms[0] + 2.6097574011 * lms[1] - 0.3413193965 * lms[2],
    -0.0041960863 * lms[0] - 0.7034186147 * lms[1] + 1.707614701 * lms[2],
  ];
}

function clamp(v: number): number {
  return Math.min(1, Math.max(0, v));
}

// Relative luminance, after reducing chroma until the colour fits sRGB,
// as a browser maps an oklch colour outside the screen's gamut.
function luminance([l, c, h]: readonly [number, number, number]): number {
  const fits = (chroma: number): boolean =>
    linear([l, chroma, h]).every((v) => v >= -1e-6 && v <= 1 + 1e-6);
  let chroma = c;
  if (!fits(chroma)) {
    let low = 0;
    for (let step = 0; step < 24; step++) {
      const mid = (low + chroma) / 2;
      if (fits(mid)) {
        low = mid;
      } else {
        chroma = mid;
      }
    }
    chroma = low;
  }
  const [r, g, b] = linear([l, chroma, h]);
  return 0.2126 * clamp(r) + 0.7152 * clamp(g) + 0.0722 * clamp(b);
}

function resolve(set: Tokens, name: string, scheme: Scheme, hue: number): [number, number, number] {
  const token = set.get(name);
  if (token === undefined) {
    throw new Error(`theme.css sets no --color-${name}`);
  }
  if ("alias" in token) {
    return resolve(set, token.alias, scheme, hue);
  }
  const [l, c, h] = token[scheme];
  return [l, c, h === "accent" ? hue : h];
}

function ratio(set: Tokens, fore: string, back: string, scheme: Scheme, hue: number): number {
  const a = luminance(resolve(set, fore, scheme, hue));
  const b = luminance(resolve(set, back, scheme, hue));
  return (Math.max(a, b) + 0.05) / (Math.min(a, b) + 0.05);
}

// Each pair a page shows, and the least contrast it needs.
const pairs: readonly [fore: string, back: string, least: number][] = [
  ["text", "canvas", 4.5],
  ["text", "surface", 4.5],
  ["text-muted", "canvas", 4.5],
  ["accent", "canvas", 4.5],
  ["accent-strong", "canvas", 4.5],
  ["on-accent", "accent", 4.5],
  ["on-accent", "accent-strong", 4.5],
  ["border-field", "canvas", 3],
  ["danger", "canvas", 3],
];

const hues = Array.from({ length: 72 }, (_, i) => i * 5);

it.each(pairs)("--color-%s on --color-%s meets %d:1", (fore, back, least) => {
  for (const [set, contrast] of [
    [base, "no-preference"],
    [contrasted, "more"],
  ] as const) {
    for (const scheme of ["light", "dark"] as const) {
      const worst = Math.min(...hues.map((hue) => ratio(set, fore, back, scheme, hue)));
      expect(worst, `${scheme} scheme, prefers-contrast ${contrast}`).toBeGreaterThanOrEqual(least);
    }
  }
});

it("reads every colour token, so a pair above never goes unchecked", () => {
  expect([...base.keys()].toSorted()).toEqual(
    [
      "accent",
      "accent-strong",
      "border-field",
      "canvas",
      "danger",
      "on-accent",
      "selection",
      "surface",
      "text",
      "text-muted",
    ].toSorted(),
  );
});
