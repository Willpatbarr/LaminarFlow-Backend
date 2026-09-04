# Deploying LaminarFlow

One image, two hosts, no host-specific values compiled in. Everything that
differs between the Raspberry Pi, the eventual home server, and a cloud VM comes
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

## Moving the image to a host without a registry

    docker save laminarflow:dev | ssh user@host 'docker load'

## Running

Copy `deploy/.env.example` to `deploy/.env` on the host and fill it in, then:

    docker compose -f deploy/docker-compose.yml up -d

The app reaches Postgres over the compose network as `db`. Postgres publishes no
host port — there is no reason to put a database on the tailnet.

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

## Host: home server (later)

Same image, same compose file, same `.env` shape. Two differences:

**Cockpit** handles host-level administration — updates, disks, systemd — and
stays private to the tailnet. It is not involved in running the app; the app is
compose like anywhere else.

**Storage** is whatever the server's data mount is, set through `PGDATA_HOST`.

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
| Published interface | `APP_BIND` | `127.0.0.1` |
| Image tag | `APP_IMAGE` | `laminarflow:dev` |

Not present, and deliberately so:

* **No base URL or origin setting.** Nothing in the code builds an absolute URL
  yet. Same-origin serving (LAM-28) means the frontend uses relative paths, so
  there is nothing to configure. Add `BASE_URL` when session cookies or outbound
  links need it — not before, because an unused setting is one more thing that
  can be wrong.
* **No storage path setting.** There is no file upload feature yet. The only
  persistent state is Postgres, covered by `PGDATA_HOST`.
* **No Tailscale hostname anywhere.** Tailscale is a property of the host, not
  the app; the app binds a port and does not care what reaches it.
