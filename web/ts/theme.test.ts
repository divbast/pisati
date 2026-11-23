// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

import { afterEach, beforeEach, describe, expect, it } from "vite-plus/test";

import { enhanceThemeForms, type Fetch } from "./theme.ts";

// What the page would send were the script not there: each submit event
// that reached the document, whether a handler had stopped it, and the
// button that sent it. The test stops every one itself afterwards, so
// happy-dom never navigates.
type Submission = { prevented: boolean; submitter: HTMLElement | null };

let submissions: Submission[] = [];

function record(event: SubmitEvent): void {
  submissions.push({ prevented: event.defaultPrevented, submitter: event.submitter });
  event.preventDefault();
}

// The theme form as the server renders it: three buttons, each naming a
// theme, and the page to come back to.
function themeForm(): HTMLFormElement {
  document.body.innerHTML = `
    <form method="post" action="/_/theme" data-theme-form>
      <input type="hidden" name="next" value="/posts/1?page=2">
      <button name="theme" value="light" aria-pressed="false">Light</button>
      <button name="theme" value="dark" aria-pressed="false">Dark</button>
      <button name="theme" value="auto" aria-pressed="true">Auto</button>
    </form>`;
  const form = document.querySelector("form");
  if (form === null) {
    throw new Error("the fixture has no form");
  }
  return form;
}

function button(form: HTMLFormElement, theme: string): HTMLButtonElement {
  const found = form.querySelector<HTMLButtonElement>(`button[value="${theme}"]`);
  if (found === null) {
    throw new Error(`the fixture has no ${theme} button`);
  }
  return found;
}

// A fetch that records each call and answers with what respond gives it.
function fakeFetch(respond: () => Promise<Response>): {
  fetch: Fetch;
  calls: { url: string; init: RequestInit | undefined }[];
} {
  const calls: { url: string; init: RequestInit | undefined }[] = [];
  const fetch: Fetch = async (input, init) => {
    calls.push({ url: input, init });
    return respond();
  };
  return { fetch, calls };
}

// Lets the handler's awaited fetch, and whatever follows it, run: a timer
// fires only once every promise before it has settled.
async function settle(): Promise<void> {
  await new Promise((resolve) => {
    setTimeout(resolve, 0);
  });
}

beforeEach(() => {
  submissions = [];
  document.documentElement.dataset["theme"] = "auto";
  document.addEventListener("submit", record);
});

afterEach(() => {
  document.removeEventListener("submit", record);
  document.body.innerHTML = "";
});

describe("enhanceThemeForms", () => {
  it("posts the chosen theme with fetch and applies it without a reload", async () => {
    const form = themeForm();
    const { fetch, calls } = fakeFetch(async () => new Response(null, { status: 200 }));
    enhanceThemeForms(document, fetch);

    form.requestSubmit(button(form, "dark"));
    await settle();

    expect(document.documentElement.dataset["theme"]).toBe("dark");
    expect(submissions).toEqual([{ prevented: true, submitter: button(form, "dark") }]);
    expect(calls).toHaveLength(1);
    const call = calls[0];
    expect(call?.url).toBe(new URL("/_/theme", document.baseURI).href);
    expect(call?.init?.method).toBe("POST");
    expect(call?.init?.mode).toBe("same-origin");
    expect(call?.init?.redirect).toBe("manual");
    // The fields the browser would send, encoded as it would send them.
    const body = call?.init?.body;
    expect(body).toBeInstanceOf(URLSearchParams);
    expect(body instanceof URLSearchParams ? body.toString() : body).toBe(
      new URLSearchParams({ next: "/posts/1?page=2", theme: "dark" }).toString(),
    );
    expect(button(form, "dark").getAttribute("aria-pressed")).toBe("true");
    expect(button(form, "light").getAttribute("aria-pressed")).toBe("false");
    expect(button(form, "auto").getAttribute("aria-pressed")).toBe("false");
  });

  it("takes the server's redirect as success", async () => {
    const form = themeForm();
    const redirect = Response.error();
    Object.defineProperty(redirect, "type", { value: "opaqueredirect" });
    const { fetch } = fakeFetch(async () => redirect);
    enhanceThemeForms(document, fetch);

    form.requestSubmit(button(form, "light"));
    await settle();

    expect(document.documentElement.dataset["theme"]).toBe("light");
    expect(submissions).toEqual([{ prevented: true, submitter: button(form, "light") }]);
  });

  const failures: [string, () => Promise<Response>][] = [
    ["the network fails", async () => Promise.reject(new TypeError("network down"))],
    ["the server refuses", async () => new Response("no", { status: 403 })],
    ["the server fails", async () => new Response("no", { status: 500 })],
  ];
  for (const [when, respond] of failures) {
    it(`falls back to the plain form submit when ${when}`, async () => {
      const form = themeForm();
      const { fetch, calls } = fakeFetch(respond);
      enhanceThemeForms(document, fetch);

      form.requestSubmit(button(form, "dark"));
      await settle();

      expect(calls).toHaveLength(1);
      expect(document.documentElement.dataset["theme"]).toBe("auto");
      // The script's own submit is stopped; the one it hands back to the
      // browser goes ahead, from the same button, so the server still
      // gets the theme chosen.
      expect(submissions).toEqual([
        { prevented: true, submitter: button(form, "dark") },
        { prevented: false, submitter: button(form, "dark") },
      ]);
    });
  }

  it("leaves a theme it does not know to the server", async () => {
    const form = themeForm();
    button(form, "dark").value = "sepia";
    const { fetch, calls } = fakeFetch(async () => new Response(null, { status: 200 }));
    enhanceThemeForms(document, fetch);

    form.requestSubmit(button(form, "sepia"));
    await settle();

    expect(calls).toHaveLength(0);
    expect(document.documentElement.dataset["theme"]).toBe("auto");
    expect(submissions).toEqual([{ prevented: false, submitter: button(form, "sepia") }]);
  });

  it("enhances only forms marked as theme forms", async () => {
    const form = themeForm();
    form.removeAttribute("data-theme-form");
    const { fetch, calls } = fakeFetch(async () => new Response(null, { status: 200 }));
    enhanceThemeForms(document, fetch);

    form.requestSubmit(button(form, "dark"));
    await settle();

    expect(calls).toHaveLength(0);
    expect(submissions).toEqual([{ prevented: false, submitter: button(form, "dark") }]);
  });
});
