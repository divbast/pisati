// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

// The script pages that have something to enhance load. Every page works
// without it: it only makes what the server already does quicker.

import { enhanceThemeForms } from "./theme.ts";

enhanceThemeForms(document, async (input, init) => fetch(input, init));
