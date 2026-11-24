// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

// Stylelint's own rules alone, each holding the stylesheets to one point
// of modern CSS.

/**
 * The message for a physical property.
 *
 * @param {string} property
 * @returns {string}
 */
function physicalProperty(property) {
  return `"${property}" is physical: use its logical property`;
}

/**
 * The message for a physical value.
 *
 * @param {string} property
 * @param {string} value
 * @returns {string}
 */
function physicalValue(property, value) {
  return `"${property}: ${value}" names a physical side or axis: use the logical one`;
}

/** @type {import("stylelint").Config} */
export default {
  // A disable comment is refused outright by just fe-check; these report
  // one that slips through all the same.
  reportNeedlessDisables: true,
  reportDescriptionlessDisables: true,
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
      { message: physicalProperty },
    ],
    // The same for values: alignment, floats and resizing by side or axis
    // of the writing direction, and transitions of logical properties.
    "declaration-property-value-disallowed-list": [
      {
        "/^text-align(?:-last)?$/": ["/^(?:left|right)$/"],
        "/^(?:float|clear)$/": ["/^(?:left|right)$/"],
        "/^justify-(?:content|items|self)$/": ["/\\b(?:left|right)\\b/"],
        resize: ["/^(?:horizontal|vertical)$/"],
        "/^(?:transition(?:-property)?|will-change)$/": [
          "/(?:^|[\\s,])(?:(?:min-|max-)?(?:width|height)|top|right|bottom|left)(?:$|[\\s,])/",
          "/\\b(?:margin|padding|border|scroll-margin|scroll-padding)-(?:top|right|bottom|left)\\b/",
        ],
      },
      { message: physicalValue },
    ],
    // Space-separated colour functions, as every modern colour space has.
    "color-function-notation": "modern",
    // Colours are oklch(), never hex and never a colour's name.
    "color-no-hex": true,
    "color-named": "never",
    // Cascade layers decide what wins, never !important.
    "declaration-no-important": true,
    // Classes and elements, never IDs, so specificity stays low and even.
    "selector-max-id": 0,
    // Range queries, such as (width >= 40rem), not min- and max- features.
    "media-feature-range-notation": "context",
  },
};
