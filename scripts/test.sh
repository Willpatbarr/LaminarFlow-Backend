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

echo "==> go build ./..."
go build ./...

echo "==> go vet ./..."
go vet ./...

echo "==> go test ./..."
go test ./...
