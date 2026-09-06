# Commands

Everything here assumes you are in the repo root. Commands read connection
details from `.env`, so nothing below hardcodes a host — copy `.env.example` to
`.env` first.

Note: `go test ./...` on its own both skips every database test *and* replays
cached results, so it can print PASS without ever reaching Postgres. Use the
script below instead — it loads `.env` and passes `-count=1`.

## Run the full check — gofmt, build, vet, staticcheck, and the database tests

    ./scripts/test.sh

## Build the frontend into this repo, then embed it

Copies the frontend's `dist/` into `web/dist`, where `go:embed` picks it up on
the next build. `FRONTEND_REPO` defaults to `../LaminarFlow-Frontend`.

    ./scripts/build-frontend.sh && go build ./...

## Run the server — serves the API and the frontend on one port

    set -a && source .env && set +a && go run .

## Run the server against a frontend bundle on disk

Skips the embed, so a frontend rebuild is visible without rebuilding Go.

    set -a && source .env && set +a && FRONTEND_DIR=../LaminarFlow-Frontend/dist go run .

## Check the server is up and can reach Postgres

    curl -s localhost:8080/healthz && curl -s localhost:8080/healthz/db

## Apply pending migrations to the real database

    set -a && source .env && set +a && go run ./cmd/migrate up

## Roll back the most recent migration — or the most recent N

    set -a && source .env && set +a && go run ./cmd/migrate down
    set -a && source .env && set +a && go run ./cmd/migrate down 2

## See which migrations have run

    set -a && source .env && set +a && go run ./cmd/migrate status

## Adopt a database whose schema was built by hand

Records every migration as applied without running any of them. Only for a
database that predates the runner — it refuses once any migration is recorded.

    set -a && source .env && set +a && go run ./cmd/migrate baseline

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

## Build the container image

Reads Node from the frontend's `.nvmrc` and Go from `go.mod`, and supplies the
frontend repo as a build context. A bare `docker build` here will not work.

    ./scripts/build-image.sh
    PLATFORMS=linux/arm64 ./scripts/build-image.sh
    PLATFORMS=linux/amd64,linux/arm64 ./scripts/build-image.sh

## Build the image in CI, against a chosen frontend ref

Runs `release.yml` on GitHub. Omitting `-f` builds against the frontend's `main`.

    gh workflow run release.yml -f frontend_ref=main
    gh workflow run release.yml -f frontend_ref=v0.2.0

## Run the deployed stack

    docker compose -f deploy/docker-compose.yml up -d

## Adopt a hand-built database inside the container

    docker compose -f deploy/docker-compose.yml run --rm app /migrate baseline

## Start and stop Postgres — run on the host that owns the database

    docker compose up -d
    docker compose down
