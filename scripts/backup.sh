#!/bin/sh
# Back up the deployed stack: the Postgres database and the uploaded files folder, as
# ONE dated archive.
#
#   ./backup.sh                      from the deploy dir (~/laminarflow on the home server)
#   COMPOSE_DIR=/path ./scripts/backup.sh
#
# Writes $BACKUP_DIR/laminarflow-backup-<UTC timestamp>.tar.gz (BACKUP_DIR defaults to
# <deploy dir>/backups, mode 700, archive mode 600) holding:
#
#   db.dump      pg_dump --format=custom of the whole database
#   files.tar    the attachments folder (FILES_HOST)
#   MANIFEST     when, schema version, row/file counts, sha256 of the two above
#
# It never stops or restarts anything: the app keeps serving. Consistency comes from
# the order. pg_dump runs in one snapshot; the files are copied AFTER it. Attachment
# files are written once under a uuid and never changed, and a row is only created after
# its file exists, so every file a dumped row names is already on disk when the folder is
# copied. The folder may hold a few files newer than the dump (uploads made meanwhile,
# harmless extras); a file DELETED meanwhile would be reported as missing below.
#
# Heavy parts run at nice 19. The files folder is owned by uid 65532 with mode 700, so it
# is read through a throwaway container rather than as the host user (no sudo needed).
#
# Keeps the newest KEEP archives (default 14) and removes older ones, only after this one
# succeeded. Restore with restore.sh.

set -eu

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

# The same variables compose reads, for FILES_HOST. Compose finds .env itself.
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
[ -d "$FILES" ] || { echo "files folder $FILES does not exist" >&2; exit 1; }

BACKUP_DIR="${BACKUP_DIR:-$DIR/backups}"
KEEP="${KEEP:-14}"
HELPER_IMAGE="${HELPER_IMAGE:-postgres:17}"
STAMP=$(date -u +%Y%m%dT%H%M%SZ)
OUT="$BACKUP_DIR/laminarflow-backup-$STAMP.tar.gz"

sha() { if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1"; else shasum -a 256 "$1"; fi | cut -d' ' -f1; }
dc() { docker compose "$@"; }
# Runs in the db container, whose environment already holds the database name and user.
dbexec() { dc exec -T db sh -c "$1"; }

umask 077
mkdir -p "$BACKUP_DIR"
W=$(mktemp -d "$BACKUP_DIR/.work-XXXXXX")
trap 'rm -rf "$W"' EXIT INT TERM

dbexec 'pg_isready -q -U "$POSTGRES_USER" -d "$POSTGRES_DB"' || { echo "database is not running (docker compose up -d db)" >&2; exit 1; }

echo "==> database"
dbexec 'nice -n 19 pg_dump -U "$POSTGRES_USER" -d "$POSTGRES_DB" --format=custom --no-owner' > "$W/db.dump"
# A dump that cannot be listed is not a backup.
dbexec 'pg_restore -l >/dev/null' < "$W/db.dump"

SCHEMA=$(dbexec 'psql -At -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c "SELECT coalesce(max(version),0) FROM schema_migrations"')
dbexec 'psql -At -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c "SELECT storage_key FROM attachment"' | sort > "$W/keys"
ROWS=$(wc -l < "$W/keys" | tr -d ' ')

echo "==> files ($FILES)"
# Not .tmp: uploads still being written. -m is not needed; the reader is read-only.
docker run --rm -m 256m --cpu-shares 2 -v "$FILES:/files:ro" --entrypoint nice "$HELPER_IMAGE" \
    -n 19 tar -C /files --exclude=./.tmp -cf - . > "$W/files.tar"
tar -tf "$W/files.tar" > /dev/null
tar -tf "$W/files.tar" | awk -F/ 'NF && $NF != "" {print $NF}' | sort > "$W/present"
NFILES=$(wc -l < "$W/present" | tr -d ' ')

MISSING=$(comm -23 "$W/keys" "$W/present" | wc -l | tr -d ' ')
if [ "$MISSING" != 0 ]; then
    echo "WARNING: $MISSING attachment row(s) have no file in the folder (deleted during the backup, or already missing):" >&2
    comm -23 "$W/keys" "$W/present" | head -5 >&2
fi

{
    echo "created_at=$STAMP"
    echo "host=$(hostname)"
    echo "schema_version=$SCHEMA"
    echo "attachment_rows=$ROWS"
    echo "attachment_files=$NFILES"
    echo "attachment_rows_without_file=$MISSING"
    echo "db.dump=$(sha "$W/db.dump")"
    echo "files.tar=$(sha "$W/files.tar")"
} > "$W/MANIFEST"

echo "==> archive"
nice -n 19 tar -C "$W" -cf - db.dump files.tar MANIFEST | nice -n 19 gzip > "$OUT.partial"
mv "$OUT.partial" "$OUT"

# Retention: newest KEEP stay. Only our own dated archives are ever considered.
ls -1 "$BACKUP_DIR"/laminarflow-backup-*.tar.gz 2>/dev/null | sort -r | awk -v k="$KEEP" 'NR>k' | while read -r old; do
    rm -f -- "$old"
    echo "removed old backup $old"
done

echo "OK  $OUT ($(du -h "$OUT" | cut -f1); schema $SCHEMA, $ROWS attachment rows, $NFILES files)"
