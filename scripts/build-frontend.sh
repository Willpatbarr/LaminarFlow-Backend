#!/bin/sh
# Build the frontend and copy its bundle into web/dist, where go:embed picks it
# up on the next `go build`.
#
# The two repos stay separate - this only moves a build artifact between them.
# FRONTEND_REPO says where the frontend checkout lives; the default assumes the
# sibling layout a dev machine has, and nothing here depends on that being true.
#
# LAM-29 owns the container build, which will do this in a multi-stage image
# rather than from a checkout. This script is the local-development path.

set -e

cd "$(dirname "$0")/.."

FRONTEND_REPO="${FRONTEND_REPO:-../LaminarFlow-Frontend}"

if [ ! -d "$FRONTEND_REPO" ]; then
    echo "✗ No frontend checkout at $FRONTEND_REPO" >&2
    echo "  Set FRONTEND_REPO to where LaminarFlow-Frontend lives." >&2
    exit 1
fi

if [ ! -f "$FRONTEND_REPO/package.json" ]; then
    echo "✗ $FRONTEND_REPO has no package.json - wrong directory?" >&2
    exit 1
fi

echo "==> building frontend in $FRONTEND_REPO"
# npm ci rather than npm install: it installs exactly the lockfile, so the
# bundle this produces does not depend on when the script last ran.
(cd "$FRONTEND_REPO" && npm ci && npm run build)

if [ ! -f "$FRONTEND_REPO/dist/index.html" ]; then
    echo "✗ Build produced no dist/index.html in $FRONTEND_REPO" >&2
    exit 1
fi

echo "==> copying bundle into web/dist"
# Clear first, so an asset removed from the frontend does not survive here
# forever. .gitkeep is restored because go:embed needs the directory to exist
# even on a clean checkout.
rm -rf web/dist
mkdir -p web/dist
touch web/dist/.gitkeep
cp -R "$FRONTEND_REPO/dist/." web/dist/

echo "✓ web/dist updated - run 'go build ./...' to embed it"
