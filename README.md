<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee> -->

# Pisati

Pisati is a secure, minimalist, federated blogging platform, derived from
WriteFreely by Musing Studio. It is written in Go and ships as a container
image that runs at zero Linux capabilities, with no new privileges, on a
read-only root filesystem as a non-root user.

The project is at its first stage: the repository holds the image, its
pipeline and a placeholder server that serves one page and a health
endpoint, which the next stages replace with the app. Nothing is published
to ghcr.io yet. The divbast organisation's settings, rulesets and Renovate
wiring live in `divbast/infra`.

## Base

The image is one static Go binary, `/usr/local/bin/pisati`, on
[Docker Hardened Images](https://docs.docker.com/dhi/)' `static` for
Debian 13, which holds nothing else but CA certificates, time zones and the
nonroot user. The binary is built in their `golang` image at the newest
stable Go. A Node image is pinned beside them for the stylesheet and script
build that a later stage adds. Each is pinned by digest, and CI checks
Docker's signature on each before it builds anything.

## Container Features

| Property | Value |
|---|---|
| Capabilities | none (`--cap-drop ALL`) |
| Privileges | no new ones (`--security-opt no-new-privileges`), under the engine's seccomp filter |
| Root filesystem | read-only |
| User | uid/gid 65532 (`nonroot`) |
| Port | 8080 (the page), 8081 (health, do not publish), and nothing else listens |
| tmpfs required | `/tmp`, 16 MiB, which nothing can run from (`--tmpfs /tmp:size=16m,noexec,nosuid,nodev`) |
| Volume | none |
| Processes | one: the app, `pisati serve`, as PID 1 |
| Shell in image | none |
| Package manager | none |
| Outbound network | none |
| Healthy | the shipped probe: `/healthz` 200 **and** `GET /` from loopback exactly `Hello from pisati` |

## Running

There is no published image, so build one first, with Podman to match the
command below: `just setup`, then `ENGINE=podman just build`, which tags it
`localhost/pisati:dev`.

```bash
podman run -d --name pisati \
  --cap-drop ALL --read-only --read-only-tmpfs=false \
  --security-opt no-new-privileges \
  --user 65532:65532 \
  --tmpfs /tmp:size=16m,noexec,nosuid,nodev \
  -p 8080:8080 \
  localhost/pisati:dev
```

`--read-only-tmpfs=false` is Podman's alone: without it, Podman mounts
writable tmpfs of its own beside the read-only root. Docker takes the same
command without it.

Port 8080 is an allowlist: `/` answers the page, and everything else is a
404 with an empty body. Each request is logged to stdout as JSON. A stop
takes SIGTERM, finishes what the server is answering within 5 seconds, and
exits 0.

## Health

The image ships its own probe and declares a `HEALTHCHECK`, which Docker
runs with no configuration. Podman reads no healthcheck from an image
published as an OCI index
([podman/podman#25454](https://github.com/containers/podman/issues/25454),
[#18904](https://github.com/containers/podman/issues/18904)), so a Podman
user passes it:

```bash
podman run -d --health-cmd '["/usr/local/bin/pisati","health"]' localhost/pisati:dev
```

Under a Quadlet, the equivalent is `HealthCmd=["/usr/local/bin/pisati","health"]`.
The array has no leading `"CMD"` on purpose: Podman before 5.8.0 re-splits
one that starts with it into a single useless token.

| | |
|---|---|
| `/usr/local/bin/pisati health` | The in-container probe. Asserts `/healthz` is 200 **and** `GET /` on 8080 answers exactly the page. Prints nothing when healthy; otherwise one `<check>: <reason>` line for each half that failed, and exits 1 |
| `http://127.0.0.1:8081/healthz` | 200 `ok` while the server runs |

There is no shell in this image, so a health command given as a plain
string can never pass.

## The pipeline

Three workflows, each a short caller of divbast/ci's reusable workflow of
the same purpose, at one pinned release of it:

- `build.yml` calls `app-check.yml` on every push, pull request and daily
  run: lint, the image built for amd64 and arm64 once every image the
  Containerfile builds from is verified, the acceptance suite against each
  under Docker and rootless Podman, and the scan. Its `required` job is the
  one check `main`'s ruleset requires. It publishes nothing.
- `renovate.yml` runs Renovate, as below.
- `stall.yml` fails when a Renovate pull request has been open more than 14
  days or red or pending for more than a day, or when Renovate has not
  succeeded in a day.

## Dependency updates

Every input the Containerfile names is pinned by digest. A pin only moves
through a pull request, and [Renovate](https://docs.renovatebot.com/) opens
those: every morning, after every merge to `main`, whenever CI finishes on
one of its branches and when the Dependency Dashboard issue is edited, from
`.github/workflows/renovate.yml`, as the divbast org's `divbast-renovate`
GitHub App. dhi.io answers nothing without a login, so `renovate.yml`
passes Renovate the DHI login divbast/infra keeps here.

Renovate merges its own pull requests once every check on one has passed,
while the repository variable `PISATI_AUTOMERGE` is `enabled`. A new major
version of divbast/ci and a raise of `go.mod`'s `go` line first wait for
approval on the Dependency Dashboard. Any other value of the variable, or
none, stops merging from the next run:

    gh variable set PISATI_AUTOMERGE --body disabled

It merges with a squash, which GitHub signs, because `main` accepts only
signed commits. The App, the `renovate` environment holding its key,
`PISATI_AUTOMERGE` and this repository's rulesets are managed from
[divbast/infra](https://github.com/divbast/infra).

## Development

- `just setup` fetches divbast/ci into `.ci/` at the commit the workflows
  pin, then installs the pinned toolchain from `mise.toml`.
- `just scan` reads the published image with
  [skopeo](https://github.com/containers/skopeo), which must be on `PATH`;
  so must it for `just ci`.
- `just build` builds the image locally, without verifying the images it
  builds from; `just ci-tools && .ci/bin/divbast-verify-images` does.
- `just test` builds, then runs the acceptance suite against the image,
  under `ENGINE`, docker unless it says podman.
- `just lint` runs every lint gate, and needs a running Docker daemon
  logged in to dhi.io.
- `just ci` runs every gate, the scan included. Run it before committing.

The recipes that call GitHub, pinact's and zizmor's, read a fine-grained
personal access token from `GITHUB_TOKEN`: divbast/ci's README says which,
and how to pass it.

## AI/LLM Disclosure

This project was developed with significant LLM involvement. Each git commit contains an `Assisted-by:` tag detailing the particular model/tool used.
