# Deploying LaminarFlow

`deploy/docker-compose.yml` is the production stack: `app` and `db`. This page is
the short version of install, upgrade and rollback. `docs/deploy.md` has the
detail (hosts, env variables, backups, Tailscale).

## First install

Follow "Running" and "Host" in [`docs/deploy.md`](../docs/deploy.md). In short:
copy `.env.example` to `.env`, set `POSTGRES_PASSWORD` and the host paths
(`PGDATA_HOST`, `FILES_HOST`), then

    docker compose -f deploy/docker-compose.yml up -d

For the home server, `scripts/deploy-remote.sh user@host` does all of this over
ssh. Migrations run on start
(`MIGRATE_ON_STARTUP=true`).

The `app` service is healthy only when `/healthz/db` passes (the binary checks
itself with `/laminarflow healthcheck /healthz/db`; the image is distroless, so
there is no curl). Check with `docker compose -f deploy/docker-compose.yml ps`.

## Upgrade

Build or pull the new image, point `APP_IMAGE` at it, and recreate:

    docker compose -f deploy/docker-compose.yml up -d

The app applies any pending migrations on start, and the data in `PGDATA_HOST`
and `FILES_HOST` is kept. On the home server, run `scripts/deploy-remote.sh`
instead; it tags each image `laminarflow:<sha>` and records it as `APP_IMAGE`.
Take a backup first if the release contains a migration (`docs/deploy.md`,
"Backup, restore and export").

## Rollback

**Rolling back the image without rolling back the schema is unsafe once a new
migration has run.** The old binary does not know the new tables and columns. It
refuses to start, logging "database schema is newer than this binary" and exiting
non-zero, rather than serving against a schema it does not match.

To roll back after a release that included migrations:

1. Stop the app: `docker compose -f deploy/docker-compose.yml stop app`.
2. Roll the schema back with the NEW image, which still has the migration files.
   `<n>` is the number of migrations that release added:

       docker compose -f deploy/docker-compose.yml run --rm app /migrate down <n>

   `/migrate status` lists what is applied. A migration with no down script is
   refused; restore the pre-upgrade backup instead.
3. Set `APP_IMAGE` back to the previous tag (`laminarflow:<sha>`) in `.env`.
4. `docker compose -f deploy/docker-compose.yml up -d`.

If the release had no migrations, only steps 3 and 4 are needed. `migrate down`
destroys whatever the rolled-back migrations created, so data written to new
tables since the upgrade is lost; restoring the backup is the alternative.
