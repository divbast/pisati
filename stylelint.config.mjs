// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

// Stylelint's own rules alone, each holding the stylesheets to one point
// of modern CSS.

/** @type {import("stylelint").Config} */
export default {
  rules: {
    // Sizes, spacing, borders and positions are logical (inline and block,
    // start and end), so a right-to-left page mirrors with no rule of its own.
    "property-disallowed-list": [
      [
        "/^(?:min-|max-)?(?:width|height)$/",
        "/^(?:top|right|bottom|left|inset)$/",
        "/^(?:margin|padding|scroll-margin|scroll-padding)-(?:top|right|bottom|left)$/",
        "/^border-(?:top|right|bottom|left)(?:-(?:width|style|color))?$/",
        "/^border-(?:top|bottom)-(?:left|right)-radius$/",
        "/^overscroll-behavior-[xy]$/",
        "/^contain-intrinsic-(?:width|height)$/",
      ],
      {
        message: (/** @type {string} */ property) =>
          `"${property}" is physical: use its logical property`,
      },
    ],
    // Space-separated colour functions, as every modern colour space has.
    "color-function-notation": "modern",
    // Colours are oklch(), never hex.
    "color-no-hex": true,
    // Cascade layers decide what wins, never !important.
    "declaration-no-important": true,
    // Classes and elements, never IDs, so specificity stays low and even.
    "selector-max-id": 0,
    // Range queries, such as (width >= 40rem), not min- and max- features.
    "media-feature-range-notation": "context",
  },
};
