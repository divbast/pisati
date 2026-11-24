// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

// The Vite dev server's URL, which vite.config.ts reads from
// PISATI_DEV_VITE_URL and holds to the rules Pisati's config holds
// dev.vite_url to.

// Where the Vite dev server listens: the URL PISATI_DEV_VITE_URL gives,
// which Pisati's dev mode loads from, or Vite's usual address when it is
// unset or empty, as Pisati's config reads an empty one. Vite reads only
// the environment, never pisati.toml, so a dev server anywhere else needs
// the variable, for both. A URL Pisati's config refuses as dev.vite_url
// is refused here too, for every command, in an error that never repeats
// it, as it may hold a password. Pisati also refuses any dev server
// unless its own base URL is on localhost, which Vite never reads.
export function devServerURL(env: Readonly<Record<string, string | undefined>>): URL {
  const url = env["PISATI_DEV_VITE_URL"];
  if (url === undefined || url === "") {
    return new URL("http://localhost:5173");
  }
  const problem = devServerURLProblem(url);
  if (problem !== undefined) {
    throw new Error(
      `PISATI_DEV_VITE_URL: ${problem}; want an http or https URL on localhost, 127.0.0.1 or [::1] ` +
        "(the value is not shown, as a URL may hold a password)",
    );
  }
  return new URL(url);
}

const notLocal =
  "the Vite dev server is for development alone, so it must be on localhost, 127.0.0.1 or [::1]";

// Why Pisati's config refuses url as dev.vite_url, beside a base URL on
// localhost, or undefined when it accepts it. url is read as Go's
// url.Parse and Pisati's rules read it, never as a browser's URL parser
// does, which drops tabs and newlines, takes a backslash for a slash and
// reads 127.1 as 127.0.0.1.
function devServerURLProblem(url: string): string | undefined {
  if (/[?#]/u.test(url)) {
    return "it has a query or a fragment";
  }
  if (control(url)) {
    return "it holds a control character";
  }
  const scheme = /^[A-Za-z][A-Za-z0-9+.-]*:/u.exec(url)?.[0].toLowerCase();
  if (scheme !== "http:" && scheme !== "https:") {
    return "its scheme is not https or http";
  }
  const rest = url.slice(scheme.length);
  const slash = rest.indexOf("/", 2);
  const authority = rest.slice(2, slash === -1 ? undefined : slash);
  if (!rest.startsWith("//") || authority === "") {
    return "it has no host";
  }
  if (authority.includes("@")) {
    return "it holds user information";
  }
  if (slash !== -1 && !escapedPath(rest.slice(slash))) {
    return "its path holds a character a URL must percent-encode";
  }
  return authorityProblem(authority);
}

// Whether text holds an ASCII control character, which a URL holds only
// percent-encoded.
function control(text: string): boolean {
  for (let i = 0; i < text.length; i++) {
    // A code point past ASCII, or half of one, is no control character.
    const code = text.codePointAt(i) ?? 0;
    if (code < 0x20 || code === 0x7f) {
      return true;
    }
  }
  return false;
}

// Whether path holds only what RFC 3986 lets a path hold unescaped:
// letters, digits, "-._~", the sub-delimiters "!$&'()*+,;=", ":", "@" and
// "/", and "%" followed by two hex digits.
function escapedPath(path: string): boolean {
  return /^(?:[A-Za-z0-9\-._~!$&'()*+,;=:@/]|%[0-9A-Fa-f]{2})*$/u.test(path);
}

// Why Pisati refuses authority, a URL's host and port, for its Vite dev
// server, or undefined when it accepts it: the host must be localhost, in
// any case, 127.0.0.1, or ::1 spelt any way Go's netip reads it, in
// brackets; and a port, when there is one, from 1 to 65535.
function authorityProblem(authority: string): string | undefined {
  if (!authority.startsWith("[")) {
    const colon = authority.indexOf(":");
    const host = colon === -1 ? authority : authority.slice(0, colon);
    if (host.toLowerCase() !== "localhost" && host !== "127.0.0.1") {
      return notLocal;
    }
    return portProblem(colon === -1 ? undefined : authority.slice(colon + 1));
  }
  const close = authority.indexOf("]");
  const after = authority.slice(close + 1);
  if (close === -1 || (after !== "" && !after.startsWith(":"))) {
    return "it is not a URL";
  }
  const host = authority.slice(1, close);
  if (host.includes("%")) {
    return "its host names an IPv6 zone";
  }
  const groups = ipv6(host);
  if (groups === undefined) {
    return "it is not a URL";
  }
  if (!groups.every((group, i) => group === (i === 7 ? 1 : 0))) {
    return notLocal;
  }
  return portProblem(after === "" ? undefined : after.slice(1));
}

// Why Pisati refuses port, the text after a host's colon, or undefined
// when it accepts it, or there is no colon.
function portProblem(port: string | undefined): string | undefined {
  if (port === undefined) {
    return undefined;
  }
  if (port === "") {
    return "it has a colon but no port";
  }
  if (!/^[0-9]+$/u.test(port)) {
    return "it is not a URL";
  }
  const n = Number(port);
  return n < 1 || n > 65_535 ? "its port is not from 1 to 65535" : undefined;
}

// The eight 16-bit groups of the IPv6 address text, as Go's netip reads
// one: groups of one to four hex digits, at most one "::" standing for one
// or more groups of zeros, and an IPv4 address in the last two groups'
// place; undefined for any other text.
function ipv6(text: string): number[] | undefined {
  const halves = text.split("::");
  if (halves.length > 2) {
    return undefined;
  }
  const head = ipv6Groups(halves[0] ?? "", halves.length === 1);
  const tail = halves.length === 1 ? [] : ipv6Groups(halves[1] ?? "", true);
  if (head === undefined || tail === undefined) {
    return undefined;
  }
  const zeros = 8 - head.length - tail.length;
  if (halves.length === 1 ? zeros !== 0 : zeros < 1) {
    return undefined;
  }
  return [...head, ...Array.from({ length: zeros }, () => 0), ...tail];
}

// The groups text holds, separated by colons, the last of which may be an
// IPv4 address when last is true; undefined when any is not a group.
function ipv6Groups(text: string, last: boolean): number[] | undefined {
  if (text === "") {
    return [];
  }
  const fields = text.split(":");
  const groups: number[] = [];
  for (const [i, field] of fields.entries()) {
    const v4 = last && i === fields.length - 1 && field.includes(".") ? ipv4(field) : undefined;
    if (v4 !== undefined) {
      groups.push(v4[0] * 256 + v4[1], v4[2] * 256 + v4[3]);
    } else if (/^[0-9A-Fa-f]{1,4}$/u.test(field)) {
      groups.push(Number.parseInt(field, 16));
    } else {
      return undefined;
    }
  }
  return groups;
}

// The four octets of the IPv4 address text, as Go's netip reads one:
// four decimal numbers from 0 to 255, none with a leading zero; undefined
// for any other text.
function ipv4(text: string): readonly [number, number, number, number] | undefined {
  const fields = text.split(".");
  const octets = fields.map((field) =>
    /^(?:0|[1-9][0-9]{0,2})$/u.test(field) ? Number(field) : 256,
  );
  const [a = 256, b = 256, c = 256, d = 256] = octets;
  return fields.length === 4 && octets.every((octet) => octet <= 255) ? [a, b, c, d] : undefined;
}
