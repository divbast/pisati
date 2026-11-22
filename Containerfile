# SPDX-License-Identifier: AGPL-3.0-only
# SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

# The app's own version, stamped into the binary and the image's version label
ARG PISATI_VERSION=0.1.0
# Every image below is a Docker Hardened Image on Debian 13, pinned by
# digest, and CI verifies Docker's signature on each before anything is
# built.
#
# The newest stable Go. Only the binary leaves this stage.
ARG GO_IMAGE=dhi.io/golang:1.27.4-debian13-dev@sha256:2f58c6928a851f0b6059f1a63514828a788a748d88b218a1bc9ae23c3b540140
# Node's LTS, in the -dev variant, for the stylesheet and script build the
# web/ sources get. Declared here; no stage builds from it yet.
ARG NODE_IMAGE=dhi.io/node:26.13.1-debian13-dev@sha256:1e91de4150a2814738074722988b0d17e025a1f5fd615e5a678207466cfa9fb3
# The runtime: Docker Hardened Images' static, since the binary is static,
# with nothing in it but CA certificates, time zones and the nonroot user
# 65532: no shell and no package manager.
ARG RUNTIME_IMAGE=dhi.io/static:20250419-debian13@sha256:4749130e822c145dbdf9ca11ec0cc2f694b5fe687fd74268a2b62160e6726f39

FROM ${GO_IMAGE} AS build
ARG PISATI_VERSION
ENV GOTOOLCHAIN=local GOFLAGS=-mod=readonly
WORKDIR /src
COPY go.mod ./
COPY cmd/ cmd/
COPY internal/ internal/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${PISATI_VERSION}" -o /out/pisati ./cmd/template

FROM ${RUNTIME_IMAGE}
ARG PISATI_VERSION

# Root-owned, and never written
COPY --from=build /out/pisati /usr/local/bin/pisati

LABEL org.opencontainers.image.source="https://github.com/divbast/pisati" \
    org.opencontainers.image.url="https://github.com/divbast/pisati" \
    org.opencontainers.image.title="pisati" \
    org.opencontainers.image.version="${PISATI_VERSION}" \
    org.opencontainers.image.licenses="AGPL-3.0-only"

EXPOSE 8080 8081
# Exec form: the shell form would need /bin/sh, which the image lacks.
# --timeout must clear pisati health's own budget: it allows each of its
# two exchanges 4 s and runs them one after the other, so a listener that
# accepts and never answers costs 8 s. A shorter timeout kills the probe
# mid-flight and records a bare engine timeout instead of the probe's own
# line, which is the only diagnostic there is. Podman reads no HEALTHCHECK
# from an image published as an OCI index: its users pass the same array
# as --health-cmd.
HEALTHCHECK --interval=30s --timeout=10s --start-period=60s --retries=3 \
    CMD ["/usr/local/bin/pisati", "health"]
USER 65532:65532
# The server itself, as PID 1: the image has no state to prepare and no
# environment to read, so nothing runs before it
ENTRYPOINT ["/usr/local/bin/pisati", "serve"]
