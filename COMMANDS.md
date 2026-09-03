# Commands

Everything here assumes you are in the repo root. Commands read connection
details from `.env`, so nothing below hardcodes a host — copy `.env.example` to
`.env` first.

Note: `go test ./...` on its own both skips every database test *and* replays
cached results, so it can print PASS without ever reaching Postgres. Use the
script below instead — it loads `.env` and passes `-count=1`.

## Run the full check — gofmt, build, vet, staticcheck, and the database tests

    ./scripts/test.sh

## Run the server

    set -a && source .env && set +a && go run .

## Check the server is up and can reach Postgres

    curl -s localhost:8080/healthz && curl -s localhost:8080/healthz/db

## Apply a migration to the real database

    set -a && source .env && set +a && psql "$DATABASE_URL" -1 -f migrations/0003_workspace.sql

## Open a psql shell on the real database

    set -a && source .env && set +a && psql "$DATABASE_URL"

## Inspect a table's columns, indexes, and constraints

    set -a && source .env && set +a && psql "$DATABASE_URL" -c '\d document'

## List the tables

    set -a && source .env && set +a && psql "$DATABASE_URL" -c '\dt'

## Rebuild every search_index row from the document blobs

    set -a && source .env && set +a && go run ./cmd/reindex

## Create the test bootstrap database — first-time setup

`TestMain` creates its own throwaway database per run and applies the
migrations itself, so this one only has to exist. It never holds a schema.

    set -a && source .env && set +a && psql "$DATABASE_URL" -c 'CREATE DATABASE laminarflow_test OWNER laminarflow;'

## Sweep test databases left behind by a killed run

`TestMain` drops its database on the way out, but a SIGKILL or a hard Ctrl-C
skips that. The pattern matches only the generated `<pid>_<timestamp>` names,
so the bootstrap database is never touched.

    set -a && source .env && set +a && psql "$DATABASE_URL" -tAc "SELECT 'DROP DATABASE ' || quote_ident(datname) || ' WITH (FORCE);' FROM pg_database WHERE datname ~ '^laminarflow_test_[0-9]+_[0-9]+\$'" | psql "$DATABASE_URL"

## Start and stop Postgres — run on the host that owns the database

    docker compose up -d
    docker compose down
