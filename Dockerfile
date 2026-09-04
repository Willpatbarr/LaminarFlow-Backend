# syntax=docker/dockerfile:1.7
#
# Multi-stage, multi-architecture image: the built frontend, the Go binary, and
# nothing else. One artifact is the whole deployment.
#
# The frontend lives in a separate repository, so it arrives as a BuildKit named
# context rather than being cloned here. That keeps the repos independent and
# needs no network access or credentials at build time:
#
#   docker buildx build --build-context frontendsrc=../LaminarFlow-Frontend .
#
# scripts/build-image.sh wraps that and fills in the version arguments below.

# Versions are arguments so the wrapper script can read them from the files that
# already own them - .nvmrc in the frontend repo and go.mod here - rather than
# this file becoming a third place a version has to be kept in step.
ARG NODE_VERSION=26
ARG GO_VERSION=1.27

# ---------------------------------------------------------------------------
# Stage 1: build the frontend bundle.
#
# --platform=$BUILDPLATFORM pins this to the machine doing the building. The
# output is plain JS and CSS with no architecture of its own, so emulating the
# target here would cost minutes and buy nothing.
# ---------------------------------------------------------------------------
FROM --platform=$BUILDPLATFORM node:${NODE_VERSION}-alpine AS frontend

WORKDIR /src

# Everything, then a clean install. Copying the checkout wholesale would drag in
# a node_modules built for the host's OS and architecture, so it is removed
# before npm ci rather than trusted. The named context has no .dockerignore of
# its own to rely on.
COPY --from=frontendsrc . .
RUN rm -rf node_modules dist \
    && npm ci \
    && npm run build

# ---------------------------------------------------------------------------
# Stage 2: build the Go binaries.
#
# Also pinned to BUILDPLATFORM: Go cross-compiles for free, so building natively
# and targeting $TARGETARCH is far faster than running the compiler under QEMU.
# ---------------------------------------------------------------------------
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build

WORKDIR /src

# Dependencies first, so a source-only change does not re-download the module
# cache on every build.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# The bundle lands where web/embed.go expects it. This is the same directory
# scripts/build-frontend.sh fills for local development.
COPY --from=frontend /src/dist ./web/dist

ARG TARGETARCH

# CGO_ENABLED=0 is what makes the binary static, which is what lets the final
# stage have no libc at all. -trimpath keeps build paths out of the binary;
# -s -w drop the symbol and DWARF tables.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
        go build -trimpath -ldflags="-s -w" -o /out/laminarflow . \
 && CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
        go build -trimpath -ldflags="-s -w" -o /out/migrate ./cmd/migrate \
 && CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
        go build -trimpath -ldflags="-s -w" -o /out/reindex ./cmd/reindex

# ---------------------------------------------------------------------------
# Stage 3: the shipped image.
#
# distroless/static has no shell, no package manager, and no libc - only CA
# certificates, timezone data, and a nonroot user. For a service exposed to a
# tailnet that is the right default; see docs/deploy.md for how to debug without
# a shell.
# ---------------------------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/laminarflow /laminarflow
# The operator tools travel with the server. cmd/migrate in particular is not
# optional: a database built before LAM-10 needs `migrate baseline` before the
# server will start, and there is no shell here to fetch a binary with.
COPY --from=build /out/migrate /migrate
COPY --from=build /out/reindex /reindex

# Documentation only - PORT still decides what the server binds. Kept at the
# same default as internal/config so the two do not disagree.
EXPOSE 8080

USER nonroot:nonroot

# The binary checks itself, because there is no curl or shell in this image.
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD ["/laminarflow", "healthcheck"]

ENTRYPOINT ["/laminarflow"]
