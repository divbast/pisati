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

# As divbast/ci's recipe of the same name, but from inside test/acceptance,
# which is a module of its own. ci-tools runs first every time, so the tool
# is the one this checkout of ci builds. It runs from the body, and .ci is
# named as a path rather than by ci's ci_dir, so that the justfile still
# parses before .ci exists. pipefail keeps go test's exit status through
# divbast-testcount.

# Run the acceptance suite against the image
acceptance:
    {{ just_executable() }} ci-tools
    set -o pipefail; ENGINE={{ ENGINE }} IMAGE={{ IMAGE }} go -C test/acceptance test -json -count=1 -timeout 90m ./... | .ci/bin/divbast-testcount

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
# each file betterleaks or gitleaks reads that could pass over a finding,
# tracked anywhere or present at the root, where they read one: an ignore
# file (.betterleaksignore, .gitleaksignore), or a configuration file of
# rules and allowlists (.betterleaks.toml, .gitleaks.toml). Each pattern
# brackets one character, so that this recipe never matches itself, and
# names match in any case, as macOS's file system does. Only
# grep's "no match" may end a search without failing the recipe: the file
# list is read on its own, so that a git that fails fails the gate.

# Fail on any suppression but //nolint
[script("mise", "exec", "--", "bash", "-euo", "pipefail")]
_one-suppression:
    rule="suppress only with //nolint, with a specific linter and a reason"
    hits=$(git grep -n -o -i -I -E -e 'exhaustive[:]ignore' -e 'revive[:](disable|enable)' \
        -e '#[n]osec' -e 'gitleaks[:]allow' -e 'betterleaks[:]allow' || [[ $? -eq 1 ]])
    files=$(git ls-files)
    files+=$'\n'$(ls -A)
    ignores=$(grep -i -E '(^|/)[.](git|better)leaks([.]toml|ignore)$' <<<"${files}" | sort -u || [[ $? -eq 1 ]])
    status=0
    if [[ -n "${hits}" ]]; then
        while IFS=: read -r file line what; do
            echo "${file}:${line}: ${what}: ${rule}" >&2
        done <<<"${hits}"
        status=1
    fi
    if [[ -n "${ignores}" ]]; then
        while read -r file; do
            echo "${file}: a file betterleaks or gitleaks reads, which could pass over a finding: ${rule}" >&2
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

# Each package's statements count as covered by its own tests alone, as
# go test -cover counts them, and a package with no tests counts with
# none covered. The floors compare whole numbers of statements, so no
# rounding lets a package through.

# Fail unless each internal/core package covers 95% and the module 80%
[script("mise", "exec", "--", "bash", "-euo", "pipefail")]
coverage:
    profile=$(mktemp)
    trap 'rm -f -- "${profile}"' EXIT
    go test -vet=off -count=1 -coverprofile="${profile}" ./...
    module=$(go list -m)
    awk -v core="${module}/internal/core/" -v module="${module}" '
        NR == 1 { next }
        {
            block = $1
            package = substr(block, 1, index(block, ":") - 1)
            sub(/\/[^\/]*$/, "", package)
            statements[block] = $2
            packages[block] = package
            if ($3 > 0) covered[block] = 1
        }
        END {
            for (block in statements) {
                n = statements[block]
                total += n
                all[packages[block]] += n
                if (block in covered) {
                    hit += n
                    hits[packages[block]] += n
                }
            }
            status = 0
            for (package in all) {
                if (index(package, core) != 1) continue
                printf "coverage: %s: %.1f%% of %d statements, at least 95%% wanted\n", package, 100 * hits[package] / all[package], all[package]
                if (100 * hits[package] < 95 * all[package]) status = 1
            }
            printf "coverage: %s: %.1f%% of %d statements, at least 80%% wanted\n", module, total ? 100 * hit / total : 0, total
            if (total == 0 || 100 * hit < 80 * total) status = 1
            fflush()
            if (status) print "coverage: a floor is not met: test what is not covered, rather than lower the floor" > "/dev/stderr"
            exit status
        }
    ' "${profile}"

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
# any case, as macOS's file system does. As in _one-suppression, only
# grep's "no match" may end a search without failing the recipe. The
# repository needs .gitignore, so this cannot refuse it, though Oxlint and
# Oxfmt skip what it lists too: review covers its entries, as it does the
# configs' own ignore lists.

# Fail on any suppression comment or ignore file
[script("mise", "exec", "--", "bash", "-euo", "pipefail")]
_fe-no-suppression:
    rule="the front end takes no suppression comment or ignore file: fix the finding, or switch its rule off by name in the config, with the reason"
    hits=$(git grep --untracked -n -o -i -I -E -e '(oxlint|eslint|stylelint)[-]disable[a-z-]*' \
        -e '@ts[-](ignore|expect-error|nocheck)' -e '(prettier|oxfmt)[-]ignore[a-z-]*' || [[ $? -eq 1 ]])
    files=$(git ls-files)
    files+=$'\n'$(ls -A)
    ignores=$(grep -i -E '(^|/)[.]((stylelint|prettier|oxlint|eslint|oxfmt)ignore|templignore_(generate|fmt))$' <<<"${files}" |
        sort -u || [[ $? -eq 1 ]])
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
#
# templ stops the command it runs, on each restart and at the end, with
# SIGINT and then SIGTERM to the command's process group, and a second
# signal stops Pisati at once, without its drain. So templ runs serve, a
# wrapper, with bash (SHELL is bash for templ alone, so that its command
# is the wrapper itself). The wrapper builds Pisati and runs it in a
# process group of its own, which neither templ's signals nor Ctrl-C
# reach, and sets its trap before Pisati starts, so that no stop finds
# Pisati running without it. Pisati gets one SIGTERM, from whichever stops
# it first: the wrapper, on templ's first signal, or this recipe as it
# exits, since templ kills the wrapper outright when it stops itself.
# Only the one that moves Pisati's pid file away sends it. A second
# Ctrl-C, while Pisati stops, has the recipe send it a second SIGTERM, so
# that it stops at once; the recipe still stops Vite and removes its run
# directory before it returns. templ runs in the background, and the
# recipe waits for it, as for the stop it sends: bash runs a trap at once
# while it waits that way, whereas a command in the foreground holds every
# trap until it exits, and a second Ctrl-C while templ itself stopped was
# lost, or cut the recipe's own stop short.

# Run Pisati in dev mode: Vite's dev server, and Pisati rebuilt as it changes
[script("mise", "exec", "--", "bash", "-euo", "pipefail")]
dev:
    run=$(mktemp -d)
    export DEV_SERVE_RUN="${run}"
    cat >"${run}/stop" <<'EOF'
    if mv "${DEV_SERVE_RUN}/pid" "${DEV_SERVE_RUN}/stopping" 2>/dev/null; then
        kill -TERM -- "-$(cat "${DEV_SERVE_RUN}/stopping")" 2>/dev/null || true
    fi
    EOF
    cat >"${run}/halt" <<'EOF'
    if pid=$(cat "${DEV_SERVE_RUN}/stopping" 2>/dev/null); then
        kill -TERM -- "-${pid}" 2>/dev/null || true
    fi
    EOF
    templ=
    # waitfor waits for the child pid to end. A signal ends a wait early,
    # once its trap has run, so it waits again until the child is gone.
    waitfor() {
        while kill -0 "$1" 2>/dev/null; do wait "$1" 2>/dev/null || true; done
    }
    finish() {
        # A command run in the background ignores Ctrl-C, so the stop runs
        # to its end; a second Ctrl-C meanwhile is held until it has.
        halt=no
        trap 'halt=yes' INT TERM
        bash "${run}/stop" &
        waitfor "$!"
        trap 'bash "${run}/halt"' INT TERM
        if [[ "${halt}" == yes ]]; then
            bash "${run}/halt"
        fi
        # templ stops on the same Ctrl-C.
        if [[ -n "${templ}" ]]; then
            waitfor "${templ}"
        fi
        # The prompt comes back once Pisati has stopped, after its last record.
        if pid=$(cat "${run}/stopping" 2>/dev/null); then
            while kill -0 "${pid}" 2>/dev/null; do sleep 0.1 || true; done
        fi
        # Pisati has stopped: the stop below is Vite's and templ's alone.
        trap '' INT TERM
        rm -rf -- "${run}"
        kill 0
    }
    trap finish EXIT
    trap 'exit 130' INT TERM
    pnpm exec vp dev &
    serve=$(cat <<'EOF'
    set -m
    stop=no
    trap 'trap "" INT TERM; stop=yes; bash "${DEV_SERVE_RUN}/stop"' INT TERM
    go build -o "${DEV_SERVE_RUN}/pisati" ./cmd/pisati || exit
    if [[ "${stop}" == yes ]]; then
        exit 0
    fi
    "${DEV_SERVE_RUN}/pisati" serve &
    pid=$!
    echo "${pid}" >"${DEV_SERVE_RUN}/pid"
    # A stop that came before the pid file did found nothing to stop.
    if [[ "${stop}" == yes ]]; then
        bash "${DEV_SERVE_RUN}/stop"
    fi
    status=0
    wait "${pid}" || status=$?
    # A signal ends the first wait early: wait for Pisati to stop.
    if kill -0 "${pid}" 2>/dev/null; then
        status=0
        wait "${pid}" || status=$?
    fi
    rm -f -- "${DEV_SERVE_RUN}/pid"
    stopped=$(cat "${DEV_SERVE_RUN}/stopping" 2>/dev/null || true)
    if [[ "${stopped}" == "${pid}" ]]; then
        rm -f -- "${DEV_SERVE_RUN}/stopping"
    fi
    echo "pisati serve exited with status ${status}" >&2
    exit "${status}"
    EOF
    )
    SHELL=bash go tool -modfile=tools/go.mod templ generate --watch --proxy=http://localhost:8080 --open-browser=false \
        --cmd="${serve}" --watch-pattern='(.+\.go$)|(.+\.templ$)' &
    templ=$!
    wait "${templ}"

# The slow suites, which the dispatch-only workflows browser.yml and
# fuzz.yml run too, and slow, below, before every push.

# The browser suite runs in Chromium, Firefox and WebKit, natively, each
# against this platform's baselines under test/browser/baselines. Each
# test starts its own Pisati, as test/browser/fixtures.ts says: pisati
# serve, built here from the tree, serving the production build of the
# front end that go:embed compiles in, so no Vite runs. Playwright fetches
# the browsers its release drives the first time; on Linux their system
# libraries come first, from playwright install-deps, as root. ARGS go to
# playwright test: --update-snapshots writes this platform's baselines
# afresh, and --project=webkit runs one browser. A failed run leaves its
# screenshots, diffs and Pisati's logs where it says, in a directory of
# its own unless BROWSER_RESULTS names one. Every other way out, a failed
# build among them, leaves nothing: the run directory goes, and so does
# any Pisati still running from it, as one a killed test worker leaves.

# Run the browser suite: Chromium, Firefox and WebKit, with axe-core and screenshots
[positional-arguments]
[script("mise", "exec", "--", "bash", "-euo", "pipefail")]
browser *ARGS:
    run=$(mktemp -d)
    keep=no
    finish() {
        pkill -KILL -f -- "${run}/pisati" || true
        if [[ "${keep}" == yes ]]; then
            rm -f -- "${run}/pisati"
        else
            rm -rf -- "${run}"
        fi
    }
    trap finish EXIT
    {{ just_executable() }} fe-build
    results="${BROWSER_RESULTS:-${run}/results}"
    go build -o "${run}/pisati" ./cmd/pisati
    pnpm exec playwright install chromium firefox webkit
    status=0
    BROWSER_PISATI="${run}/pisati" pnpm exec playwright test --config test/browser/playwright.config.ts \
        --output "${results}" "$@" || status=$?
    if [[ "${status}" -ne 0 ]]; then
        if [[ -z "${BROWSER_RESULTS:-}" ]]; then
            keep=yes
        fi
        echo "browser: the run's screenshots, diffs and logs are in ${results}" >&2
    fi
    exit "${status}"

# How long fuzz gives each fuzz target: just FUZZTIME=30s fuzz
FUZZTIME := "10m"

# Go fuzzes one target of one package at a time, so these go through each
# in turn. They carry on past a failure, and name every target that failed
# at the end. -fuzzminimizetime=1s keeps the minimiser, which goes over
# each new input the fuzzer keeps, from taking the target's time as its
# corpus grows. A failing input lands under its package's testdata/fuzz,
# where plain go test runs it from then on: commit it with the fix.

# Fuzz every target in every package, for FUZZTIME each
fuzz:
    {{ just_executable() }} _fuzz '{{ FUZZTIME }}' ./...

# The packages that differ from main are those with a Go file, or a fuzz
# corpus under testdata/fuzz, that differs from main's, committed on this
# branch or not: added, changed or removed. A moved file counts in both
# its old and its new package, so renames are not followed. The
# acceptance module, a module of its own, and tools/ hold no fuzz target.

# Fuzz the targets in every package that differs from main, for 30 s each
[script("mise", "exec", "--", "bash", "-euo", "pipefail")]
fuzz-changed:
    if ! base=$(git merge-base main HEAD 2>/dev/null); then
        echo "fuzz-changed: there is no branch main to compare with; fetch it first" >&2
        exit 1
    fi
    changed=$({ git diff --name-only --no-renames "${base}" --; git ls-files --others --exclude-standard; } |
        awk '/(^|\/)testdata\/fuzz\// { sub(/(^|\/)testdata\/fuzz\/.*/, ""); print ($0 == "" ? "." : $0); next }
            /\.go$/ { print (sub(/\/[^\/]*$/, "") ? $0 : ".") }' | sort -u)
    root=$(go list -m -f '{{{{.Dir}}')
    packages=()
    while read -r path dir; do
        relative="${dir#"${root}"}"
        relative="${relative#/}"
        if grep -qxF -- "${relative:-.}" <<<"${changed}"; then
            packages+=("${path}")
        fi
    done < <(go list -f '{{{{.ImportPath}} {{{{.Dir}}' ./...)
    if [[ "${#packages[@]}" -eq 0 ]]; then
        echo "fuzz-changed: no package differs from main"
        exit 0
    fi
    {{ just_executable() }} _fuzz 30s "${packages[@]}"

# Fuzz each target in PACKAGES for TIME, one at a time
[positional-arguments]
[script("mise", "exec", "--", "bash", "-euo", "pipefail")]
_fuzz TIME *PACKAGES:
    time="$1"
    shift
    targets=$(go test -list '^Fuzz' "$@" |
        awk '/^Fuzz/ { names[++n] = $1; next } /^ok / { for (i = 1; i <= n; i++) print $2, names[i]; n = 0 }')
    if [[ -z "${targets}" ]]; then
        echo "fuzz: no fuzz target in $*"
        exit 0
    fi
    failed=()
    while read -r package target; do
        echo "fuzz: ${target} in ${package}, for ${time}"
        if ! go test -run='^$' -fuzz="^${target}\$" -fuzztime="${time}" -fuzzminimizetime=1s "${package}" </dev/null; then
            failed+=("${target} in ${package}")
        fi
    done <<<"${targets}"
    if [[ "${#failed[@]}" -gt 0 ]]; then
        printf 'fuzz: failed: %s\n' "${failed[@]}" >&2
        exit 1
    fi

# What runs before every push: the browser suite, then every fuzz target
# for TIME, 30 s unless given, as `just slow 10m` gives the 10 minutes of
# just fuzz and fuzz.yml.

# Before every push: the browser suite, then every fuzz target for TIME
slow TIME="30s":
    {{ just_executable() }} browser
    {{ just_executable() }} FUZZTIME='{{ TIME }}' fuzz

# Every lint gate. Needs a running Docker daemon, for docker build --check
lint:
    {{ just_executable() }} ci-fresh actions-check workflow-audit actionlint shell-lint generate-check go-gates go-unit coverage vuln secrets fe-check reuse-lint app-lint

# Every gate. Run before committing
ci:
    {{ just_executable() }} lint test archive scan
