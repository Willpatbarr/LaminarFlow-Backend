#!/bin/sh
# Build the container image.
#
# Supplies the two things a bare `docker build` here cannot: the frontend
# repository as a BuildKit named context, and the Node and Go versions read from
# the files that already own them. Without the named context the build fails on
# `COPY --from=frontendsrc` with a message that does not explain itself.
#
#   ./scripts/build-image.sh                      # native architecture
#   PLATFORMS=linux/amd64,linux/arm64 ./scripts/build-image.sh
#   TAG=laminarflow:v1 ./scripts/build-image.sh
#
# A multi-platform build cannot be loaded into the local docker images list -
# that list holds one architecture per tag - so PLATFORMS with a comma builds
# and verifies without loading. Use PUSH=true with a registry tag to publish.

set -e

cd "$(dirname "$0")/.."

FRONTEND_REPO="${FRONTEND_REPO:-../LaminarFlow-Frontend}"
TAG="${TAG:-laminarflow:dev}"
PLATFORMS="${PLATFORMS:-}"

if ! command -v docker >/dev/null 2>&1; then
    echo "✗ docker is not installed." >&2
    exit 1
fi

if [ ! -f "$FRONTEND_REPO/package.json" ]; then
    echo "✗ No frontend checkout at $FRONTEND_REPO" >&2
    echo "  Set FRONTEND_REPO to where LaminarFlow-Frontend lives." >&2
    exit 1
fi

# Read the versions from the files that already declare them, so the Dockerfile
# never becomes a third place a version has to be kept in step.
if [ -f "$FRONTEND_REPO/.nvmrc" ]; then
    NODE_VERSION=$(tr -d ' \t\nv' < "$FRONTEND_REPO/.nvmrc")
else
    echo "✗ No .nvmrc in $FRONTEND_REPO - it is the source of truth for Node." >&2
    exit 1
fi

# go.mod says "go 1.27.0"; golang image tags want "1.27".
GO_VERSION=$(awk '/^go /{split($2,v,"."); print v[1]"."v[2]; exit}' go.mod)

echo "==> node ${NODE_VERSION}, go ${GO_VERSION}, tag ${TAG}"

set -- build \
    --build-context "frontendsrc=${FRONTEND_REPO}" \
    --build-arg "NODE_VERSION=${NODE_VERSION}" \
    --build-arg "GO_VERSION=${GO_VERSION}" \
    --tag "${TAG}"

if [ -n "$PLATFORMS" ]; then
    set -- "$@" --platform "$PLATFORMS"
    case "$PLATFORMS" in
        *,*)
            # Multi-platform: nothing to load, so this proves it builds only.
            echo "==> multi-platform build (${PLATFORMS}) - not loaded locally"
            ;;
        *) set -- "$@" --load ;;
    esac
else
    set -- "$@" --load
fi

if [ "$PUSH" = "true" ]; then
    set -- "$@" --push
fi

echo "==> docker buildx $*"
docker buildx "$@" .

echo "✓ built ${TAG}"
