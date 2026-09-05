# Relay Deploy & Upgrade Runbook — `piper-relay`

How to stand up a self-hosted relay from a release, and — the part no other doc
covers — how to **upgrade a running relay**, including across a schema change.
The hosted relay runs this same open-source binary; this procedure applies to it
too.

Building from source instead? [manual-setup.md](../manual-setup.md#run-the-relay-as-a-service)
covers the from-checkout install; [git-deploy-e2e.md Part B](git-deploy-e2e.md)
covers full end-to-end verification with a box and DNS. This runbook assumes a
Linux host with a public IP and systemd; for container platforms see
[Run as a container](#run-as-a-container).

---

## What's on disk

| Path | What |
| --- | --- |
| `/usr/local/bin/piper-relay` | the binary |
| `/etc/systemd/system/piper-relay.service` | the shipped unit (`DynamicUser`, `StateDirectory=piper-relay`) |
| `/var/lib/piper-relay/` | TLS wildcard pair and (via a credential drop-in) the GitHub App key — no database lives here |
| `/etc/piper-relay.env` | env overrides read by the unit (`EnvironmentFile=-`); **must** carry `PIPER_RELAY_DB_URL` |
| Postgres, at `PIPER_RELAY_DB_URL` | **all relay state** — agents, tokens, accounts, orgs, domains, installations, parked webhooks |

The store applies `schema.sql` with `CREATE TABLE IF NOT EXISTS` on every start
and there are **no migrations** (pre-1.x policy): a release that adds a *table*
upgrades in place; a release that changes an *existing table* needs that table
dropped (or the database recreated) before the new binary starts. Each
release's notes say which case it is — or check yourself:

```bash
git diff <old-tag>..<new-tag> -- internal/relay/schema.sql
```

New `CREATE TABLE` → in-place. Changed `CREATE TABLE` body → drop that table (see
[Upgrade across a schema change](#upgrade-across-a-schema-change)).

---

## Fresh deploy

### 1. Install the binary and unit

Grab the tarball for the host's platform from the release
(`linux_amd64` / `linux_arm64` / `linux_armv7`):

```bash
VER=0.16.0-rc.1   # target version, no leading v
curl -fsSLO "https://github.com/piperbox/piper/releases/download/v${VER}/piper-relay_${VER}_linux_amd64.tar.gz"
tar xzf "piper-relay_${VER}_linux_amd64.tar.gz"
sudo install -m 0755 piper-relay /usr/local/bin/piper-relay
curl -fsSLO "https://github.com/piperbox/piper/releases/download/v${VER}/piper-relay.service"
sudo install -m 0644 piper-relay.service /etc/systemd/system/piper-relay.service
sudo systemctl daemon-reload
```

### 2. Configure

The relay needs a Postgres database (13 or newer; 17 is what the tests run).
On a single host the distribution package is the simplest pairing with a
systemd-run relay:

```bash
sudo apt install postgresql
sudo -u postgres psql -c "CREATE ROLE piper_relay LOGIN PASSWORD '<password>'"
sudo -u postgres psql -c "CREATE DATABASE piper_relay OWNER piper_relay"
```

Then, **required**, in `/etc/piper-relay.env`:

```bash
PIPER_RELAY_DB_URL=postgres://piper_relay:<password>@127.0.0.1:5432/piper_relay
```

The relay creates its tables on first start. Any `admin` / `enroll` invocation
needs the same variable — the transient-unit commands below pass it with
`--setenv`.

The shipped systemd unit orders itself after `postgresql.service` and retries
indefinitely (no start-limit) if the database isn't reachable yet, so a slow
Postgres on boot doesn't leave the relay permanently failed.

Defaults work for a passthrough-only relay otherwise. Anything else goes in
`/etc/piper-relay.env`:

```bash
# Listeners (defaults shown)
PIPER_RELAY_TLS_ADDR=:443
PIPER_RELAY_HTTP_ADDR=:80
PIPER_RELAY_TUNNEL_ADDR=:7000
PIPER_RELAY_API_ADDR=:8080

# Shared-domain identity + quotas
PIPER_RELAY_APEX=public.example.dev     # default public.getpiper.dev
PIPER_RELAY_MAX_AGENTS=3
PIPER_RELAY_MAX_APPS=10
PIPER_RELAY_MAX_DOMAINS=5

# Self-service login (`piper login` against this relay) — omit to disable
PIPER_RELAY_GITHUB_CLIENT_ID=...
PIPER_RELAY_GITHUB_CLIENT_SECRET=...

# Shared-domain TLS termination — omit for passthrough-only
PIPER_RELAY_TLS_CERT=/var/lib/piper-relay/wildcard.crt
PIPER_RELAY_TLS_KEY=/var/lib/piper-relay/wildcard.key

# PROXY protocol v2 on :443/:80/:7000 (#485) — set ONLY when a trusted L4 load
# balancer / TCP reverse proxy is the sole path to those ports. With it on,
# every connection must open with a PROXY v2 header (missing or malformed ⇒
# the connection is dropped), and rate limiting + logs see the real client IP.
# Left off, or on with the ports reachable directly, anyone can spoof their
# source IP.
#PIPER_RELAY_PROXY_PROTOCOL=1
```

The wildcard pair is re-read when it changes on disk, so a renewal — a certbot
deploy hook, a cert-manager Secret remount — is served from the next handshake
with no restart and no dropped tunnels (#484). Write the new pair over the same
paths; a pair caught half-written is logged and the previous certificate keeps
serving until a complete one lands.

Most renewal tools write the cert and the key as two separate, non-atomic
writes, so during a normal renewal you can expect one or more
`serving the last good certificate` log lines. That is not an incident signal
by itself: the relay converges to the new pair as soon as both files land.

Without `PIPER_RELAY_GITHUB_CLIENT_ID` the relay logs
`self-service login disabled` — boxes can then only be operator-enrolled
(`piper-relay enroll`, see manual-setup). `PIPER_RELAY_FAKE_APPROVE=1` is for
tests only; never set it on a public relay.

### 3. Start and verify

```bash
sudo systemctl enable --now piper-relay
sudo journalctl -u piper-relay -n 20 --no-pager
sudo ss -lnt '( sport = :443 or sport = :7000 or sport = :8080 )'
```

Expect listeners on all three ports (plus `:80`), and no `disabled` log lines
you didn't intend. Open `443`, `80`, `7000`, and `8080` inbound in the host and
provider firewalls; point DNS (`*.apex` and the apex, plus any per-account
subdomains) at the host.

First real check from a box: `piper login --relay https://<api-host>:8080`
claims it end-to-end.

---

## Upgrade, same schema

For a release whose notes say no relay schema change:

```bash
VER=<new-version>
curl -fsSLO "https://github.com/piperbox/piper/releases/download/v${VER}/piper-relay_${VER}_linux_amd64.tar.gz"
tar xzf "piper-relay_${VER}_linux_amd64.tar.gz"
sudo install -m 0755 piper-relay /usr/local/bin/piper-relay
sudo systemctl restart piper-relay
```

On SIGTERM the relay drains before it exits (#523): its pool row flips to
`draining` so an edge places nothing new on it, new tunnels are refused, and
each agent session is closed once it has no open streams — up to 20 s — then
the row is deleted and in-flight webhook deliveries park (up to 35 s more).
Give it a 60 s stop timeout: systemd's default `TimeoutStopSec` (90 s)
already covers it; on compose that is `stop_grace_period: 60s`, on ECS
`stopTimeout: 60`, on Kubernetes `terminationGracePeriodSeconds: 60`.
The relay reads the signal channel once, so a second SIGTERM (another
`docker stop` or Ctrl-C) during the drain changes nothing — the only
escalation is SIGKILL, which the orchestrator sends once the stop timeout
elapses.
Tunnels still drop when their session closes and agents reconnect on their
own (piperd's tunnel client retries in the background) until #530 gives
each agent a second session. Verify: `journalctl -u piper-relay -f` shows
agents re-registering within a minute, and `piper box ls` from an enrolled
account shows `connected`.

Keep the previous binary around (`/usr/local/bin/piper-relay.prev`) — rollback
is reinstall + restart.

---

## Upgrade across a schema change

Example: v0.16.0 added `agents.box_id` — a column on an existing table, so an
old database will not gain it and the new binary's queries against it fail.

**Order matters: relay first, then agents.** The agent side (one-command login,
daemon-owned enrollment) expects the new relay's enroll semantics; upgrading
agents against an old relay leaves them creating duplicate rows instead of
upserting.

Path (b) below resets **all** relay state — every agent row, token, account
link, and custom domain — and boxes re-enroll afterwards; that is the designed
recovery path, not an accident to engineer around, and the rest of this
section describes recovering from it. Path (a) resets nothing except the
dropped table(s): for the self-healing tables it names, the data it holds
comes back on its own once boxes reconnect, so there's no broader recovery to
walk through.

`relay_instances` and `agent_owners` are ephemeral — heartbeats re-create
the first, registration and the heartbeat's ownership re-assert the second —
so a release that changes either is dropped with
`DROP TABLE agent_owners, relay_instances;`, both together. Dropping only
`relay_instances` with `CASCADE` would remove the foreign key from
`agent_owners`, and `CREATE TABLE IF NOT EXISTS` would never put it back.
The drop is mandatory and is the only thing that matters for this release:
both `piper-relay` and `piper-edge` apply the same `schema.sql`, so after
the drop whichever binary opens the store first re-creates the tables with
the new column. Skip the drop and every new relay's heartbeat fails,
leaving the whole pool unplaceable regardless of roll order.

```bash
sudo systemctl stop piper-relay
sudo cp /usr/local/bin/piper-relay /usr/local/bin/piper-relay.prev
sudo -u postgres pg_dump piper_relay > "relay-$(date +%Y%m%d).sql"   # rollback point
# install the new binary as in the same-schema upgrade, then EITHER
# (a) drop only the changed table(s) — the release notes name them:
sudo -u postgres psql piper_relay -c "DROP TABLE <table>"
# OR (b) start from empty, which resets ALL relay state:
sudo -u postgres psql -c "DROP DATABASE piper_relay" -c "CREATE DATABASE piper_relay OWNER piper_relay"
sudo systemctl start piper-relay
```

Prefer (a) whenever the changed table is one the boxes repopulate on
reconnect (`hostnames`, `repo_bindings`, `custom_domains`, `pending_events`)
or one you can rebuild by hand in `psql`; fall back to (b) otherwise.

The bare `DROP TABLE <table>` above works as-is for those four leaf tables.
With foreign keys enforced, a parent table (`agents`, `accounts`) refuses a
bare drop; it needs `DROP TABLE <table> CASCADE`, which also drops every
child row across the schema — at that point prefer path (b) instead.

The fresh tables materialize on first start after (b). Then, per box:

- **Self-service boxes:** upgrade piperd, then `piper login --re-enroll`
  (re-claims the box; base domain is re-minted).
- **Operator-enrolled boxes:** re-run the `piper-relay enroll` transient-unit
  command from [manual-setup.md](../manual-setup.md#run-the-relay-as-a-service)
  and hand the new `rlyt_…` token to the box's `PIPER_RELAY_TOKEN`.

### What comes back on its own

Most relay state is a *projection* of what the boxes hold, so it is restored by
the reconnect rather than by you. Once a box is enrolled again:

- **Custom domains** re-appear. The agent keeps its own domain rows and cert on
  disk, and the relay re-derives the mapping when the tunnel session registers.
  There is nothing to re-add — `piper domains add` on a domain the box already
  holds answers `409 domain already exists`. (Only a box that never reconnects
  needs its domains re-created, on whichever box replaces it.)
- **Repo bindings** re-appear: piperd re-pushes every binding, and its whole app
  set, on each tunnel (re)connect.
- **GitHub App links** re-appear at the next `piper login`: when an account has
  no installations on record the relay asks GitHub's API directly and links back
  what that account owns.

### What does not

An **org-target installation with no linked Piper org** is the one gap: the
relay cannot attribute it without the installing user's identity, which only
the webhook carries. Re-fire that webhook by hand — GitHub org (or user)
settings → GitHub Apps → **suspend**, then **unsuspend** the Piper
installation, which sends an `unsuspend` event and links it. Symptom if you
skip it: `piper github repos` stays empty for that org's boxes.

Verify as in the same-schema upgrade, plus one idempotency check: run
`piper login` a second time from any box — it must print
`already enrolled as <domain>` and `piper box ls` must show exactly one row for
that box. It must not ask for a device code the second time: a saved credential
that still works skips the browser trip.

`piper login` ends with an advisory GitHub-App install poll — the `Waiting…`
dots. It is bounded (ten minutes) and exits 0 regardless; enrollment already
succeeded by the time it runs, so dots are never a hung enrollment.

Rollback within the window: stop the service, restore `piper-relay.prev` and
restore the `pg_dump` (`psql piper_relay < relay-<date>.sql` into a freshly
created database), start. Old tokens become valid again; boxes that already
re-enrolled against the new DB must `piper login --re-enroll` once more.

---

## Ops surface

- Logs: `journalctl -u piper-relay -f`
- Admin CLI (runs against the same `PIPER_RELAY_DB_URL`): `piper-relay admin …`
  via the same transient-unit pattern as `enroll`
- Optional ops listener on `127.0.0.1:9090` (`PIPER_RELAY_OPS_ADDR`);
  `PIPER_RELAY_METRICS=1` / `PIPER_RELAY_LOGS=1` to enable metrics/log
  endpoints

---

## Run as a container

Every release also publishes a multi-arch (amd64/arm64) image to
`ghcr.io/piperbox/piper-relay` — version tags for every release including RCs,
`:latest` for finals only. It is the same binary on a distroless base; all the
env vars above apply unchanged.

The standard self-host path is one compose file — relay plus Postgres:

```yaml
services:
  postgres:
    image: postgres:17
    restart: unless-stopped
    environment:
      POSTGRES_USER: piper_relay
      POSTGRES_PASSWORD: change-me
      POSTGRES_DB: piper_relay
    volumes:
      - relay_pg:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U piper_relay -d piper_relay"]
      interval: 5s
      timeout: 3s
      retries: 10
  relay:
    image: ghcr.io/piperbox/piper-relay:<version>
    restart: unless-stopped
    depends_on:
      postgres:
        condition: service_healthy
    env_file: piper-relay.env
    environment:
      PIPER_RELAY_DB_URL: postgres://piper_relay:change-me@postgres:5432/piper_relay
    ports: ["443:443", "80:80", "7000:7000", "8080:8080"]
    volumes:
      - ./certs:/var/lib/piper-relay:ro
volumes:
  relay_pg:
```

- **State** lives in Postgres. On K8s/ECS point `PIPER_RELAY_DB_URL` at a
  managed instance and give the relay no volume at all beyond its certs.
- **Certs and the GitHub App key** are file mounts, as before; the key must
  not be world-readable (mount `0600`).
- **Admin/enroll** run in the relay container against the same database:
  `docker compose exec relay piper-relay admin …`.
- **Upgrade** = pull the new tag, recreate the relay container. The
  schema-change caveats above apply identically — drop the named table(s) in
  `psql` first for a schema-change release.
- **More than one replica:** put `piper-edge` in front — see [Scale out](#scale-out).
  Without it, scale up, never out: each agent's tunnel lives in one relay
  process's memory and traffic for it must reach that process.
- **Fronting:** custom-domain :443 is SNI passthrough (certs live on the
  boxes) and :7000 is a raw TCP protocol — only an L4/TCP ingress can sit in
  front, never an HTTP(S)-terminating one. Behind any L4 proxy the relay sees
  the proxy's IP ([#485](https://github.com/piperbox/piper/issues/485)); with
  a direct public IP (host networking) nothing is lost.

## Scale out

`piper-edge` is the only public entrypoint; `piper-relay` runs as N processes
behind it and nothing else changes. Design:
[`docs/superpowers/specs/2026-09-04-relay-edge-ownership-design.md`](../superpowers/specs/2026-09-04-relay-edge-ownership-design.md).

**How it routes.** Each agent's tunnel is terminated by exactly one relay,
which records itself as the owner in Postgres (`relay_instances` with a 5 s
heartbeat, `agent_owners`). The edge keeps an in-memory copy, refreshed by
LISTEN/NOTIFY and a 15 s poll, and forwards raw bytes:

| Listener | Decision |
| --- | --- |
| `:443` | SNI → agent → owner relay. `api.<apex>` is pinned to the earliest-started relay until login-flow state moves to Postgres. |
| `:80` | Host → custom domain → owner relay (custom domains only, as today). |
| `:7000` | the live relay with the fewest sessions. |

Control-API calls that land on a non-owner relay hop to the owner over the
internal `:8080`; webhooks parked by any relay wake the owner. Relays never
forward data traffic to each other.

**Client IP chain.** The edge writes a PROXY v2 header on every backend
connection, so every relay behind an edge **must** run with
`PIPER_RELAY_PROXY_PROTOCOL=1` (and must not be reachable except through the
edge). If a cloud balancer that speaks PROXY protocol sits in front of the
edge, set `PIPER_EDGE_PROXY_PROTOCOL=1` on the edge too.

**Edge configuration.** `PIPER_EDGE_DB_URL` (the relays' database) and
`PIPER_EDGE_APEX` are required; `PIPER_EDGE_TLS_ADDR` / `HTTP_ADDR` /
`TUNNEL_ADDR` default to `:443` / `:80` / `:7000`; `PIPER_EDGE_OPS_ADDR`,
`PIPER_EDGE_METRICS`, `PIPER_EDGE_LOGS` mirror the relay's ops surface. The
edge holds no certificate, no GitHub App and no relay env: a relay env file
handed to it fails on the missing `PIPER_EDGE_*` names.

**Relay configuration.** `PIPER_RELAY_ADVERTISE_HOST` is the host edges dial;
it defaults to the first non-loopback IPv4 (the container or pod IP), which
is right on a bridge network and in a pod. Ports are the relay's own listener
ports.

**Single host (compose).** The edge owns the public ports on the host
network; relays scale on the bridge network with no published ports:

```yaml
services:
  postgres:
    image: postgres:17
    restart: unless-stopped
    environment:
      POSTGRES_USER: piper_relay
      POSTGRES_PASSWORD: change-me
      POSTGRES_DB: piper_relay
    ports:
      - "127.0.0.1:5432:5432"
    volumes:
      - relay_pg:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U piper_relay -d piper_relay"]
      interval: 5s
      timeout: 3s
      retries: 10
  edge:
    image: ghcr.io/piperbox/piper-edge:<version>
    restart: unless-stopped
    network_mode: host
    depends_on:
      postgres:
        condition: service_healthy
    environment:
      PIPER_EDGE_DB_URL: postgres://piper_relay:change-me@127.0.0.1:5432/piper_relay
      PIPER_EDGE_APEX: public.getpiper.dev
  relay:
    image: ghcr.io/piperbox/piper-relay:<version>
    restart: unless-stopped
    depends_on:
      postgres:
        condition: service_healthy
    env_file: piper-relay.env
    environment:
      PIPER_RELAY_DB_URL: postgres://piper_relay:change-me@postgres:5432/piper_relay
      PIPER_RELAY_PROXY_PROTOCOL: "1"
      PIPER_RELAY_API_ADDR: ":8080"
    volumes:
      - ./certs:/var/lib/piper-relay:ro
volumes:
  relay_pg:
```

Postgres must publish `127.0.0.1:5432` for the host-networked edge (the
snippet above and the Hetzner file both do). Add capacity with
`docker compose up -d --scale relay=3`; nothing is configured. The relay's
`:8080` is reachable only on the bridge network, which is what the control hop
relies on. Each relay's ops endpoint is on its own container IP; scrape it
there or publish it per replica.

**Kubernetes.** `piper-edge` as a Deployment behind a TCP Service that holds
the public IP (or a cloud NLB with PROXY protocol and
`PIPER_EDGE_PROXY_PROTOCOL=1`; otherwise `externalTrafficPolicy: Local`).
`piper-relay` as a Deployment with `PIPER_RELAY_ADVERTISE_HOST` from the
downward API (`status.podIP`), `PIPER_RELAY_PROXY_PROTOCOL=1`, and a
NetworkPolicy admitting only edge pods and other relays. An L7 ingress may
terminate `api.<apex>` with a cert-manager certificate and route it to the
relays' `:8080` Service — that port is plain HTTP written to be fronted with
TLS — with DNS pointing `api.<apex>` at the ingress and the wildcard at the
edge. The ingress must never take the wildcard: per-hostname routing to the
owning pod is the dynamic map the edge exists to hold, and box-held
certificates need L4 passthrough anyway.

**What still drops.** A relay restart closes each of its tunnels once the
tunnel has no streams in flight (#523), so requests finish but the agent's
hostnames are dark for the 1–3 s it takes to redial onto a survivor; #530
(two sessions per agent) closes that gap. Restarting the edge on a single
host drops every tunnel through it (on Kubernetes the Service holds the port
across a rolling restart). Both relays drain at once if they are recreated
together, and on compose they are: `docker compose up -d` recreates every
replica of a scaled service within the same second, and
`COMPOSE_PARALLEL_LIMIT=1` does not change that (confirmed on the Hetzner
host 2026-09-05; it bounds concurrent engine calls, not replica order). A
true one-at-a-time relay roll on compose is #535; on Kubernetes or ECS the
orchestrator's rolling update already provides it.

## Single host with compose

The generic compose above is written for bridge networking. A relay that owns a
public IP — the hosted relay's Hetzner layout, with certbot on the host and a
colocated `piperd` — wants four things done differently. The ready-made file is
[`deploy/compose/relay/docker-compose.yml`](../../deploy/compose/relay/docker-compose.yml);
this section explains it and walks the cutover from the systemd unit.

**What differs from the generic example, and why**

1. **`piper-edge` on the host network, relays on the bridge.** The edge owns
   `:443`/`:80`/`:7000` on the host network so it sees real client IPs, and
   passes them to the relays in a PROXY v2 header; the relays run with
   `PIPER_RELAY_PROXY_PROTOCOL=1` on the bridge network with no published
   ports and scale with `--scale relay=N`. Two relay values from
   `/etc/piper-relay.env` are overridden in the compose file because they
   only fit the old host-networked layout: `PIPER_RELAY_DB_URL` (relays reach
   Postgres by service name, the host-networked edge by `127.0.0.1:5432`,
   which is why Postgres publishes that loopback port — also what `psql` and
   the nightly dump use) and `PIPER_RELAY_API_ADDR` (`:8080`, so the control
   hop between relays can reach it). The host's `127.0.0.1:9090`, where
   `tailscale serve` already fronts an ops endpoint, is now the **edge's**;
   each relay's is on its container IP. A colocated `piperd` keeps working
   unchanged: it dials the public tunnel address, which the edge answers.
2. **Mount `/etc/letsencrypt` whole.** certbot's `live/<domain>/` entries are
   symlinks into `../../archive/`; mounting only `live/` leaves them dangling
   inside the container. Point `PIPER_RELAY_TLS_CERT`/`KEY` at the `live/`
   paths. The relay re-reads the pair on every handshake, so the certbot
   deploy hook that restarted the systemd unit is no longer needed — remove
   it (or make it `docker compose restart relay`); do not leave it aimed at a
   unit that no longer exists.
3. **The GitHub App key is a plain read-only bind mount**, not a systemd
   `LoadCredential`. Keep it `0600` root-owned — the relay only refuses
   world-readable bits, and distroless runs as root — and set
   `PIPER_RELAY_GITHUB_APP_KEY` to the in-container path.
4. **Postgres durability is yours.** Pin `postgres:17` (a major bump does not
   upgrade the data directory by itself) and add a nightly dump the box's
   existing backups cover:

   ```sh
   # /etc/cron.d/piper-relay-pgdump
   15 3 * * * root docker compose -f /opt/piper-relay/docker-compose.yml exec -T postgres \
     pg_dump -U piper_relay piper_relay | gzip > /var/backups/piper-relay-$(date +\%F).sql.gz
   ```

`PIPER_RELAY_DATA_DIR` needs no mount: the image declares it as a volume and
the relay only creates it. Everything it used to hold is addressed by the env
vars above.

**Cutover from the systemd unit**

Relay before agents, as always; nothing on the boxes changes.

1. Install the files:

   ```sh
   sudo mkdir -p /opt/piper-relay
   sudo cp deploy/compose/relay/docker-compose.yml /opt/piper-relay/
   sudo cp deploy/compose/relay/.env.example /opt/piper-relay/.env   # then edit all three values
   sudo chmod 600 /opt/piper-relay/.env
   ```

   In `/etc/piper-relay.env`, add
   `PIPER_RELAY_DB_URL=postgres://piper_relay:<password>@127.0.0.1:5432/piper_relay`
   and repoint `PIPER_RELAY_TLS_CERT`, `PIPER_RELAY_TLS_KEY`, and
   `PIPER_RELAY_GITHUB_APP_KEY` from their `/run/credentials/…` paths to
   `/etc/letsencrypt/live/<domain>/fullchain.pem`, `…/privkey.pem`, and
   `/etc/piper-relay/github-app.pem`.
2. Start only Postgres and wait for it to report healthy:

   ```sh
   cd /opt/piper-relay && sudo docker compose up -d postgres && sudo docker compose ps
   ```
3. Bring the data across, if there is any to bring. A relay already on a host
   Postgres moves with `pg_dump | psql` into the container's database. A relay
   still on SQLite (v0.18.0 or older) starts empty: the one-off `relay.db`
   copier existed only for the hosted relay's own cutover and was deleted
   afterwards (#516), so per the pre-1.x policy its agents re-enroll.
4. Stop and disable the unit:

   ```sh
   sudo systemctl disable --now piper-relay
   ```

   Keep the old binary — and `relay.db`, if it was still on SQLite — as the
   rollback pair; the old binary needs its old store.
5. Start the edge and the relays and verify:

   ```sh
   sudo docker compose up -d --scale relay=2 && sudo docker compose logs -f
   ```

   Expect the usual startup lines, every agent re-registering within a few
   seconds (if their tokens came across), `/v1/github/status` still showing the
   linked App, and the app URLs serving. Check `ss -ltnp` shows `piper-edge`
   holding `:443`, `:80`, `:7000`, and `127.0.0.1:9090`, and that
   `relay_instances` has one live row per replica.
6. Remove the certbot deploy hook that restarted `piper-relay.service`.

**Day-to-day afterwards**

- `enroll` / `admin`: `sudo docker compose exec relay piper-relay admin …`.
- Upgrade: set `PIPER_RELAY_VERSION` and `PIPER_EDGE_VERSION` in `.env`, then
  `sudo docker compose pull`, then relays before the edge:
  `sudo docker compose up -d --no-deps --scale relay=2 relay`, check the
  pool rows and owners are back, then `sudo docker compose up -d --no-deps edge`.
  Compose recreates both relay replicas together (#535), so every agent
  redials once; each relay drains (#523) so requests in flight finish first.
  Restarting the edge drops every tunnel until it is back. For a
  schema-change release, drop the named tables first with
  `sudo docker compose exec postgres psql -U piper_relay piper_relay`,
  immediately before the relay `up` so the new binaries re-create them
  within seconds.
- Logs and metrics: `127.0.0.1:9090` (and the `tailscale serve` in front of
  it) is the edge's ops endpoint. A relay's is on its container IP:
  `curl http://$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' piper-relay-relay-1):9090/logs`.
- Scaling out is a separate step ([Scale out](#scale-out)); the file as
  shipped is still one relay.
