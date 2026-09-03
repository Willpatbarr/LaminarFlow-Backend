#!/bin/sh
# Build, vet, lint, and run the test suite against a throwaway database.
#
# Go does not read .env, so a bare `go test ./...` leaves TEST_DATABASE_URL
# unset, silently SKIPS every database test, and still prints PASS. This script
# exists so that a pass means something: it loads .env and refuses to run at all
# if the database tests would be skipped.

set -e

cd "$(dirname "$0")/.."

# .env is how a developer supplies these. CI sets them in the environment
# instead and has no .env, so its absence is only fatal if the vars are missing
# too - checked below. This lets CI run this exact script rather than a
# reimplementation of it in YAML that could drift.
if [ -f .env ]; then
    set -a
    . ./.env
    set +a
fi

if [ -z "$TEST_DATABASE_URL" ]; then
    echo "✗ TEST_DATABASE_URL is not set." >&2
    echo "  Without it every database test skips and the run proves nothing." >&2
    echo "  Locally: copy .env.example to .env (see COMMANDS.md for the" >&2
    echo "  bootstrap database). In CI: set it in the job environment." >&2
    exit 1
fi

# TEST_DATABASE_URL is only a bootstrap connection: TestMain creates and drops a
# throwaway database per run, so the tests never write to whatever this names.
# The guard stays as defence in depth - if that ever regresses, aiming it at the
# real database is the mistake with the worst blast radius.
if [ "$TEST_DATABASE_URL" = "$DATABASE_URL" ]; then
    echo "✗ TEST_DATABASE_URL and DATABASE_URL name the same database." >&2
    echo "  Tests must never bootstrap from the real database." >&2
    exit 1
fi

# gofmt -l prints offending filenames but exits 0 either way, so a bare
# `gofmt -l .` under `set -e` would pass silently. Test the output instead.
echo "==> gofmt"
unformatted=$(gofmt -l .)
if [ -n "$unformatted" ]; then
    echo "✗ These files are not gofmt-clean:" >&2
    echo "$unformatted" | sed 's/^/      /' >&2
    echo "  Run: gofmt -w ." >&2
    exit 1
fi

echo "==> go build ./..."
go build ./...

echo "==> go vet ./..."
go vet ./...

echo "==> staticcheck ./..."
if ! command -v staticcheck >/dev/null 2>&1; then
    echo "✗ staticcheck is not installed." >&2
    echo "  Run: go install honnef.co/go/tools/cmd/staticcheck@v0.8.1" >&2
    exit 1
fi
staticcheck ./...

# -count=1 disables Go's test cache: these tests only mean anything when they
# actually reach Postgres, and a cached pass never opens a connection. -v is
# what makes a pass legible - without it Go swallows a passing package's output,
# so a run that skipped everything looks identical to one that tested it all.
echo "==> go test ./..."
go test -count=1 -v ./...
