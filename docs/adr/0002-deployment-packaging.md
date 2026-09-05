# 2. Deployment packaging: one image, no shell, frontend embedded

Status: Accepted — 2026-09-04 (LAM-29, recorded by LAM-39)

## Context

The application ships as a container image that must run unmodified on a
Raspberry Pi today, a home server later, and a cloud VM after that. Two facts
shape every decision below: the frontend lives in a separate repository, and the
service is reachable over a tailnet rather than sitting behind someone else's
infrastructure.

LAM-29 made these decisions. They are recorded here rather than left in
`docs/changes/LAM-29-changes.md` because that document describes a ticket at the
moment it was written and is not maintained; these constraints are still in
force and a reader will hit them.

## Decision

**The image contains the Go binaries and the embedded frontend bundle, and
nothing else.** Base is `gcr.io/distroless/static-debian12:nonroot`: CA
certificates, timezone data, and a nonroot user. No shell, no package manager,
no libc.

**The frontend arrives as a BuildKit named context**, not a clone:

    docker buildx build --build-context frontendsrc=../LaminarFlow-Frontend .

**Both build stages are pinned to `$BUILDPLATFORM`** and the Go build
cross-compiles to `$TARGETARCH`.

**`/migrate` and `/reindex` ship alongside `/laminarflow`.**

## Consequences

### No shell means debugging changes shape

`docker exec … sh` does not work. `docs/deploy.md` documents what to use
instead: container logs, `psql` inside the database container, and the operator
binaries in the image. This is the cost of the decision, not an oversight.

Swapping the final stage to `alpine:3` is a one-line change if the tradeoff ever
stops being worth it. Prefer not to — the whole point is that a service exposed
to a tailnet carries no interactive tooling.

### The operator binaries are not optional

A database whose schema was built before the migration runner existed needs
`migrate baseline` before the server will start (ADR 1's sibling problem, see
`migrations/README.md`). With no shell and no `/migrate`, that would be
impossible from inside the container. The size cost is a few megabytes against a
deployment that would otherwise have no recovery path.

### A bare `docker build` does not work

`COPY --from=frontendsrc` fails without the named context, with an error that
does not explain itself. `scripts/build-image.sh` is the supported entry point;
it also reads the Node and Go versions from `.nvmrc` and `go.mod` so the
Dockerfile never becomes a third place a version is declared.

The alternative — cloning the frontend during the build — was rejected because it
needs network access at build time and credentials if either repository ever goes
private. Requiring `build-frontend.sh` to have run first was rejected because it
makes the image build depend on prior state on the machine.

### Nothing is emulated

The frontend bundle has no architecture and Go cross-compiles, so pinning both
stages to `$BUILDPLATFORM` means the arm64 image costs what the amd64 one does.
A future stage that genuinely needs to run target-architecture code would have to
introduce QEMU and would be significantly slower; check this assumption before
adding one.

### The bundle is embedded, so a frontend change needs a Go rebuild

Deliberate: the binary is the deployment. `FRONTEND_DIR` overrides the embedded
bundle with a directory for development, so the constraint does not reach the
dev loop.

### Accepted limitation: the arm64 image is built but never run in CI

There is no arm64 runner, and emulation would prove less than it costs. The Pi is
the first place that image executes. Treat a first Pi deploy as a test, not a
formality.
