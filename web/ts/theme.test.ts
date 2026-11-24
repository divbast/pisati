// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

import { afterEach, beforeEach, expect, it } from "vite-plus/test";

import { enhanceThemeForms, type Fetch } from "./theme.ts";

// What the page would send were the script not there: each submit event
// that reached the document, whether a handler had stopped it, and the
// button that sent it. The test stops every one itself afterwards, so
// happy-dom never navigates.
type Submission = { prevented: boolean; submitter: HTMLElement | null };

const submissions: Submission[] = [];

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

// Each theme button's aria-pressed, as "theme=value".
function pressed(form: HTMLFormElement): string[] {
  return [...form.querySelectorAll("button")].map(
    (b) => `${b.value}=${b.getAttribute("aria-pressed") ?? ""}`,
  );
}

const unpressed = ["light=false", "dark=false", "auto=true"];

type Call = { url: string; init: RequestInit | undefined };

// A fetch that records each call and answers with what respond gives it.
function fakeFetch(respond: () => Promise<Response>): { fetch: Fetch; calls: Call[] } {
  const calls: Call[] = [];
  const fetch: Fetch = (input, init) => {
    calls.push({ url: input, init });
    return respond();
  };
  return { fetch, calls };
}

// A fetch whose answers the test gives, one call at a time, and which, like
// the browser's, fails a request once its signal is aborted.
function heldFetch(): {
  fetch: Fetch;
  held: { answer: (r: Response) => void; init: RequestInit | undefined }[];
} {
  const held: { answer: (r: Response) => void; init: RequestInit | undefined }[] = [];
  const fetch: Fetch = (_input, init) =>
    new Promise((resolve, reject) => {
      init?.signal?.addEventListener("abort", () => {
        reject(new DOMException("The request was aborted.", "AbortError"));
      });
      held.push({ answer: resolve, init });
    });
  return { fetch, held };
}

const ok = (): Promise<Response> => Promise.resolve(new Response(null, { status: 200 }));

// Lets the handler's awaited fetch, and whatever follows it, run: a timer
// fires only once every promise before it has settled.
async function settle(): Promise<void> {
  await new Promise((resolve) => {
    setTimeout(resolve, 0);
  });
}

beforeEach(() => {
  submissions.length = 0;
  document.documentElement.dataset["theme"] = "auto";
  document.addEventListener("submit", record);
});

afterEach(() => {
  document.removeEventListener("submit", record);
  document.body.innerHTML = "";
});

it("posts the chosen theme with fetch and applies it without a reload", async () => {
  const form = themeForm();
  const { fetch, calls } = fakeFetch(ok);
  enhanceThemeForms(document, fetch);

  form.requestSubmit(button(form, "dark"));
  expect(form.getAttribute("aria-busy")).toBe("true");
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
  expect(pressed(form)).toEqual(["light=false", "dark=true", "auto=false"]);
  expect(form.hasAttribute("aria-busy")).toBe(false);
});

it("takes the server's redirect as success", async () => {
  const form = themeForm();
  const redirect = Response.error();
  Object.defineProperty(redirect, "type", { value: "opaqueredirect" });
  const { fetch } = fakeFetch(() => Promise.resolve(redirect));
  enhanceThemeForms(document, fetch);

  form.requestSubmit(button(form, "light"));
  await settle();

  expect(document.documentElement.dataset["theme"]).toBe("light");
  expect(submissions).toEqual([{ prevented: true, submitter: button(form, "light") }]);
});

it.each<[string, () => Promise<Response>]>([
  ["the network fails", () => Promise.reject(new TypeError("network down"))],
  ["the server refuses", () => Promise.resolve(new Response("no", { status: 403 }))],
  ["the server fails", () => Promise.resolve(new Response("no", { status: 500 }))],
])("falls back to the plain form submit when %s", async (_when, respond) => {
  const form = themeForm();
  const { fetch, calls } = fakeFetch(respond);
  enhanceThemeForms(document, fetch);

  form.requestSubmit(button(form, "dark"));
  await settle();

  expect(calls).toHaveLength(1);
  expect(document.documentElement.dataset["theme"]).toBe("auto");
  expect(pressed(form)).toEqual(unpressed);
  expect(form.hasAttribute("aria-busy")).toBe(false);
  // The script's own submit is stopped; the one it hands back to the
  // browser goes ahead, from the same button, so the server still gets
  // the theme chosen.
  expect(submissions).toEqual([
    { prevented: true, submitter: button(form, "dark") },
    { prevented: false, submitter: button(form, "dark") },
  ]);
});

it("falls back with the theme in a field of its own once the button is gone", async () => {
  const form = themeForm();
  const { fetch, held } = heldFetch();
  enhanceThemeForms(document, fetch);
  form.requestSubmit(button(form, "dark"));

  // The browser refuses to submit from a button that has left the form,
  // and a plain submit sends no button's value.
  Object.defineProperty(form, "requestSubmit", {
    value: () => {
      throw new DOMException("The button is not in the form.", "NotFoundError");
    },
  });
  const sent: string[] = [];
  Object.defineProperty(form, "submit", {
    value: () => {
      const fields = new URLSearchParams();
      for (const [name, value] of new FormData(form)) {
        fields.append(name, typeof value === "string" ? value : value.name);
      }
      sent.push(fields.toString());
    },
  });
  held[0]?.answer(new Response("no", { status: 500 }));
  await settle();

  expect(sent).toEqual([
    new URLSearchParams({ next: "/posts/1?page=2", theme: "dark" }).toString(),
  ]);
  expect(pressed(form)).toEqual(unpressed);
});

it("applies only the newest choice when the reader changes their mind", async () => {
  const form = themeForm();
  const { fetch, held } = heldFetch();
  enhanceThemeForms(document, fetch);

  form.requestSubmit(button(form, "dark"));
  form.requestSubmit(button(form, "light"));
  expect(held.map((h) => h.init?.signal?.aborted)).toEqual([true, false]);
  await settle();
  // The overtaken request failing is no reason to fall back.
  expect(submissions).toHaveLength(2);
  expect(form.getAttribute("aria-busy")).toBe("true");

  held[1]?.answer(new Response(null, { status: 200 }));
  await settle();

  expect(document.documentElement.dataset["theme"]).toBe("light");
  expect(pressed(form)).toEqual(["light=true", "dark=false", "auto=false"]);
  expect(form.hasAttribute("aria-busy")).toBe(false);
  expect(submissions).toEqual([
    { prevented: true, submitter: button(form, "dark") },
    { prevented: true, submitter: button(form, "light") },
  ]);
});

it("ignores an overtaken choice that is answered anyway", async () => {
  const form = themeForm();
  const answers: ((r: Response) => void)[] = [];
  // A fetch that answers whatever its signal says, as one may when the
  // answer was already on its way.
  const fetch: Fetch = () =>
    new Promise((resolve) => {
      answers.push(resolve);
    });
  enhanceThemeForms(document, fetch);

  form.requestSubmit(button(form, "dark"));
  form.requestSubmit(button(form, "light"));
  answers[1]?.(new Response(null, { status: 200 }));
  await settle();
  answers[0]?.(new Response(null, { status: 200 }));
  await settle();

  expect(document.documentElement.dataset["theme"]).toBe("light");
  expect(pressed(form)).toEqual(["light=true", "dark=false", "auto=false"]);
});

it("leaves a theme it does not know to the server", async () => {
  const form = themeForm();
  button(form, "dark").value = "sepia";
  const { fetch, calls } = fakeFetch(ok);
  enhanceThemeForms(document, fetch);

  form.requestSubmit(button(form, "sepia"));
  await settle();

  expect(calls).toHaveLength(0);
  expect(document.documentElement.dataset["theme"]).toBe("auto");
  expect(submissions).toEqual([{ prevented: false, submitter: button(form, "sepia") }]);
});

it("enhances only forms marked as theme forms", async () => {
  const form = themeForm();
  delete form.dataset["themeForm"];
  const { fetch, calls } = fakeFetch(ok);
  enhanceThemeForms(document, fetch);

  form.requestSubmit(button(form, "dark"));
  await settle();

  expect(calls).toHaveLength(0);
  expect(submissions).toEqual([{ prevented: false, submitter: button(form, "dark") }]);
});
