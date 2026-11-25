# SPDX-License-Identifier: AGPL-3.0-only
# SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

# The app's own version, stamped into the binary and the image's version label
ARG PISATI_VERSION=0.1.0
# Every image below is a Docker Hardened Image on Debian 13, pinned by
# digest, and CI verifies Docker's signature on each before anything is
# built.
#
# The newest stable Go. Only the binary leaves this stage.
ARG GO_IMAGE=dhi.io/golang:1.27.4-debian13-dev@sha256:a56385e6b77b61ccaf807d5ea72d8593ad7a390fdd1c6552c4e127a90730b05f
# Node's LTS, in the -dev variant, which builds the stylesheets and
# scripts under web/. Only the build's output leaves this stage.
ARG NODE_IMAGE=dhi.io/node:26.13.1-debian13-dev@sha256:bb2dac138a83abecec5ba2bf092a733fbc2598742395410734ed60e5a6966cfb
# The runtime: Docker Hardened Images' static, since the binary is static,
# with nothing in it but CA certificates, time zones and the nonroot user
# 65532: no shell and no package manager.
ARG RUNTIME_IMAGE=dhi.io/static:20250419-debian13@sha256:1295ce0b300e0972ee6034f75f2161a629c12d760526f2e029528416dec2f96e

FROM ${NODE_IMAGE} AS web
# The Node image has npm and no pnpm, so npm installs pnpm first, exactly
# as build/pnpm's lockfile records it, checked by integrity. No package's
# install script runs, in either install: pnpm's own only puts its native
# binary in place of the script that starts it.
WORKDIR /opt/pnpm
COPY build/pnpm/package.json build/pnpm/package-lock.json ./
RUN npm ci --ignore-scripts
ENV PATH=/opt/pnpm/node_modules/.bin:${PATH}
WORKDIR /src
# Exactly the packages pnpm-lock.yaml records, or the build fails
COPY package.json pnpm-lock.yaml pnpm-workspace.yaml ./
RUN pnpm install --frozen-lockfile --ignore-scripts
COPY tsconfig.json vite.config.ts vite.devserver.ts ./
COPY web/ web/
# Writes internal/web/dist: the hashed files, and the manifest that names them
RUN pnpm exec vp build

FROM ${GO_IMAGE} AS build
ARG PISATI_VERSION
ENV GOTOOLCHAIN=local GOFLAGS=-mod=readonly
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
# go:embed compiles the front end's build into the binary: the one the
# stage above made, and nothing a local build left in the build context.
RUN rm -rf internal/web/dist
COPY --from=web /src/internal/web/dist/ internal/web/dist/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${PISATI_VERSION}" -o /out/pisati ./cmd/pisati \
    && mkdir -p /out/lib/pisati

FROM ${RUNTIME_IMAGE}
ARG PISATI_VERSION

# Root-owned, and never written
COPY --from=build /out/pisati /usr/local/bin/pisati
# The lockfile of the npm packages that built the stylesheets and scripts
# compiled into the binary: the image holds no node_modules that names them
COPY --from=web /src/pnpm-lock.yaml /usr/share/doc/pisati/pnpm-lock.yaml
# The state directory, empty, and Pisati's user's alone. The root is
# read-only, so Pisati refuses to start until a volume is mounted here; an
# empty volume starts out with this directory's owner and mode. It is
# copied as what /out/lib holds, so that --chmod sets its own mode under
# every builder: buildah sets none on a directory it makes to copy into.
COPY --from=build --chown=65532:65532 --chmod=0700 /out/lib/ /var/lib/

LABEL org.opencontainers.image.source="https://github.com/divbast/pisati" \
    org.opencontainers.image.url="https://github.com/divbast/pisati" \
    org.opencontainers.image.title="pisati" \
    org.opencontainers.image.version="${PISATI_VERSION}" \
    org.opencontainers.image.licenses="AGPL-3.0-only"
# Names the lockfile above to divbast/ci's scan, which then scans the npm
# packages it records along with the image's own
LABEL io.github.divbast.scan.lockfiles="/usr/share/doc/pisati/pnpm-lock.yaml"

EXPOSE 8080 8081
# Exec form: the shell form would need /bin/sh, which the image lacks.
# --timeout must clear pisati health's own budget, 4 s for its one
# request: a shorter timeout kills the probe mid-flight and records a bare
# engine timeout instead of the probe's own line, which is the only
# diagnostic there is. Podman reads no HEALTHCHECK from an image published
# as an OCI index: its users pass the same array as --health-cmd.
HEALTHCHECK --interval=30s --timeout=10s --start-period=60s --retries=3 \
    CMD ["/usr/local/bin/pisati", "health"]
USER 65532:65532
# The server itself, as PID 1: nothing runs before it. Pisati reads its
# settings and checks its state directory itself as it starts, and
# refuses to start when either is wrong.
ENTRYPOINT ["/usr/local/bin/pisati", "serve"]
