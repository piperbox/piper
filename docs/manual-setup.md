# Manual setup (building from source)

`apt install piperd piper` (Linux) / `brew install piperbox/tap/piper` (macOS)
in [getting started](getting-started.md#install) already do everything below
for you. Use this instead if you're building `piperd`/`piper-relay` from
source, on a non-Debian distro, or wiring your own automation.

## Run the agent as a service (manual / from source)

On the box that runs your apps (a Pi, a VPS, a laptop), install the static
`piperd` binary and the shipped systemd unit so the agent runs headless and comes back
on boot (`install` is the coreutils command — copy-into-place with a mode, needing root
to write these system paths). If you built `piperd` locally, the unit file is
already in your checkout:

```bash
sudo install -m 0755 bin/piperd /usr/local/bin/piperd
sudo install -m 0644 packaging/systemd/piperd.service \
  /etc/systemd/system/piperd.service
```

No checkout on hand — e.g. you installed with the diet curl installer instead
of building from source? Fetch the identical unit from the release you
installed; this is exactly what the installer's own next-steps print when it
can't hand off to apt:

```bash
sudo curl -fsSL https://github.com/piperbox/piper/releases/download/vX.Y.Z/piperd.service \
  -o /etc/systemd/system/piperd.service
```

Either way, seed the env file and enable the service (`piperd.env` is
optional — the unit's `EnvironmentFile=-/etc/piper/piperd.env` tolerates a
missing file — but seeding it from the example gives you a documented place
to override defaults):

```bash
sudo install -d -m 0700 /etc/piper
sudo install -m 0600 packaging/systemd/piperd.env.example /etc/piper/piperd.env
sudo systemctl daemon-reload
sudo systemctl enable --now piperd
```

(No checkout for `piperd.env.example` either? Fetch it from the repo instead:
`sudo curl -fsSL https://raw.githubusercontent.com/piperbox/piper/main/packaging/systemd/piperd.env.example -o /etc/piper/piperd.env && sudo chmod 0600 /etc/piper/piperd.env`.)

The unit runs as a `DynamicUser` in the `docker` group (so piperd can drive the host
Docker daemon), keeps state under `PIPER_DATA_DIR=/var/lib/piper`, and binds `:80`/`:443`
via `CAP_NET_BIND_SERVICE` — no root. Edit `/etc/piper/piperd.env` to override defaults
or switch on relay mode. `apt install piperd piper` does all of the above for you;
use the manual steps only when you need to wire it yourself. See the
[end-to-end runbook](runbooks/git-deploy-e2e.md) for verification, logs, and teardown.

## Run the agent on macOS (dev box)

macOS is a **development** target: install via Homebrew and let `brew services`
manage it — no manual unit to write:

```bash
brew install piperbox/tap/piper
brew services start piper
```

`piper agent up`/`down`/`status` wrap the same `brew services` calls. State
lives at `~/.piper/piperd`, and it serves apps at
`http://<name>.piper.localhost`. This path is LAN-only; the relay/public-URL
flow is Linux/Pi (systemd) only.

## Run piperd in Docker (Compose)

Prefer to run `piperd` itself as a container instead of a systemd service? Build and
start it with Compose from the repo root:

```bash
docker compose -f deploy/compose/docker-compose.yml up -d --build
```

This builds the image from the repo's `Dockerfile`, mounts the host's
`/var/run/docker.sock` (piperd drives the **host** Docker daemon as a sibling
container — not Docker-in-Docker), and persists state in the named `piper_data`
volume at `/var/lib/piper`. To override defaults, copy the env file and point
`env_file` at your copy instead of the tracked example:

```bash
cp packaging/systemd/piperd.env.example deploy/compose/piperd.env
```

Then edit `deploy/compose/piperd.env` and change the `env_file:` entry in
`docker-compose.yml` to `deploy/compose/piperd.env` (already gitignored).

**Networking:** the compose file sets `network_mode: host`, which is required, not
optional. `piperd` publishes every app container's port to `127.0.0.1` on the
Docker host and dials that same address for health checks, so `piperd` must share
the host's network namespace to see them — otherwise `127.0.0.1` inside piperd's
own container would be its own empty loopback, not the host's. Host networking
also lets the embedded Caddy manager bind `:80`/`:443` directly. This is Linux-only,
matching the rest of the service-install path.

**Trust:** mounting `docker.sock` grants the container root-equivalent control over
the host's Docker daemon — the same trust boundary the systemd unit already accepts
via its `docker` group membership (see the previous section). Only run this on a
box you already trust with root.

## Run the relay as a service

On a Linux relay host, build or download the static `piper-relay` binary, then install
the binary and the shipped systemd unit:

```bash
sudo install -m 0755 bin/piper-relay /usr/local/bin/piper-relay
sudo install -m 0644 packaging/systemd/piper-relay.service \
  /etc/systemd/system/piper-relay.service
sudo systemctl daemon-reload
```

The relay stores everything in Postgres; create a database and put its URL in
`/etc/piper-relay.env` as `PIPER_RELAY_DB_URL` first (see the
[relay runbook](runbooks/relay-deploy.md#2-configure)).

Enroll the box before starting the service, then enable it at boot:

```bash
sudo systemd-run --pipe --wait --collect \
  --property=DynamicUser=yes \
  --property=StateDirectory=piper-relay \
  --setenv=PIPER_RELAY_DATA_DIR=/var/lib/piper-relay \
  --setenv=PIPER_RELAY_DB_URL=postgres://… \
  /usr/local/bin/piper-relay enroll <name> --domain <base-domain>
sudo systemctl enable --now piper-relay
```

Open inbound TCP ports `443` and `7000`. See the
[end-to-end runbook](runbooks/git-deploy-e2e.md#part-b--relay) for verification,
address overrides, logs, and teardown.
