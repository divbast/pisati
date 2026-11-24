// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

// The theme form without a page load. The form works on its own: it
// posts the chosen theme to the server, which sets the theme cookie and
// sends the browser back to the page it came from. With this script, the
// same request goes out with fetch, and the page changes its theme where
// it stands. Whatever goes wrong, the form is submitted as it would have
// been without the script, so the reader always gets the theme they chose.
//
// Nothing here runs inline or evaluates a string: it is a module the
// page's policy allows by its nonce, and it only adds listeners.

/** The themes the server knows, as data-theme on <html> names them. */
export type Theme = "light" | "dark" | "auto";

/** The fetch the forms post with, which tests replace. */
export type Fetch = (input: string, init?: RequestInit) => Promise<Response>;

/** How one posted choice ended: applied, failed, or overtaken by a newer one. */
type Outcome = "applied" | "failed" | "overtaken";

const themes: ReadonlySet<string> = new Set<Theme>(["light", "dark", "auto"]);

function isTheme(value: unknown): value is Theme {
  return typeof value === "string" && themes.has(value);
}

/**
 * Enhances each form in doc marked data-theme-form, which posts its
 * buttons' theme (name="theme", value light, dark or auto) and a next page
 * to come back to. On submit, it posts the same fields with post, marks
 * the form aria-busy while the request is out, and applies the theme to
 * <html> once the server has answered with success, setting aria-pressed
 * on the form's theme buttons to show which is chosen. A newer choice
 * cancels the request of an older one, which then changes nothing. A theme
 * it does not know goes to the server as the form would send it. A request
 * that fails, or an answer that is not success, submits the form as the
 * browser would have, with the theme chosen.
 */
export function enhanceThemeForms(doc: Document, post: Fetch): void {
  for (const form of doc.querySelectorAll<HTMLFormElement>("form[data-theme-form]")) {
    enhance(doc, form, post);
  }
}

function enhance(doc: Document, form: HTMLFormElement, post: Fetch): void {
  // Set while this script submits the form itself, after a failure, so
  // that submit goes ahead as the browser's own.
  let fallingBack = false;
  // The newest choice's request, which a newer choice cancels.
  let newest: AbortController | undefined;
  form.addEventListener("submit", (event) => {
    if (fallingBack) {
      return;
    }
    const submitter = event.submitter;
    const fields = new FormData(form, submitter);
    const theme = fields.get("theme");
    if (!isTheme(theme)) {
      return;
    }
    event.preventDefault();
    newest?.abort();
    const request = new AbortController();
    newest = request;
    form.setAttribute("aria-busy", "true");
    void apply(doc, form, fields, theme, post, request.signal).then((outcome) => {
      if (outcome === "overtaken") {
        return;
      }
      form.removeAttribute("aria-busy");
      if (outcome === "failed") {
        fallingBack = true;
        try {
          submitAsBrowser(doc, form, submitter, theme);
        } finally {
          fallingBack = false;
        }
      }
    });
  });
}

/**
 * Posts fields to form's action and, when the server answers with success,
 * applies theme, unless signal says a newer choice has overtaken it.
 */
async function apply(
  doc: Document,
  form: HTMLFormElement,
  fields: FormData,
  theme: Theme,
  post: Fetch,
  signal: AbortSignal,
): Promise<Outcome> {
  const body = new URLSearchParams();
  for (const [name, value] of fields) {
    if (typeof value === "string") {
      body.append(name, value);
    }
  }
  let response: Response;
  try {
    response = await post(form.action, {
      method: "POST",
      body,
      // The theme route is this site's own, so nothing goes elsewhere.
      mode: "same-origin",
      credentials: "same-origin",
      // The server answers by redirecting to the page the form came
      // from, which there is no need to fetch: the redirect itself is
      // the success.
      redirect: "manual",
      signal,
    });
  } catch {
    return signal.aborted ? "overtaken" : "failed";
  }
  // An answer may arrive after its request was cancelled.
  if (signal.aborted) {
    return "overtaken";
  }
  if (!response.ok && response.type !== "opaqueredirect") {
    return "failed";
  }
  doc.documentElement.dataset["theme"] = theme;
  for (const button of form.querySelectorAll<HTMLButtonElement>('button[name="theme"]')) {
    button.setAttribute("aria-pressed", String(button.value === theme));
  }
  return "applied";
}

/**
 * Submits form as the browser would have without this script: from the
 * same button, or, when the browser refuses that because the button has
 * left the form, with the theme in a field of its own, since a plain
 * submit sends no button's value.
 */
function submitAsBrowser(
  doc: Document,
  form: HTMLFormElement,
  submitter: HTMLElement | null,
  theme: Theme,
): void {
  try {
    form.requestSubmit(submitter);
  } catch {
    const field = doc.createElement("input");
    field.type = "hidden";
    field.name = "theme";
    field.value = theme;
    form.append(field);
    form.submit();
  }
}
