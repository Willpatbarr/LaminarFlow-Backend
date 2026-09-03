#!/bin/sh
# Build, vet, and run the test suite against the test database.
#
# Go does not read .env, so a bare `go test ./...` leaves TEST_DATABASE_URL
# unset, silently SKIPS every database test, and still prints PASS. This script
# exists so that a pass means something: it loads .env and refuses to run at all
# if the database tests would be skipped.

set -e

cd "$(dirname "$0")/.."

if [ ! -f .env ]; then
    echo "✗ No .env in $(pwd)." >&2
    echo "  Copy .env.example to .env and fill in real values." >&2
    exit 1
fi

set -a
. ./.env
set +a

if [ -z "$TEST_DATABASE_URL" ]; then
    echo "✗ TEST_DATABASE_URL is not set in .env." >&2
    echo "  Without it every database test skips and the run proves nothing." >&2
    echo "  See .env.example, and COMMANDS.md to create the test database." >&2
    exit 1
fi

# The fixture opens each run with DELETE FROM document. Pointed at the real
# database that silently destroys data, which is the whole reason these are
# two separate variables.
if [ "$TEST_DATABASE_URL" = "$DATABASE_URL" ]; then
    echo "✗ TEST_DATABASE_URL and DATABASE_URL name the same database." >&2
    echo "  The tests delete every document in whatever they point at." >&2
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
    echo "  Run: go install honnef.co/go/tools/cmd/staticcheck@2026.2.1" >&2
    exit 1
fi
staticcheck ./...

echo "==> go test ./..."
go test ./...
