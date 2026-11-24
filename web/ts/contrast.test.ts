// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

// The colour pairs in the list below, each a foreground --color-* token of
// theme.css on a background one, meet WCAG 2.2's contrast minimums, in
// both schemes, with more contrast asked for or not, and at every accent
// hue a blog may set: 4.5:1 for text (1.4.3), and 3:1 for what marks out a
// control or its state (1.4.11) and for the lines that only separate,
// which Pisati holds to the same. The test reads every --color-* token,
// fails on one it cannot read, and on one that takes currentcolor, whose
// colour depends on where it is used.

import { expect, it } from "vite-plus/test";

import theme from "../css/theme.css?raw";

type Oklch = readonly [lightness: number, chroma: number, hue: number | "accent"];
type Scheme = "light" | "dark";
type Rgb = readonly [number, number, number];
// A colour token: a value for each scheme, another token's name, or
// another token's colour at a share, over transparent.
type Token =
  | Readonly<Record<Scheme, Oklch>>
  | { readonly alias: string }
  | { readonly mix: string; readonly share: number };

const oklch = String.raw`oklch\(\s*([\d.]+)%\s+([\d.]+)\s+(var\(--accent-hue\)|[\d.]+)\s*\)`;
const pair = new RegExp(
  String.raw`--color-([a-z-]+):\s*light-dark\(\s*${oklch}\s*,\s*${oklch}\s*\)`,
  "gu",
);
const alias = /--color-([a-z-]+):\s*var\(--color-([a-z-]+)\)/gu;
const mix =
  /--color-([a-z-]+):\s*color-mix\(\s*in oklch\s*,\s*var\(--color-([a-z-]+)\)\s+([\d.]+)%\s*,\s*transparent\s*\)/gu;
// Every --color-* declaration, with its value.
const declaration = /--color-([a-z-]+)\s*:([^;]*);/gu;

function colour(l: string, c: string, h: string): Oklch {
  return [Number(l) / 100, Number(c), h.startsWith("var(") ? "accent" : Number(h)];
}

// The tokens a block of theme.css sets.
function tokens(css: string): ReadonlyMap<string, Token> {
  const found = new Map<string, Token>();
  for (const [, name = "", l1 = "", c1 = "", h1 = "", l2 = "", c2 = "", h2 = ""] of css.matchAll(
    pair,
  )) {
    found.set(name, { light: colour(l1, c1, h1), dark: colour(l2, c2, h2) });
  }
  for (const [, name = "", target = ""] of css.matchAll(alias)) {
    found.set(name, { alias: target });
  }
  for (const [, name = "", over = "", share = ""] of css.matchAll(mix)) {
    found.set(name, { mix: over, share: Number(share) / 100 });
  }
  return found;
}

// The --color-* tokens css declares that tokens cannot read, and so no
// pair could check.
function unread(css: string): string[] {
  const read = tokens(css);
  return [...css.matchAll(declaration)]
    .map(([, name = ""]) => name)
    .filter((name) => !read.has(name));
}

const more = "@media (prefers-contrast: more)";

// The tokens theme.css sets, as they stand, and with more contrast asked
// for. A theme.css without the more-contrast block is an error, so its
// colours never go unchecked by quietly checking the others twice.
function blocks(css: string): [ReadonlyMap<string, Token>, ReadonlyMap<string, Token>] {
  const at = css.indexOf(more);
  if (at < 0) {
    throw new Error(`theme.css has no ${more} block to check`);
  }
  const base = tokens(css.slice(0, at));
  return [base, new Map([...base, ...tokens(css.slice(at))])];
}

const [base, contrasted] = blocks(theme);

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

// Linear sRGB of an oklch colour, after reducing chroma until it fits
// sRGB, as a browser maps a colour outside the screen's gamut.
function rgb([l, c, h]: readonly [number, number, number]): Rgb {
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
  return [clamp(r), clamp(g), clamp(b)];
}

// sRGB's transfer function and its inverse, since a browser blends a
// translucent colour over what lies under it in encoded sRGB.
function encode(v: number): number {
  return v <= 0.0031308 ? 12.92 * v : 1.055 * v ** (1 / 2.4) - 0.055;
}

function decode(v: number): number {
  return v <= 0.04045 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4;
}

// The linear sRGB of the token name, in scheme, at the accent hue, laid
// over back, which a translucent token needs.
function shown(
  set: ReadonlyMap<string, Token>,
  name: string,
  scheme: Scheme,
  hue: number,
  back: Rgb,
): Rgb {
  const token = set.get(name);
  if (token === undefined) {
    throw new Error(`theme.css sets no --color-${name}`);
  }
  if ("alias" in token) {
    return shown(set, token.alias, scheme, hue, back);
  }
  if ("mix" in token) {
    const over = shown(set, token.mix, scheme, hue, back);
    const blend = (i: 0 | 1 | 2): number =>
      decode(token.share * encode(over[i]) + (1 - token.share) * encode(back[i]));
    return [blend(0), blend(1), blend(2)];
  }
  const [l, c, h] = token[scheme];
  return rgb([l, c, h === "accent" ? hue : h]);
}

function luminance([r, g, b]: Rgb): number {
  return 0.2126 * r + 0.7152 * g + 0.0722 * b;
}

function ratio(
  set: ReadonlyMap<string, Token>,
  fore: string,
  back: string,
  scheme: Scheme,
  hue: number,
): number {
  const under = shown(set, back, scheme, hue, [0, 0, 0]);
  const a = luminance(shown(set, fore, scheme, hue, under));
  const b = luminance(under);
  return (Math.max(a, b) + 0.05) / (Math.min(a, b) + 0.05);
}

// Each pair a page shows, and the least contrast it needs.
const pairs: readonly [fore: string, back: string, least: number][] = [
  ["text", "canvas", 4.5],
  ["text", "surface", 4.5],
  ["text", "selection", 4.5],
  ["text-muted", "selection", 4.5],
  ["text-muted", "canvas", 4.5],
  ["accent", "canvas", 4.5],
  ["accent-strong", "canvas", 4.5],
  ["on-accent", "accent", 4.5],
  ["on-accent", "accent-strong", 4.5],
  ["border-field", "canvas", 3],
  ["border-field", "surface", 3],
  ["danger", "canvas", 3],
  ["rule", "canvas", 3],
  ["rule", "surface", 3],
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

it("reads every colour token theme.css declares, in either block", () => {
  const at = theme.indexOf(more);
  expect(unread(theme.slice(0, at))).toEqual([]);
  expect(unread(theme.slice(at))).toEqual([]);
});

it("has no colour token that takes currentcolor, whose colour depends on where it is used", () => {
  const takers = [...theme.matchAll(declaration)]
    .filter(([, , value = ""]) => /currentcolor/iu.test(value))
    .map(([, name = ""]) => name);
  expect(takers).toEqual([]);
});

it("knows every colour token, so that each one new gets its pairs", () => {
  expect([...base.keys()].toSorted()).toEqual(
    [
      "accent",
      "accent-strong",
      "border-field",
      "canvas",
      "danger",
      "on-accent",
      "rule",
      "selection",
      "surface",
      "text",
      "text-muted",
    ].toSorted(),
  );
});

it("refuses a theme with no more-contrast block", () => {
  expect(() => blocks(theme.replace(more, "@media (width >= 0)"))).toThrow(more);
});
