# SPDX-License-Identifier: AGPL-3.0-only
# SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

set shell := ["mise", "exec", "--", "bash", "-euo", "pipefail", "-c"]

# The acceptance recipe below replaces the one divbast/ci's app.just defines
set allow-duplicate-recipes := true

# The image's name, without its registry: everything below is named from it
APP := "pisati"

# What divbast/ci's app recipes act on: the engine, the image the build
# writes and the acceptance suite runs, where archive writes it and scan
# reads it, and the published image scan compares it with. IMAGE is a
# plain value, so `just IMAGE=<image> acceptance` tests another image.
ENGINE := env_var_or_default("ENGINE", "docker")
IMAGE := "localhost/" + APP + ":dev"
ARCHIVE := env_var_or_default("ARCHIVE", "/tmp/" + APP + "-image.tar")
PUBLISHED := env_var_or_default("PUBLISHED", "ghcr.io/divbast/" + APP + ":latest")

# build, acceptance, test, archive, scan and app-lint come from divbast/ci's
# app.just, and the other lint recipes but the Go ones below from its
# common.just. Each is run from a recipe's body, never named as a
# dependency, so `just setup` still runs before .ci exists.
import? '.ci/just/common.just'
import? '.ci/just/app.just'

# Tool versions live in mise.toml

default:
    @just --list

# Fetch divbast/ci at the commit the workflows pin, then install the toolchain and packages
setup: ci-checkout
    #!/usr/bin/env bash
    # Not through the mise exec shell: this is what installs what it runs
    set -euo pipefail
    mise install
    # Exactly the packages pnpm-lock.yaml records, or nothing at all
    mise exec -- pnpm install --frozen-lockfile

# Fetch divbast/ci at the commit the workflows pin, into .ci, and nothing else
ci-checkout:
    #!/usr/bin/env bash
    # Not through the mise exec shell, which would first install what is missing
    set -euo pipefail
    # The one commit the workflows call divbast/ci at, spelt in any case
    uses="^[[:space:]]*(-[[:space:]]+)?uses:[[:space:]]*[\"']?divbast/ci/[^@[:space:]\"']+@[^[:space:]\"'#]+"
    sha=$(grep -hoiE "${uses}" .github/workflows/*.y*ml | sed 's/.*@//' | sort -u | paste -sd ' ' - || true)
    if [[ ! "${sha}" =~ ^[0-9a-f]{40}$ ]]; then
        echo "ci-checkout: the workflows must call divbast/ci at one full commit SHA, not at: ${sha:-none}" >&2
        exit 1
    elif [[ "$(git -C .ci rev-parse HEAD 2>/dev/null || true)" == "${sha}" ]]; then
        echo "ci-checkout: .ci is already at ${sha}"
        exit 0
    fi
    changed=$([[ ! -e .ci/.git ]] || git -C .ci status --porcelain)
    if [[ -n "${changed}" ]]; then
        printf 'ci-checkout: moving .ci to %s would carry its own changes across, or stop on them; save or discard them first:\n%s\n' "${sha}" "${changed}" >&2
        exit 1
    fi
    git init -q .ci
    git -C .ci fetch -q --depth 1 https://github.com/divbast/ci "${sha}"
    git -C .ci checkout -q FETCH_HEAD && rm -rf .ci/bin

# Run the acceptance suite against the image. As divbast/ci's recipe of the
# same name, but from inside test/acceptance, which is a module of its own.
# ci-tools runs first every time, so the tool is the one this checkout of ci
# builds. pipefail keeps go test's exit status through divbast-testcount.
acceptance: ci-tools
    set -o pipefail; ENGINE={{ ENGINE }} IMAGE={{ IMAGE }} go -C test/acceptance test -json -count=1 -timeout 90m ./... | {{ ci_dir }}/bin/divbast-testcount

# The Go gates run under .golangci.yml, in place of ci's go-lint and
# go-test. Each runs in the root module and the acceptance module, which is
# a module of its own; tools/ holds only tool directives, so it has nothing
# to build or lint. golangci-lint run reports formatting too, so there is
# no separate format gate.

# Every Go gate but the tests: nothing to change, and nothing reported
go-gates: _goleak-in-step _one-suppression
    go build ./...
    go fix -diff -testingcontext=false ./...
    go mod tidy -diff
    go mod verify
    golangci-lint run ./...
    go -C tools mod tidy -diff
    go -C tools mod verify
    go -C test/acceptance build ./...
    go -C test/acceptance fix -diff -testingcontext=false ./...
    go -C test/acceptance mod tidy -diff
    go -C test/acceptance mod verify
    cd test/acceptance && golangci-lint run ./...

# Renovate never reads test/acceptance, so a goleak bump it makes moves
# the root module's alone, and the two would drift apart unseen.

# Fail unless both modules require the same goleak
[script("mise", "exec", "--", "bash", "-euo", "pipefail")]
_goleak-in-step:
    root=$(go list -m -f '{{{{.Version}}' go.uber.org/goleak)
    acceptance=$(go -C test/acceptance list -m -f '{{{{.Version}}' go.uber.org/goleak)
    if [[ "${root}" != "${acceptance}" ]]; then
        echo "goleak: the root module requires go.uber.org/goleak ${root} and test/acceptance ${acceptance}; to match them: go -C test/acceptance get go.uber.org/goleak@${root} && go -C test/acceptance mod tidy" >&2
        exit 1
    fi

# //nolint is the one way to suppress a finding, and nolintlint holds it to
# a named linter and a reason. This refuses, in any tracked file, every
# other suppression comment the linters and betterleaks would honour, and
# a betterleaks ignore file tracked anywhere or present at the root, where
# betterleaks reads one. Each pattern brackets one character, so that this
# recipe never matches itself.

# Fail on any suppression but //nolint
[script("mise", "exec", "--", "bash", "-euo", "pipefail")]
_one-suppression:
    rule="suppress only with //nolint, with a specific linter and a reason"
    hits=$(git grep -n -o -i -I -E -e 'exhaustive[:]ignore' -e 'revive[:](disable|enable)' \
        -e '#[n]osec' -e 'gitleaks[:]allow' -e 'betterleaks[:]allow' || [[ $? -eq 1 ]])
    ignores=$({ git ls-files; ls -A; } | grep -E '(^|/)[.](git|better)leaksignore$' | sort -u || true)
    status=0
    if [[ -n "${hits}" ]]; then
        while IFS=: read -r file line what; do
            echo "${file}:${line}: ${what}: ${rule}" >&2
        done <<<"${hits}"
        status=1
    fi
    if [[ -n "${ignores}" ]]; then
        while read -r file; do
            echo "${file}: a betterleaks ignore file: ${rule}" >&2
        done <<<"${ignores}"
        status=1
    fi
    exit "${status}"

# Vet is off because golangci-lint's govet runs every check it would.
# ./... never reaches the acceptance suite, a module of its own, which
# needs an image.

# The unit tests, under the race detector and in random order
go-unit:
    go test -race -shuffle=on -vet=off -count=1 ./...

# The acceptance module is all tests, so govulncheck sees its code only
# with -test.

# Fail when the code reaches a known Go advisory
vuln:
    go tool -modfile=tools/go.mod govulncheck ./...
    go -C test/acceptance tool -modfile=../../tools/go.mod govulncheck -test ./...

# A shallow clone has only its last commit to scan, so CI needs a full one.
# Without --verbose, betterleaks reports only how many it found; with it,
# each finding's rule, file, line and commit, and --redact keeps the value
# itself out of the log. --ignore-gitleaks-allow reports a secret whatever
# comment sits beside it, so no comment can wave one through.

# Fail on a secret anywhere in the history of every ref
secrets:
    betterleaks git --ignore-gitleaks-allow --redact --verbose --no-color --no-banner .

# A front-end finding is fixed, or its rule switched off by name, with the
# reason, in vite.config.ts or stylelint.config.mjs: neither a comment nor
# an ignore file may suppress one. This refuses, in every file git tracks
# or would track, each comment Oxlint, ESLint, Stylelint, TypeScript and
# Oxfmt honour as a suppression, in each of its forms (-line, -next-line
# and the rest); and each ignore file these tools read, or templ, whose
# ignore files would keep a template from generate-check, tracked anywhere
# or present at the root, where they read one. Each pattern brackets one
# character, so that this recipe never matches itself, and names match in
# any case, as macOS's file system does. The repository needs .gitignore,
# so this cannot refuse it, though Oxlint and Oxfmt skip what it lists
# too: review covers its entries, as it does the configs' own ignore lists.

# Fail on any suppression comment or ignore file
[script("mise", "exec", "--", "bash", "-euo", "pipefail")]
_fe-no-suppression:
    rule="the front end takes no suppression comment or ignore file: fix the finding, or switch its rule off by name in the config, with the reason"
    hits=$(git grep --untracked -n -o -i -I -E -e '(oxlint|eslint|stylelint)[-]disable[a-z-]*' \
        -e '@ts[-](ignore|expect-error|nocheck)' -e '(prettier|oxfmt)[-]ignore[a-z-]*' || [[ $? -eq 1 ]])
    ignores=$({ git ls-files; ls -A; } |
        grep -i -E '(^|/)[.]((stylelint|prettier|oxlint|eslint|oxfmt)ignore|templignore_(generate|fmt))$' | sort -u || true)
    status=0
    if [[ -n "${hits}" ]]; then
        while IFS=: read -r file line what; do
            echo "${file}:${line}: ${what}: ${rule}" >&2
        done <<<"${hits}"
        status=1
    fi
    if [[ -n "${ignores}" ]]; then
        while read -r file; do
            echo "${file}: an ignore file the front end's tools or templ read: ${rule}" >&2
        done <<<"${ignores}"
        status=1
    fi
    exit "${status}"

# The front end's gates. vp check runs Oxfmt, Oxlint and the TypeScript
# type checks, and vp test the scripts' tests. pnpm audit reads only the
# packages whose code a page ships, the dependencies; the tools that build
# it are devDependencies. The audit asks the npm registry.

# Every gate of the stylesheets and scripts under web/
fe-check: _fe-no-suppression
    pnpm exec vp check
    pnpm exec vp test
    pnpm exec stylelint "web/**/*.css"
    pnpm audit --prod

# vp build writes internal/web/dist, which go:embed compiles into the
# binary. Everything there but PLACEHOLDER is build output, cleared first
# so no earlier build's file lingers: vite.config.ts leaves Vite's own
# emptying off, which would delete the placeholder too.

# Build the stylesheets and scripts into internal/web/dist
fe-build:
    find internal/web/dist -mindepth 1 ! -name PLACEHOLDER -delete
    pnpm exec vp build

# The generated code is committed: templ's pages from each .templ file,
# sqlc's queries from internal/store, and the icons from lucide-static,
# which just setup installs. The templ and sqlc commands are exactly those
# Renovate may run to regenerate them.

# Regenerate every generated file from its source
generate:
    go tool -modfile=tools/go.mod templ generate
    go tool -modfile=tools/go.mod sqlc generate
    go run ./cmd/genicons

# git diff sees only tracked files, so a generated file never added is
# looked for apart, under the paths generators write to. Any change to a
# tracked file fails it, so it runs on a tree with nothing unstaged.

# Fail when any generated file differs from what its source generates
[script("mise", "exec", "--", "bash", "-euo", "pipefail")]
generate-check:
    {{ just_executable() }} generate
    go tool -modfile=tools/go.mod sqlc diff
    if ! git diff --exit-code; then
        echo "generate-check: the generated files above differ from their sources; run just generate and commit what it writes" >&2
        exit 1
    fi
    untracked=$(git ls-files --others --exclude-standard -- '*_templ.go' internal/web/icons internal/store/storedb)
    if [[ -n "${untracked}" ]]; then
        printf 'generate-check: generated, but not committed:\n%s\n' "${untracked}" >&2
        exit 1
    fi

# Vite serves the front end's sources on PISATI_DEV_VITE_URL, which
# Pisati's dev mode points its pages at. templ regenerates the pages as
# they change, rebuilds and restarts Pisati as Go files change, and serves
# it through its proxy, on port 7331, which reloads the browser. The proxy
# opens no browser itself: open the base URL Pisati is set to, as
# http://localhost:7331, so that the page's origin is the site's. Stopping
# either stops both.

# Run Pisati in dev mode: Vite's dev server, and Pisati rebuilt as it changes
[script("mise", "exec", "--", "bash", "-euo", "pipefail")]
dev:
    trap 'kill 0' EXIT
    pnpm exec vp dev &
    go tool -modfile=tools/go.mod templ generate --watch --proxy=http://localhost:8080 --open-browser=false \
        --cmd="go run ./cmd/pisati serve" --watch-pattern='(.+\.go$)|(.+\.templ$)'

# Every lint gate. Needs a running Docker daemon, for docker build --check
lint:
    {{ just_executable() }} ci-fresh actions-check workflow-audit actionlint shell-lint generate-check go-gates go-unit vuln secrets fe-check reuse-lint app-lint

# Every gate. Run before committing
ci:
    {{ just_executable() }} lint test archive scan
