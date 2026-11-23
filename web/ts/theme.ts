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

const themes: ReadonlySet<string> = new Set<Theme>(["light", "dark", "auto"]);

function isTheme(value: unknown): value is Theme {
  return typeof value === "string" && themes.has(value);
}

/**
 * Enhances each form in doc marked data-theme-form, which posts its
 * buttons' theme (name="theme", value light, dark or auto) and a next page
 * to come back to. On submit, it posts the same fields with post, applies
 * the theme to <html> once the server has answered with success, and sets
 * aria-pressed on the form's theme buttons to show which is chosen. A
 * theme it does not know goes to the server as the form would send it.
 * A request that fails, or an answer that is not success, submits the form
 * as the browser would have, from the same button.
 */
export function enhanceThemeForms(doc: Document, post: Fetch): void {
  for (const form of doc.querySelectorAll<HTMLFormElement>("form[data-theme-form]")) {
    // Set while this script submits the form itself, after a failure, so
    // that submit goes ahead as the browser's own.
    let fallingBack = false;
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
      void apply(doc, form, fields, theme, post).then((applied) => {
        if (applied) {
          return;
        }
        fallingBack = true;
        try {
          form.requestSubmit(submitter);
        } catch {
          // The button has left the form since: submit without it.
          form.submit();
        } finally {
          fallingBack = false;
        }
      });
    });
  }
}

/**
 * Posts fields to form's action and, when the server answers with success,
 * applies theme. It reports whether it did: false for a request that
 * failed or an answer that is not success.
 */
async function apply(
  doc: Document,
  form: HTMLFormElement,
  fields: FormData,
  theme: Theme,
  post: Fetch,
): Promise<boolean> {
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
    });
  } catch {
    return false;
  }
  if (!response.ok && response.type !== "opaqueredirect") {
    return false;
  }
  doc.documentElement.dataset["theme"] = theme;
  for (const button of form.querySelectorAll<HTMLButtonElement>('button[name="theme"]')) {
    button.setAttribute("aria-pressed", String(button.value === theme));
  }
  return true;
}
