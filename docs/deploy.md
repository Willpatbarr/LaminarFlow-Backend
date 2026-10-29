# Deploying LaminarFlow

One image, two hosts, no host-specific values compiled in. Everything that
differs between the Raspberry Pi, the home server, and a cloud VM comes
from the environment.

The image itself is generic. This document is the only place host-specific
instructions live.

## What is in the image

| Path | What it is |
|------|------------|
| `/laminarflow` | The server. Entrypoint. Serves the API and the frontend on one port. |
| `/migrate` | Schema tool — `up`, `down [n]`, `status`, `baseline`. |
| `/reindex` | Rebuilds `search_index` from the document blobs. |

Base is `gcr.io/distroless/static-debian12:nonroot` — CA certificates, timezone
data, and a nonroot user. No shell, no package manager, no libc. The frontend
bundle is embedded in `/laminarflow`, not a separate directory.

## Building

The frontend is a separate repository, so it arrives as a BuildKit named context
rather than being cloned during the build. `scripts/build-image.sh` supplies it
along with the Node and Go versions, read from `.nvmrc` and `go.mod`:

    ./scripts/build-image.sh                                  # native arch
    PLATFORMS=linux/arm64 ./scripts/build-image.sh             # for a 64-bit Pi
    PLATFORMS=linux/amd64,linux/arm64 ./scripts/build-image.sh # both, no load

`FRONTEND_REPO` overrides where the frontend checkout is found; it defaults to
`../LaminarFlow-Frontend`.

**Build on a workstation, not on the Pi.** The frontend stage is the expensive
one and will thrash or run out of memory on a Pi with under 4GB. On Apple
Silicon you are already arm64, so a plain build produces a Pi-compatible image
with no emulation at all.

A multi-platform build cannot be loaded into the local image list — that list
holds one architecture per tag — so a comma in `PLATFORMS` builds and verifies
without loading. That is the right form for CI; use a single platform with
`--load` when you actually want to run it.

### Building in CI

`.github/workflows/release.yml` builds the image on a `v*` tag, on a pull
request touching the Dockerfile or `scripts/build-image.sh`, and on demand from
the Actions tab. A manual run takes a `frontend_ref` input — the frontend branch
or tag the bundle is built from, defaulting to `main`. `ci.yml` runs the Go
checks and never builds an image.

## Moving the image to a host without a registry

    docker save laminarflow:dev | ssh user@host 'docker load'

## Running

Copy `deploy/.env.example` to `deploy/.env` on the host and fill it in, then:

    docker compose -f deploy/docker-compose.yml up -d

The app reaches Postgres over the compose network as `db`. Postgres publishes no
host port — there is no reason to put a database on the tailnet.

### Uploaded files

Attachments and images (LAM-90) are kept as files in `FILES_HOST` on the host, mounted at
`/data/files` in the container (`ATTACHMENT_DIR`). **This folder is state, like the
database: back it up with Postgres.** The database holds each file's name, type and size;
the folder holds the bytes. A restore needs both from the same moment, and a database
restored without its folder leaves attachments whose download is a 404.

The app runs as the `nonroot` user (uid 65532), so a bind-mounted folder must be writable
by it, or the server refuses to start with "not writable":

    mkdir -p files && sudo chown 65532:65532 files

Point `FILES_HOST` at the data disk, next to `PGDATA_HOST`. The per-file size limit is
`MAX_ATTACHMENT_BYTES` (default 25 MiB). A reverse proxy in front of the app needs a body
limit at least that large.

### First run against an existing database

A database whose schema was built by hand before the migration runner existed
has the tables but no `schema_migrations`, and the server will refuse to start.
Adopt it once:

    docker compose -f deploy/docker-compose.yml run --rm app /migrate baseline

`baseline` records every migration as applied without running any, and refuses on
a database that already has migration history, so it cannot hide a failed run.

### Upgrading

    docker compose -f deploy/docker-compose.yml pull   # or docker load a new image
    docker compose -f deploy/docker-compose.yml up -d

`MIGRATE_ON_STARTUP` defaults to `true` in the deployed stack, so the schema
follows the image. Set it to `false` and run `/migrate up` by hand if you would
rather see migrations as a separate step.

## Debugging without a shell

The image has no shell, which is deliberate for a network-exposed service. What
to reach for instead:

    docker compose -f deploy/docker-compose.yml logs -f app
    docker compose -f deploy/docker-compose.yml exec db psql -U laminarflow
    docker compose -f deploy/docker-compose.yml run --rm app /migrate status
    curl localhost:8080/healthz
    curl localhost:8080/healthz/db

The container's own healthcheck runs `/laminarflow healthcheck`, which asks the
server over loopback rather than needing curl. `docker inspect` shows its
history.

If you genuinely need a shell in the image, rebuild the final stage on
`alpine:3` instead of distroless — one line in the `Dockerfile`. Prefer not to
ship that.

---

## Host: Raspberry Pi (current)

Interim host while the home server is not yet reachable. Nothing about the image
changes; only this section does.

**Check the architecture first.** `uname -m` must print `aarch64`. A 32-bit
`armv7l` install cannot run the arm64 image, and no build flag fixes that — it
needs a 64-bit OS.

**Storage.** Point `PGDATA_HOST` at the USB disk, not the SD card. Postgres will
wear out an SD card and the failure is silent until it is not.

**Memory limits.** `mem_limit` in compose is ignored on Raspberry Pi OS unless
cgroup memory accounting is on. Add to `/boot/firmware/cmdline.txt`, on the
single existing line, then reboot:

    cgroup_enable=memory cgroup_memory=1

Confirm with `docker info` — it warns when the limit support is missing.

**If you build on the Pi anyway**, raise swap first. Raspberry Pi OS ships about
100MB, which npm will go straight through:

    sudo dphys-swapfile swapoff
    sudo sed -i 's/^CONF_SWAPSIZE=.*/CONF_SWAPSIZE=2048/' /etc/dphys-swapfile
    sudo dphys-swapfile setup && sudo dphys-swapfile swapon

**Restart at boot** is handled by `restart: unless-stopped`. There is no Cockpit
here and none is needed.

## Host: home server (Ubuntu PC, current)

The home server is an x86_64 Ubuntu PC on the LAN (`willbarr@192.168.4.46`). It
also runs a Minecraft server, so the deploy is built to be a polite neighbour:
the image is built there at `nice 19` with a CPU and memory cap, a watchdog aborts
the build if available memory drops below 1.5 GB, and the containers have memory
limits (app 512m, Postgres 1g). Ports 25565, 25575 and 8443 belong to other
things; the app publishes only **8080**.

**Deploy and redeploy, from a checkout on your workstation (no root needed):**

    ./scripts/deploy-remote.sh willbarr@192.168.4.46

It streams `git archive` of `main` from both repositories (committed work only)
to `~/laminarflow/build` on the PC, builds the amd64 image there, runs
`docker compose up -d`, waits for the container to report healthy and checks
`/healthz` and `/healthz/db`. The database and uploaded files are kept. The
first run also creates everything below. `BACKEND_REF`, `FRONTEND_REF`,
`FRONTEND_REPO`, `REMOTE_DIR`, `APP_BIND`, `BUILD_CPUS`, `BUILD_MEM` and
`MIN_AVAIL_MB` override the defaults.

| What | Where on the PC |
|------|-----------------|
| Compose file | `~/laminarflow/docker-compose.yml` (copied from `deploy/`) |
| Secrets and settings | `~/laminarflow/.env`, mode 600, generated on first run, never committed |
| Postgres data | `~/laminarflow/pgdata` |
| Uploaded files | `~/laminarflow/files`, chowned to 65532 by a throwaway container |

Back up `pgdata` and `files` together. The app is at `http://192.168.4.46:8080`
(`APP_BIND=0.0.0.0` in that `.env`: LAN only; Tailscale exposure is not
configured). It restarts on boot through `restart: unless-stopped` and the
systemd-enabled Docker service. Each deploy tags the image `laminarflow:<sha>`
and records it as `APP_IMAGE`, so rolling back the image is editing that line
and `docker compose up -d` (see the migration caveat in `deploy/README.md`).

The PC has no `buildx` plugin, so the script falls back to the classic builder
with a rewritten copy of the Dockerfile (generated on the PC, not committed).

**Cockpit** handles host-level administration - updates, disks, systemd - and
stays private to the tailnet. It is not involved in running the app.

## Both hosts: Tailscale

    curl -fsSL https://tailscale.com/install.sh | sh && sudo tailscale up

The app is then reachable at `http://<host>.<tailnet>.ts.net:<APP_PORT>` from
anything on the tailnet.

`APP_BIND` defaults to `127.0.0.1`, which Tailscale can still route to while the
local network cannot. Setting it to `0.0.0.0` exposes the app to the LAN — do
that only on purpose.

---

## Portability audit

The check the ticket asks for: nothing in the image or its configuration assumes
a particular machine.

| Value | Where it comes from | Default |
|-------|--------------------|---------|
| Listen port | `PORT` | `8080` |
| Database connection | `DATABASE_URL` | none — the server refuses to start without it |
| Migrate on startup | `MIGRATE_ON_STARTUP` | `false` in the binary, `true` in the deployed stack |
| Frontend bundle | embedded; `FRONTEND_DIR` overrides | embedded |
| Postgres data path | `PGDATA_HOST` | `./pgdata` |
| Uploaded files path (host) | `FILES_HOST` | `./files` |
| Uploaded files path (in the container) | `ATTACHMENT_DIR` | `data/files` in the binary, `/data/files` in the deployed stack |
| Largest upload | `MAX_ATTACHMENT_BYTES` | `26214400` (25 MiB) |
| Published interface | `APP_BIND` | `127.0.0.1` |
| Image tag | `APP_IMAGE` | `laminarflow:dev` |

Not present, and deliberately so:

* **No base URL or origin setting.** Nothing in the code builds an absolute URL
  yet. Same-origin serving (LAM-28) means the frontend uses relative paths, so
  there is nothing to configure. Add `BASE_URL` when session cookies or outbound
  links need it — not before, because an unused setting is one more thing that
  can be wrong.
* **No object-storage setting.** Uploaded files are a local folder
  (`ATTACHMENT_DIR`) behind an interface (ADR 7); a cloud backend is added when a
  cloud host needs one. Persistent state is therefore two things: Postgres
  (`PGDATA_HOST`) and the files folder (`FILES_HOST`).
* **No Tailscale hostname anywhere.** Tailscale is a property of the host, not
  the app; the app binds a port and does not care what reaches it.
