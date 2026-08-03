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
| `/var/lib/piper-relay/relay.db` | **all relay state** — agents, tokens, accounts, domains (SQLite) |
| `/etc/piper-relay.env` | optional env overrides, read by the unit (`EnvironmentFile=-`) |

The store applies `schema.sql` with `CREATE TABLE IF NOT EXISTS` and there are
**no migrations** (pre-1.x policy): a release that adds a *table* upgrades in
place; a release that adds a *column to an existing table* needs a fresh
`relay.db`. Each release's notes say which case it is — or check yourself:

```bash
git diff <old-tag>..<new-tag> -- internal/relay/schema.sql
```

New `CREATE TABLE` → in-place. Changed `CREATE TABLE` body → fresh DB.

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

Defaults work for a passthrough-only relay. Anything else goes in
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

Tunnels drop and agents reconnect on their own (piperd's tunnel client retries
in the background). Verify: `journalctl -u piper-relay -f` shows agents
re-registering within a minute, and `piper box ls` from an enrolled account
shows `connected`.

Keep the previous binary around (`/usr/local/bin/piper-relay.prev`) — rollback
is reinstall + restart.

---

## Upgrade across a schema change

Example: v0.16.0 added `agents.box_id` — a column on an existing table, so an
old `relay.db` will not gain it and the new binary's queries against it fail.

**Order matters: relay first, then agents.** The agent side (one-command login,
daemon-owned enrollment) expects the new relay's enroll semantics; upgrading
agents against an old relay leaves them creating duplicate rows instead of
upserting.

This resets **all** relay state — every agent row, token, account link, and
custom domain. Boxes re-enroll afterwards; that is the designed recovery path,
not an accident to engineer around.

```bash
sudo systemctl stop piper-relay
sudo cp /usr/local/bin/piper-relay /usr/local/bin/piper-relay.prev
# install the new binary as in the same-schema upgrade, then:
sudo mv /var/lib/piper-relay/relay.db "/var/lib/piper-relay/relay.db.pre-$(date +%Y%m%d)"
sudo systemctl start piper-relay
```

The fresh DB materializes on first start. Then, per box:

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
the moved `relay.db.pre-*`, start. Old tokens become valid again; boxes that
already re-enrolled against the new DB must `piper login --re-enroll` once
more.

---

## Ops surface

- Logs: `journalctl -u piper-relay -f`
- Admin CLI (runs against the same data dir): `piper-relay admin …` via the
  same transient-unit pattern as `enroll`
- Optional ops listener on `127.0.0.1:9090` (`PIPER_RELAY_OPS_ADDR`);
  `PIPER_RELAY_METRICS=1` / `PIPER_RELAY_LOGS=1` to enable metrics/log
  endpoints

---

## Run as a container

Every release also publishes a multi-arch (amd64/arm64) image to
`ghcr.io/piperbox/piper-relay` — version tags for every release including RCs,
`:latest` for finals only. It is the same binary on a distroless base; all the
env vars above apply unchanged.

```bash
docker run -d --name piper-relay --restart unless-stopped \
  -v piper-relay-data:/var/lib/piper-relay \
  --env-file piper-relay.env \
  -p 443:443 -p 80:80 -p 7000:7000 -p 8080:8080 \
  ghcr.io/piperbox/piper-relay:<version>
```

- **State** is the single SQLite file at `/var/lib/piper-relay/relay.db` — keep
  it on a volume (a PVC / EBS volume on K8s/ECS).
- **Certs and the GitHub App key** are file mounts; mount them into the data
  dir (or anywhere, with the env vars pointing at them). The App key must not
  be world-readable — the relay refuses it (mount `0600`).
- **Admin/enroll** run in the same container against the same DB:
  `docker exec piper-relay piper-relay admin …` (distroless has no shell; the
  binary is invoked directly).
- **Upgrade** = pull the new tag, recreate the container, same volume. The
  schema-change caveats above apply identically — a schema-change release
  still means moving `relay.db` aside and re-enrolling boxes.
- **Exactly one replica.** All state is one SQLite file and the tunnel router
  lives in process memory; two relay instances behind one address split-brain
  agents and routes. Scale up, never out.
- **Fronting:** custom-domain :443 is SNI passthrough (certs live on the
  boxes) and :7000 is a raw TCP protocol — only an L4/TCP ingress can sit in
  front, never an HTTP(S)-terminating one. Behind any L4 proxy the relay sees
  the proxy's IP ([#485](https://github.com/piperbox/piper/issues/485)); with
  a direct public IP (host networking) nothing is lost.
