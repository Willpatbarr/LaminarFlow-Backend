#!/bin/sh
# Restore a backup made by backup.sh into an EMPTY stack.
#
#   ./restore.sh laminarflow-backup-20261029T031500Z.tar.gz
#   COMPOSE_DIR=/path ./scripts/restore.sh <archive>
#
# Deliberately cannot overwrite anything. It refuses when
#   - the app container is running (stop it, or use a fresh project),
#   - the database already has tables, or
#   - the files folder already has files.
# To restore over an existing install, remove the old state first yourself, on purpose:
#   docker compose down -v && rm -rf pgdata files     (the compose file uses bind mounts,
#   so `-v` alone does not empty them; the folders are what hold the data)
# To rehearse, point COMPOSE_DIR at a scratch directory with its own .env, its own
# COMPOSE_PROJECT_NAME, PGDATA_HOST and FILES_HOST (docs/deploy.md).
#
# What it does: starts ONLY the db service if it is not up, checks the archive's
# checksums, loads db.dump in a single transaction (all or nothing), unpacks the files,
# gives them to uid 65532, then checks that every attachment row has its file. It does not
# start the app: run `docker compose up -d` when it reports OK.

set -eu

ARCHIVE="${1:?usage: restore.sh <backup.tar.gz>}"
[ -f "$ARCHIVE" ] || { echo "no such file: $ARCHIVE" >&2; exit 1; }
ARCHIVE=$(cd "$(dirname "$ARCHIVE")" && pwd)/$(basename "$ARCHIVE")

here=$(cd "$(dirname "$0")" && pwd)
if [ -n "${COMPOSE_DIR:-}" ]; then
    DIR=$(cd "$COMPOSE_DIR" && pwd)
elif [ -f "$here/docker-compose.yml" ]; then
    DIR=$here
else
    DIR=$(cd "$here/../deploy" && pwd)
fi
cd "$DIR"
[ -f docker-compose.yml ] || { echo "no docker-compose.yml in $DIR (set COMPOSE_DIR)" >&2; exit 1; }

# Like compose, the environment wins over .env: remember FILES_HOST across the source.
_files_host="${FILES_HOST-}"
if [ -f .env ]; then
    set -a
    . ./.env
    set +a
fi
[ -z "$_files_host" ] || FILES_HOST="$_files_host"

case "${FILES_HOST:-./files}" in
    /*) FILES="${FILES_HOST:-./files}" ;;
    *) FILES="$DIR/${FILES_HOST#./}" ;;
esac
HELPER_IMAGE="${HELPER_IMAGE:-postgres:17}"

sha() { if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1"; else shasum -a 256 "$1"; fi | cut -d' ' -f1; }
dc() { docker compose "$@"; }
dbexec() { dc exec -T db sh -c "$1"; }

umask 077
W=$(mktemp -d "$DIR/.restore-XXXXXX")
trap 'rm -rf "$W"' EXIT INT TERM

echo "==> target: $DIR (project $(basename "$DIR" | tr 'A-Z' 'a-z')${COMPOSE_PROJECT_NAME:+, COMPOSE_PROJECT_NAME=$COMPOSE_PROJECT_NAME}), files $FILES"

if [ -n "$(dc ps -q --status running app 2>/dev/null)" ]; then
    echo "the app container is running in this project; refusing. Stop it, or restore into a fresh project." >&2
    exit 1
fi
if [ -d "$FILES" ] && [ -n "$(ls -A "$FILES" 2>/dev/null | grep -v '^\.tmp$' || true)" ]; then
    echo "$FILES is not empty; refusing to restore over it." >&2
    exit 1
fi

echo "==> checking the archive"
tar -C "$W" -xzf "$ARCHIVE"
for f in db.dump files.tar MANIFEST; do [ -f "$W/$f" ] || { echo "archive has no $f" >&2; exit 1; }; done
for f in db.dump files.tar; do
    want=$(sed -n "s/^$f=//p" "$W/MANIFEST")
    [ "$(sha "$W/$f")" = "$want" ] || { echo "checksum mismatch on $f: the archive is damaged" >&2; exit 1; }
done

echo "==> starting the database (db service only)"
dc up -d db
i=0
until dbexec 'pg_isready -q -U "$POSTGRES_USER" -d "$POSTGRES_DB"' 2>/dev/null; do
    i=$((i+1)); [ $i -lt 60 ] || { echo "database did not come up" >&2; exit 1; }
    sleep 2
done

TABLES=$(dbexec 'psql -At -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c "SELECT count(*) FROM information_schema.tables WHERE table_schema = '"'public'"'"')
if [ "$TABLES" != 0 ]; then
    echo "the database already has $TABLES table(s); refusing to restore over it." >&2
    exit 1
fi

echo "==> database"
# One transaction: a failure leaves the database as empty as it was found.
dbexec 'pg_restore -U "$POSTGRES_USER" -d "$POSTGRES_DB" --no-owner --single-transaction --exit-on-error' < "$W/db.dump"

echo "==> files"
mkdir -p "$FILES"
docker run --rm -i -v "$FILES:/files" --entrypoint sh "$HELPER_IMAGE" \
    -c 'tar -C /files -xf - && chown -R 65532:65532 /files && chmod 700 /files' < "$W/files.tar"

echo "==> checking"
dbexec 'psql -At -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c "SELECT storage_key FROM attachment"' | sort > "$W/keys"
tar -tf "$W/files.tar" | awk -F/ 'NF && $NF != "" {print $NF}' | sort > "$W/present"
ROWS=$(wc -l < "$W/keys" | tr -d ' ')
MISSING=$(comm -23 "$W/keys" "$W/present" | wc -l | tr -d ' ')
SCHEMA=$(dbexec 'psql -At -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c "SELECT coalesce(max(version),0) FROM schema_migrations"')
echo "restored schema version $SCHEMA, $ROWS attachment rows, $MISSING without a file"
if [ "$MISSING" != 0 ]; then
    echo "WARNING: these attachment rows have no file (they were already missing when the backup was taken):" >&2
    comm -23 "$W/keys" "$W/present" | head -10 >&2
fi

echo "OK  start it with: docker compose up -d"
