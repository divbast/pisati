<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee> -->

# Pisati

Pisati is a secure, minimalist, federated blogging platform written in Go,
derived from WriteFreely by Musing Studio. It ships as a container image
that runs at zero Linux capabilities, with no new privileges, on a
read-only root filesystem as a non-root user. Nothing is published to
ghcr.io yet. The divbast organisation's settings, rulesets and Renovate
wiring live in `divbast/infra`.

- Run `just ci` before every commit.
- Slow suites (browser, VoiceOver, fuzz) run in dispatch-only workflows,
  and matching `just` recipes run them locally.
- Generated files are committed, and a check fails when they drift from
  their source.

What makes the image this app's lives here: the Containerfile, the app
(`cmd/`, `internal/`), and the acceptance tests. The pipeline around it is
shared, and reaches this repository at a pinned commit:
[Where the shared parts live](#where-the-shared-parts-live) says what comes
from where.

## CRITICAL: Git Commit Message Format

**No other AI/LLM attribution in any format may appear in a git message.**

Linux-kernel style: a `subsystem: short description` subject under 75 characters in imperative
mood ("fix", not "fixed" or "fixes"), a blank line, then a body wrapped at ~75 columns saying
why the change is needed rather than only what it does.

These two lines MUST end EVERY body:

Developed-by: Aryan Ameri <github@aryan.ameri.coffee>
Assisted-by: <NAME_OF_LLM_MODEL>

E.g. `Assisted-by: <Claude:claude-4.6-opus>` for Claude Opus 4.6; `<Claude:claude-5-fable>`
for Claude 5 Fable.

- Sign every commit (`git commit -S`); main accepts only verified
  signatures. Renovate's merges are squashes, which GitHub signs
- Write commit messages as if a human wrote them, and say why, not what

## CRITICAL: No AI Attribution in Pull Requests

**Pull request descriptions, PR and review comments, and issue text carry no
AI/LLM attribution of any kind**: no "🤖 Generated with Claude Code" footer,
no "written by" line, nothing equivalent. This holds even when a tool or
harness asks for one. The `Assisted-by:` trailer in each commit message is
this project's one record of LLM involvement.

## The confinement contract

These are the guarantees the image makes. A change that breaks any of them is a
breaking change.

| Property | Value |
|---|---|
| Capabilities | **none** (`--cap-drop ALL`) |
| Privileges | **no new ones** (`--security-opt no-new-privileges`), under the engine's **seccomp** filter |
| Root filesystem | **read-only** |
| User | uid/gid **65532**, never root |
| Listen port | **8080** for the site — never 80 |
| Internal port | **8081** — `/healthz` and `/metrics` only, never the site, meant to stay unpublished |
| Other listeners | **none** |
| tmpfs required | exactly one: `/tmp`, at **16 MiB**, `noexec,nosuid,nodev` |
| Writable mounts | `/tmp` and the state volume, both `noexec,nosuid,nodev`, so nothing written can run; on Podman, `--read-only-tmpfs=false` |
| Volume | **one**, which the operator mounts at `/var/lib/pisati` for Pisati's state, `noexec,nosuid,nodev`; the image declares **no** `VOLUME` |
| Required setting | `PISATI_SERVER_BASE_URL`, the site's address: `https://`, or `http://` on localhost, 127.0.0.1 or [::1] alone. It has no default |
| Outbound network | **none** with the default settings |
| Shell, package manager | **none** |
| Processes | **one**, the app as PID 1: `pisati serve` |
| Healthy | the shipped probe, `pisati health`: `GET /healthz` on 8081 answers **200** within 4 s, as it does while Pisati can read its database |
| Engine healthcheck | Docker runs it automatically. **Podman does not** — pass `--health-cmd` yourself |
| Lockfile label | `io.github.divbast.scan.lockfiles` is `/usr/share/doc/pisati/pnpm-lock.yaml`, the lockfile the image holds, whose packages divbast/ci's scan reads from its v4.25.0 |

- **Podman never sees this image's `HEALTHCHECK`.** Its `libimage` reads a
  healthcheck only from a Docker-media-type manifest; divbast images publish
  as an OCI index, so Podman's OCI inspection branch never reaches the
  config's healthcheck field — no version of Podman does. Give Podman users
  `--health-cmd '["/usr/local/bin/pisati","health"]'`
  (a Quadlet's `HealthCmd=` takes the same value) — no leading `"CMD"`,
  because Podman before 5.8.0 re-splits such an array into one token instead
  of running it. Tracked upstream as podman/podman#25454 and #18904. The
  contract suite checks the image's `HEALTHCHECK` is that array in exec
  form, and has each engine judge the container: Docker by the image's
  `HEALTHCHECK`, Podman by the same array passed as `--health-cmd`
- **The shipped probe asks `/healthz` alone.** `pisati health` sends
  `GET /healthz` to the host `server.internal_listen` names, or 127.0.0.1
  when it names none. `/healthz` answers 200 `ok` while the database file
  is at its path and Pisati reads it from disk within 2 s, and 503
  `unavailable` otherwise, with the reason in `pisati serve`'s log alone.
  It never goes through 8080's routes: the acceptance tests do. It prints
  nothing when healthy, which the contract's `health-probe` check asserts;
  each failure is a line, `<check>: <reason>`: with no shell, the engine's
  health log is the only record of why
- **8080 serves the site, and 8081 only health and metrics.** 8080
  answers every path the site has no page for, `/index.php`, `/healthz`
  and `/metrics` among them, with the site's HTML 404 page. 8081 answers
  `/healthz` and `/metrics`, and the profiler only when `telemetry.pprof`
  is on, and an empty 404 to everything else, `/` and `/login` included.
  `TestServes` asks 8080 for `/index.php`, `/healthz` and a path nothing
  names, and fails on any answer but the 404 page;
  `TestMetricsOnlyInternal` fails on `/metrics` answering on 8080, or not
  on 8081; the contract's `health-port-only` check fails on `/healthz`
  answering on 8080, or `/` or `/login` answering on 8081
- **Nothing listens but 8080 and 8081.** The contract suite reads the
  kernel's socket tables and fails on any other TCP port a process in the
  container listens on, and on any UDP socket one holds
- **One process, always, and it stops when told.** The app is the
  ENTRYPOINT, so it is PID 1. Nothing runs before it: Pisati reads its
  settings and checks its state directory itself as it starts, and exits
  1 rather than start when either is wrong. The kernel drops a signal
  PID 1 does not handle, so the app handles SIGTERM and SIGINT itself,
  finishes what it is doing within one 8 s deadline and exits 0;
  `TestStopsPromptly` fails on a stop that takes 9 s or more, or any other
  exit. Do not add a supervisor or a second long-running process
- **Pisati refuses to start without its state volume or its base URL.**
  On the read-only root, or on a read-only volume, `pisati serve` exits 1,
  naming `/var/lib/pisati`; with `PISATI_SERVER_BASE_URL` unset, or plain
  http away from localhost, it exits 1, naming the variable and never its
  value. `TestRefusesReadOnlyState`, `TestRefusesWithoutBaseURL` and
  `TestRefusesPlainHTTPBaseURL` fail on any other outcome
- **Zero egress.** With its default settings, the app opens no connection
  out. The contract suite proves the image serves and is healthy on a
  genuinely `--internal` network, and cannot connect out
- **Nothing may add a shell or a package manager to the final image.** The
  base is Docker Hardened Images' `static`; that is the point, and the
  contract's `no-shell` and `no-package-manager` checks hold it there. What
  it removes is us running apt, not the patching itself: it ships CA
  certificates and time zones. That obligation moves to the base image's own rebuilds —
  and **nothing in this repository collects them on its own.** Rebuilding
  an unchanged Containerfile does not pick them up. **Renovate is the
  update path**: it runs every morning, after every merge to main and
  whenever CI finishes on one of its branches, from
  `.github/workflows/renovate.yml`, which calls divbast/ci's
  `renovate.yml`, as the org's `divbast-renovate` App, managed from
  `divbast/infra`. After a merge it rebases every open PR left behind
  main, since GitHub re-runs no checks when a base moves and a green check
  on a stale base proves nothing. **It merges its own PRs** once every
  check passes, with a squash, while the repository variable
  `PISATI_AUTOMERGE` is `enabled`; any other value stops
  merging from the next run. A new major of divbast/ci, a raise of
  `go.mod`'s `go` line wait for approval on the Dependency Dashboard
  first. **Nothing is published:** `build.yml` has no publish job.
  `.github/workflows/stall.yml`, through divbast/ci's `stall.yml`, fails
  each day that updates have stopped flowing: a Renovate PR stuck, or
  Renovate silent
- **Every image the Containerfile builds from is verified first.** Before
  `app-check.yml` builds anything, `divbast-verify-images` refuses any image
  not pinned as `<name>:<tag>@sha256:<digest>`, and verifies Docker's
  signature on each Docker Hardened Image: the Go builder, the Node stage
  and the `static` runtime. A local `just build` does not
  verify; `just ci-tools && .ci/bin/divbast-verify-images` does, with
  cosign from `mise.toml`
- **The binary must never carry a file capability.** A binary with a file
  capability cannot be exec'd at all under `--cap-drop ALL` — it dies with
  `Operation not permitted` and exit 126 — and the contract suite's
  `file-capability` check, which runs `pisati version`, fails on one, as
  `no-file-capability-xattr` fails on one anywhere in the image
- **The application is one root-owned file, never written.** The binary
  is `COPY`'d from the build stage. Pisati writes its state, the database,
  its lock and the main key it generates, to the volume at
  `/var/lib/pisati` alone: the image holds that directory empty, owned by
  65532 with mode 0700, so an empty volume mounted there starts out the
  same, and Pisati sets its umask to 077, so every file it makes is 0600
  and every directory 0700. `TestStatePersists` fails on any other mode
  there, and unless a new container on the same volume finds the
  database and the main key the first one left
- **The image holds the lockfile its front end was built from.** The
  stylesheets and scripts are compiled into the binary, and no
  `node_modules` ships. The `pnpm-lock.yaml` they were built from is at
  `/usr/share/doc/pisati/pnpm-lock.yaml`, and `TestImageCarriesLockfile`
  fails unless that is this repository's own. The image's label
  `io.github.divbast.scan.lockfiles` names it, and
  `TestImageNamesItsLockfile` fails unless the label is exactly that path

## Where the shared parts live

- **divbast/ci.** `just setup` checks it out into `.ci/`, which git and
  the build context both ignore, at the one commit every workflow calls it
  at: a release's full SHA, with its tag as the comment. Its README is the
  reference for all of it:
  - the reusable workflows: `app-check.yml` (lint; the image built for
    amd64 and arm64 once every image it builds from verifies; the acceptance
    suite on each under Docker and rootless Podman; the scan),
    `renovate.yml` and `stall.yml`. `build.yml`, `renovate.yml` and
    `stall.yml` here call them, and keep their own triggers and the
    `required` job;
  - the recipes the justfile imports, `.ci/just/common.just` and
    `.ci/just/app.just`. Recipes here call them from a recipe's body, never
    as a dependency, so the justfile still parses before `.ci` exists;
  - the Go tools, which `just ci-tools` builds into `.ci/bin/`, run from
    this repository's root: `divbast-pin`, `divbast-verify-images`,
    `divbast-scan`, `divbast-testcount`, `divbast-published-revision`,
    `divbast-stall` and the rest;
  - the acceptance `harness` and the `contract` suite, which
    `test/acceptance/go.mod` requires through
    `replace github.com/divbast/ci => ../../.ci`: the suite is a Go module of
    its own, so the root module imports nothing from ci;
  - `.github/actionlint.yaml`. golangci-lint runs under this repository's
    own `.golangci.yml`, never ci's `go/golangci.yml`.

  A change to ci reaches this repository only through the Renovate PR that
  moves every pin of it at once. `just setup`, and the pin check
  (`divbast-pin`) in each ci workflow that runs this repository's code,
  refuse workflows that call ci at more than one commit
- **divbast/renovate-config** holds the presets `renovate.json` extends.
  `renovate.json` adds rules of its own: templ's runtime and its generator
  move in one PR; npm packages wait 7 days after release, as pnpm's
  `minimumReleaseAge` makes the install do; and pnpm moves in `mise.toml`
  and `package.json`'s `packageManager` together
- **divbast/infra** holds the org's settings, this repository's rulesets,
  the `divbast-renovate` App, the `renovate`
  environment and `PISATI_AUTOMERGE`

## Key commands

- `just setup` — fetch divbast/ci into `.ci/` at the commit the workflows
  pin, then install the pinned toolchain and, with `pnpm install
  --frozen-lockfile`, the front end's packages. One command on a
  fresh clone, and again whenever the pin moves; `just lint` fails, naming
  it, when `.ci` is not at the pinned commit (`ci-fresh`)
- `just build` — build the image locally. It does not verify the images it
  builds from; `just ci-tools && .ci/bin/divbast-verify-images` does, with
  cosign from `mise.toml`
- `just acceptance` — `go test` inside `test/acceptance`, its own module,
  against the image
  `just build` wrote, under `ENGINE` (docker unless it says podman),
  through `divbast-testcount`, which fails the run unless a test passed and
  none failed or skipped. `just IMAGE=… acceptance` tests another image.
  `ENGINE=docker IMAGE=localhost/pisati:dev mise exec -- go
  -C test/acceptance test -run TestServes ./...` runs one test
- `just test` — build, then the acceptance suite
- `just lint` — pinact, zizmor, actionlint, ShellCheck and shfmt,
  `generate-check`, the Go
  gates (`go-gates`, `go-unit`, `vuln` and `secrets`), `fe-check`,
  `reuse lint`, hadolint and `docker build --check`. `go-gates` runs the
  compiler, `go fix -diff`, `go mod tidy -diff`, `go mod verify` and `golangci-lint
  run`, which reports formatting too, in the root module and the
  acceptance module, and tidies and verifies `tools/`. It also fails
  unless both modules require the same goleak, and on any suppression
  but `//nolint`: another linter's own suppression comment, betterleaks'
  allow comment, or a betterleaks ignore file. `go-unit` runs the root
  module's tests under the race detector, in random order; the
  acceptance module is not part of it. `vuln` runs govulncheck over both
  modules. `secrets` runs betterleaks over the history of every ref the
  clone holds: all of it locally, and in CI once the lint job checks out
  the full history
- `just fe-check` — the front end's gates: no suppression comment at
  all, of Oxlint, ESLint, Stylelint, TypeScript or Oxfmt, and no ignore
  file they read (`.stylelintignore`, `.prettierignore`, `.oxlintignore`,
  `.eslintignore`, `.oxfmtignore`), nor templ's (`.templignore_generate`,
  `.templignore_fmt`), tracked anywhere or present at the
  root (a noisy rule is switched off by name, with its reason, in
  `vite.config.ts` or `stylelint.config.mjs` instead). The repository
  needs `.gitignore`, so the gate can't refuse it, though Oxlint and Oxfmt
  skip what it lists too: its entries are reviewed like the configs' own
  ignore lists; `vp check` (Oxfmt, Oxlint with the
  correctness, suspicious, perf and pedantic categories as errors, and the
  TypeScript type checks); `vp test` (Vitest, on happy-dom, with a test of
  the colour contrast `web/css/theme.css` gives); Stylelint
  over `web/**/*.css`; and `pnpm audit --prod`
- `just fe-build` — `vp build` the stylesheets and scripts under `web/`
  into `internal/web/dist`, which `go:embed` compiles into the binary.
  Only `PLACEHOLDER` there is tracked: the build output never is
- `vp dev` (`pnpm exec vp dev`) serves the same sources for Pisati's dev
  mode, which is on only when `dev.vite_url` is set. Vite reads the
  environment alone, never `pisati.toml`: `PISATI_DEV_VITE_URL`, which
  is `http://localhost:5173` to Vite when unset or empty, and, for an
  https URL, `PISATI_DEV_VITE_CERT_FILE` and `PISATI_DEV_VITE_KEY_FILE`,
  which only `vp dev` needs. A certificate or key variable that is set
  must hold an absolute, clean path, for every command, as Pisati refuses
  any other, and `vite.devserver.ts` refuses a `PISATI_DEV_VITE_URL`
  that Pisati refuses as `dev.vite_url`, without repeating it. Each side's
  test reads one table of cases for each rule, in
  `internal/config/testdata/`. So a dev server on a port other than 5173,
  or over https, takes these variables, which Pisati and Vite both read
- `just generate` — regenerate the committed generated code: templ's
  pages from each `.templ` file (`go tool -modfile=tools/go.mod templ
  generate`), sqlc's queries (`go tool -modfile=tools/go.mod sqlc
  generate`), and `internal/web/icons` from `lucide-static` in
  `node_modules` (`go run ./cmd/genicons`, which keeps only the SVG
  Lucide draws with, and marks the file with the icons' licences).
  `just generate-check`, part of `lint`, runs it, then `sqlc diff`, and
  fails on any change to a tracked file and on a generated file never
  committed; it needs a tree with nothing unstaged
- `just dev` — Pisati in dev mode: `vp dev` beside `templ generate
  --watch`, which regenerates the pages, rebuilds and restarts `pisati
  serve` as files change, and serves it through its proxy on port 7331,
  which reloads the browser. It sets nothing for you: Pisati reads its
  own settings, `dev.vite_url` among them
- `just archive`, `just scan` — write the image as an OCI archive, then run
  `divbast-scan` over it and the published `:latest`. Needs network access
  to ghcr.io
- `just actions-pin`, `just actions-update` — move the workflows' action pins
- `just ci` — every gate, locally. Run this before committing

## Where the toolchain comes from

- **`mise.toml` is where tool versions are written.** Go, golangci-lint,
  Node, pnpm, betterleaks, hadolint, ShellCheck, shfmt, actionlint, reuse,
  just, pinact, trivy, grype, zizmor and cosign are all listed there. The
  front end's own tools (Vite+, TypeScript, Lightning CSS, Stylelint and
  happy-dom), and `lucide-static`, which `cmd/genicons` reads the icons
  from, are pinned in `package.json` and `pnpm-lock.yaml`, and pnpm
  installs only releases at least 7 days old (`pnpm-workspace.yaml`). Every
  justfile recipe but `setup`, which installs them, runs through `mise
  exec`, so a recipe resolves those binaries and not a same-named one on
  PATH. The recipes that call GitHub,
  pinact's and zizmor's, take a fine-grained token from `GITHUB_TOKEN` and
  nothing else, as divbast/ci's README says; CI passes its own
- **Go builds with the toolchain mise installs and nothing else.**
  `GOTOOLCHAIN=local` keeps `go.mod`'s line from downloading another. And
  `GOFLAGS=-mod=readonly`, which is the default outside vendor mode, and
  still refuses to edit `go.mod`. Go tools live in their own module,
  `tools/go.mod`, and run as `go tool -modfile=tools/go.mod <tool>`
- **`just lint` needs a running Docker daemon**, for `docker build --check`,
  logged in to dhi.io for the Containerfile's images, which `--check`
  reads. `--check` is a buildx flag with no podman
  equivalent, so that gate does not honour `ENGINE` and does not run
  without dockerd
- **`just scan` needs `skopeo` on `PATH`.** It reads the published image with
  [skopeo](https://github.com/containers/skopeo), which no `mise.toml` entry
  provides, so `just ci` needs it too. CI runners already have it
- Licensing for `renovate.json` lives in a
  `.license` sidecar. JSON has no comment syntax, and an SPDX tag written as
  a string value carries the closing quote into the expression, which `reuse`
  rejects

## Code quality

- All linter gates are enforced as errors — fix them, don't suppress them. Any
  suppression, anywhere, needs an explicit, comment-level justification beside
  it to be accepted
- **The scan blocks only what a change would add, and is advisory while
  nothing is published.** `divbast-scan` scans the candidate and the
  published `:latest` for the same arch from one Trivy database, and fails
  only on a HIGH or CRITICAL finding, the same vulnerability in the same
  package, that the published image lacks. No release can make an
  advisory against something already shipped any worse, so blocking on one
  only holds back whatever else the release fixes. Whether anything is
  published is the registry's to say, never Trivy's: the scan reads
  `:latest` as an anonymous client, and when ghcr.io refuses, asks GitHub
  why, with the check job's token, which is what its `packages: read` is
  for. Not published yet, or a package still private: every finding is
  reported, on the summary page, as a warning annotation and on stderr,
  and none blocks. Once the image is public, only new findings block. A
  public image the registry refuses, a registry that cannot be reached, or
  GitHub not answering fails the scan rather than waving it through. A
  local `just scan` never asks GitHub, so a refusal there is advisory,
  with a warning that only CI can tell what it means
- REUSE-compliant SPDX headers on every file; licence is **AGPL-3.0-only**.
  The image's licences label names only that; bundled third-party files
  keep their own licence, and REUSE and the files' SPDX headers say which
  file carries which
- **The acceptance suite is Go, on divbast/ci's harness.** Every
  container it starts runs under `harness.Contract("16m")`, the *exact*
  confinement the contract claims, `--security-opt no-new-privileges` and
  `--tmpfs /tmp:size=16m,noexec,nosuid,nodev` included, so a
  check can never pass under looser settings than we ship. What CI proves
  is what ships. `TestContract` runs ci's contract suite against the image;
  `TestServes` and `TestStopsPromptly` are the app's own. Each serves through
  the harness's `Serve` (or `StartServing`, for a container that must be
  started apart from its first request), which publishes on the host's
  loopback at ports the engine picks, so no two tests, or two suites on one
  host, ask for the same one. The assertions (`StatusIs`, `Answers`,
  `Refuses` and the rest) are the harness's too, shared with every divbast
  image. An app gives each thing it serves a test, one topic per file
- **Go is modern and idiomatic, and linted strictly**, under
  `.golangci.yml`, which sets the Go Development Principles below. Bash
  is for what is short and will stay so: the justfile's recipes. A check
  that runs inside a Containerfile build stage is the exception, since a
  build stage has bash and no Go: it stays bash, under ShellCheck
  `enable=all`, shfmt and `set -euo pipefail`, with a Go test that runs
  it against fixtures and sees each way it can fail fail
- A test suite that has never failed proves nothing. When adding a check, prove
  it can fail before trusting it

# Go Development Principles

## Scope

Prefer modern APIs to older substitutes. 

Prefer guarantees from the compiler, then from package boundaries, then from tooling, then from runtime checks. Where a rule would cost more readability than the mistake it prevents, ask the operator for direction on how to proceed.

## Types

**Start from the types.** Design domain types and function signatures before behaviour.

**Model alternatives as sealed interfaces.** Use an interface with an unexported marker method and one type per variant, each holding only the fields valid for it. Consume it with a type switch that lists every variant and treats an unknown one as a bug.

**Give domain concepts their own types.** Use defined types to keep confusable values apart, and constants of a defined type for a closed set of plain values, instead of bare primitives and boolean parameters.

**Guard invariants behind constructors.** A type with a rule is a struct with unexported fields, built only by a constructor that returns `(X, error)`, with no method that can break the rule.

**Design the zero value.** It is either valid and useful or detectably invalid. In a closed set it means unspecified unless there is a natural default.

**Make absence explicit.** In domain code, return `(T, bool)` rather than a pointer. A pointer signals sharing or mutation.

## Functions

**Narrow inputs, precise outputs.** Accept the narrowest type that works, using standard interfaces where they fit. Declare a new behavioural interface only when a second implementation or a test needs it, in the package that consumes it. Return the most specific type that is useful, never `any`; errors are always returned as `error`.

**Show effects in the signature.** A function that may block or do I/O takes a leading `ctx context.Context`. Every other dependency arrives as a parameter, built once in `main` from configuration read once. Outside `main`, nothing uses package-level state that changes after initialisation, `init` side effects, the environment, the wall clock or a random source other than `crypto/rand`.

**Fail with an error.** Every input gets a result or an error, never a panic and never a nil result with a nil error. Panic only for bugs, and recover only at the top of a request or goroutine.

**Make errors part of the API.** Provide sentinel values or error types for the conditions callers branch on. Inspect them with `errors.Is` and `errors.AsType`, and report independent failures together with `errors.Join`.

**Handle each error exactly once.** Return it or log it, never both. Discard one only explicitly and never the error from `Close` on anything written.

**Copy at ownership boundaries.** Clone slices and maps when storing a caller's data or returning internal state.

**Generalise late.** Add type parameters on the second or third real use. Never type-switch or reflect on a value of type-parameter type. A method may declare its own type parameters, but such a method cannot satisfy an interface.

## Structure

**Pure core, thin shell.** Core packages hold domain types and pure functions that return decisions as data. They take no context, start no goroutines and import nothing that performs I/O. The shell does I/O, concurrency and wiring, and imports the core (never the reverse).

**Parse at the edge, once.** Decode each external input strictly into a boundary type, then convert it to domain types through their constructors. Inner code never sees `any`, `map[string]any` or an unvalidated string that stands for a domain concept.

**Shape packages by capability.** Name each package for what it provides, export the minimum and enforce boundaries with `internal/`.

**Change loudly inside, quietly outside.** Within the module, prefer changes the compiler flags at every call site. Across a published API after v1.0, add rather than change, deprecate with a `//go:fix inline` migration, and reserve breaking changes for a new major version.

**Prefer the standard library.** Add a third-party module only when the standard library cannot reasonably do the job.

## Runtime

**Own every goroutine.** Each has an owner, a cancellation path and a way to be waited for.

**Bound everything.** Limit goroutines, queue and buffer sizes, input size and the duration of every external call.

**Shut down gracefully.** Long-running programs stop accepting work on a signal and drain what is in flight within a deadline.

**Log structured events.** Use `log/slog` with key-value attributes and stable messages, and never log secrets or personal data.

## Readability

**Comment the contract and the reason.** Doc comments on exported identifiers state what they guarantee. Other comments explain why, never what the code already shows.

**Measure before optimising.** Change clear code only where measurement shows it is warranted.

## Testing

**Test behaviour through the exported API.** Use external `_test` packages, so a refactor that preserves behaviour breaks no test.

**Test by layer, hermetically.** Core: table-driven tests without mocks. Shell: fakes, with `testing/synctest` for time and concurrency. Tests that need a real external system form a separate tier that never runs by default.

**Fuzz the invariants.** Fuzz constructor rules, round trips and idempotence.
