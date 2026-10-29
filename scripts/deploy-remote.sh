#!/bin/sh
# Build the image ON a remote Docker host and (re)deploy the stack there.
#
#   ./scripts/deploy-remote.sh willbarr@192.168.4.46
#   REMOTE_DIR=laminarflow FRONTEND_REPO=../LaminarFlow-Frontend ./scripts/deploy-remote.sh user@host
#
# Idempotent: the first run creates the deploy dir, a .env with generated secrets
# (chmod 600) and the data folders; later runs only rebuild and `up -d`, so the
# database and uploaded files are kept. Needs no root on the host - only docker.
#
# What it does:
#   1. git archive of main (backend + frontend) streamed over ssh. Uncommitted
#      work is NOT deployed. Override with BACKEND_REF / FRONTEND_REF.
#   2. Builds the image on the host (amd64) at nice 19 with the build's
#      containers capped at BUILD_CPUS / BUILD_MEM, and a watchdog that aborts if
#      MemAvailable falls under MIN_AVAIL_MB. The host may be doing other work
#      (it runs a Minecraft server) - the build must not starve it.
#   3. `docker compose up -d`; the app applies migrations on start.
#   4. Waits for the app to be healthy and checks /healthz and /healthz/db.
#
# Hosts without the buildx plugin cannot run the Dockerfile as written (BuildKit
# only), so the script falls back to the classic builder using a rewritten copy
# of the Dockerfile; the repository's Dockerfile is never modified.

set -eu
cd "$(dirname "$0")/.."

HOST="${1:?usage: deploy-remote.sh user@host}"
REMOTE_DIR="${REMOTE_DIR:-laminarflow}"
FRONTEND_REPO="${FRONTEND_REPO:-../LaminarFlow-Frontend}"
BACKEND_REF="${BACKEND_REF:-main}"
FRONTEND_REF="${FRONTEND_REF:-main}"
APP_BIND="${APP_BIND:-0.0.0.0}"
APP_PORT="${APP_PORT:-8080}"
BUILD_CPUS="${BUILD_CPUS:-4}"
BUILD_MEM="${BUILD_MEM:-3g}"
MIN_AVAIL_MB="${MIN_AVAIL_MB:-1500}"

[ -f "$FRONTEND_REPO/package.json" ] || { echo "no frontend checkout at $FRONTEND_REPO" >&2; exit 1; }
NODE_VERSION=$(git -C "$FRONTEND_REPO" show "$FRONTEND_REF:.nvmrc" | tr -d ' \t\nv')
GO_VERSION=$(awk '/^go /{split($2,v,"."); print v[1]"."v[2]; exit}' go.mod)
SHA=$(git rev-parse --short "$BACKEND_REF")
TAG="laminarflow:${SHA}"

echo "==> $HOST:~/$REMOTE_DIR  image $TAG (node $NODE_VERSION, go $GO_VERSION)"
ssh "$HOST" "mkdir -p ~/$REMOTE_DIR && cd ~/$REMOTE_DIR && rm -rf build && mkdir -p build/frontend"
git archive "$BACKEND_REF" | ssh "$HOST" "cd ~/$REMOTE_DIR/build && tar -x"
git -C "$FRONTEND_REPO" archive "$FRONTEND_REF" | ssh "$HOST" "cd ~/$REMOTE_DIR/build/frontend && tar -x"

ssh "$HOST" "cat > ~/$REMOTE_DIR/docker-compose.yml" < deploy/docker-compose.yml
# Backup tooling lives beside the compose file (docs/deploy.md); the nightly cron entry is the operator's.
for f in backup.sh restore.sh; do
    ssh "$HOST" "cat > ~/$REMOTE_DIR/$f && chmod 700 ~/$REMOTE_DIR/$f" < "scripts/$f"
done

# The remote half runs as one script so every step shares a shell.
ssh "$HOST" sh -s -- "$REMOTE_DIR" "$TAG" "$NODE_VERSION" "$GO_VERSION" \
    "$APP_BIND" "$APP_PORT" "$BUILD_CPUS" "$BUILD_MEM" "$MIN_AVAIL_MB" <<'REMOTE'
set -eu
DIR="$HOME/$1"; TAG="$2"; NODE="$3"; GO="$4"; BIND="$5"; PORT="$6"; CPUS="$7"; MEM="$8"; MINMB="$9"
cd "$DIR"

# --- .env (created once; never overwritten except the image tag) ------------
if [ ! -f .env ]; then
    umask 077
    {
        echo "APP_IMAGE=$TAG"
        echo "APP_PORT=$PORT"
        echo "APP_BIND=$BIND"
        echo "POSTGRES_DB=laminarflow"
        echo "POSTGRES_USER=laminarflow"
        echo "POSTGRES_PASSWORD=$(openssl rand -hex 24)"
        echo "PGDATA_HOST=$DIR/pgdata"
        echo "FILES_HOST=$DIR/files"
        echo "MAX_ATTACHMENT_BYTES=26214400"
        echo "MIGRATE_ON_STARTUP=true"
    } > .env
    echo "==> created $DIR/.env"
fi
chmod 600 .env
sed -i "s|^APP_IMAGE=.*|APP_IMAGE=$TAG|" .env

# --- data folders ------------------------------------------------------------
mkdir -p pgdata files
# The app runs as uid 65532 and must be able to write the files folder. Without
# root on the host, let a throwaway root container do the chown.
if [ "$(stat -c %u files)" != 65532 ]; then
    docker run --rm -v "$DIR/files:/f" --entrypoint chown postgres:17 65532:65532 /f
fi

# --- build -------------------------------------------------------------------
cd build
if docker buildx version >/dev/null 2>&1; then
    BUILD="docker buildx build --build-context frontendsrc=./frontend"
    DF=Dockerfile
else
    # Classic builder: no BUILDPLATFORM, no syntax directive, and the named
    # context becomes an ordinary stage fed from ./frontend.
    ARCH=$(docker version -f '{{.Server.Arch}}')
    awk '
        /^# syntax=/ { next }
        /^FROM --platform=\$BUILDPLATFORM node/ && !done {
            print "FROM scratch AS frontendsrc"; print "COPY frontend/ /"; print ""; done=1 }
        { sub(/--platform=\$BUILDPLATFORM /, ""); print }' Dockerfile > Dockerfile.classic
    BUILD="docker build --build-arg TARGETARCH=$ARCH --cpu-period 100000 --cpu-quota $((CPUS * 100000)) --memory $MEM --memory-swap $MEM"
    DF=Dockerfile.classic
fi

echo "MemAvailable before build: $(awk '/MemAvailable/{printf "%d MB", $2/1024}' /proc/meminfo)"
# Watchdog: abort the build (our own process only) if the host runs short.
( while sleep 3; do
    avail=$(awk '/MemAvailable/{print int($2/1024)}' /proc/meminfo)
    if [ "$avail" -lt "$MINMB" ]; then
        echo "ABORT: MemAvailable ${avail} MB < ${MINMB} MB" >&2
        pkill -TERM -P "$$" -x docker || true
        break
    fi
  done ) &
WATCH=$!
trap 'kill $WATCH 2>/dev/null || true' EXIT
nice -n 19 ionice -c3 $BUILD -f "$DF" \
    --build-arg "NODE_VERSION=$NODE" --build-arg "GO_VERSION=$GO" -t "$TAG" .
kill $WATCH 2>/dev/null || true
cd ..
rm -rf build
docker tag "$TAG" laminarflow:latest
echo "MemAvailable after build: $(awk '/MemAvailable/{printf "%d MB", $2/1024}' /proc/meminfo)"

# --- deploy ------------------------------------------------------------------
docker compose up -d --remove-orphans
printf '==> waiting for app to be healthy'
i=0
while [ $i -lt 60 ]; do
    st=$(docker inspect -f '{{.State.Health.Status}}' "$(docker compose ps -q app)" 2>/dev/null || echo none)
    [ "$st" = healthy ] && break
    printf .; i=$((i+1)); sleep 3
done
echo " $st"
[ "$st" = healthy ] || { docker compose logs --tail 40 app; exit 1; }
docker compose ps
REMOTE

PORT_URL="http://${HOST#*@}:${APP_PORT}"
echo "==> verifying $PORT_URL"
curl -fsS "$PORT_URL/healthz" && echo
curl -fsS "$PORT_URL/healthz/db" && echo
echo "OK  redeploy with: ./scripts/deploy-remote.sh $HOST"
