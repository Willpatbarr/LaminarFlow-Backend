# Commands

Everything here assumes you are in the repo root. Commands read connection
details from `.env`, so nothing below hardcodes a host — copy `.env.example` to
`.env` first.

Note: `go test ./...` on its own skips every database test and still prints
PASS. Use the script below instead.

## Run the full check — build, vet, and the database tests

    ./scripts/test.sh

## Run the server

    set -a && source .env && set +a && go run .

## Check the server is up and can reach Postgres

    curl -s localhost:8080/healthz && curl -s localhost:8080/healthz/db

## Apply a migration to the real database

    set -a && source .env && set +a && psql "$DATABASE_URL" -1 -f migrations/0003_workspace.sql

## Apply a migration to the test database

    set -a && source .env && set +a && psql "$TEST_DATABASE_URL" -1 -f migrations/0003_workspace.sql

## Open a psql shell on the real database

    set -a && source .env && set +a && psql "$DATABASE_URL"

## Inspect a table's columns, indexes, and constraints

    set -a && source .env && set +a && psql "$DATABASE_URL" -c '\d document'

## List the tables

    set -a && source .env && set +a && psql "$DATABASE_URL" -c '\dt'

## Rebuild every search_index row from the document blobs

    set -a && source .env && set +a && go run ./cmd/reindex

## Create the test database from scratch — first-time setup

    set -a && source .env && set +a && psql "$DATABASE_URL" -c 'CREATE DATABASE laminarflow_test OWNER laminarflow;' && for f in migrations/*.sql; do psql "$TEST_DATABASE_URL" -v ON_ERROR_STOP=1 -1 -f "$f"; done

## Start and stop Postgres — run on the host that owns the database

    docker compose up -d
    docker compose down
