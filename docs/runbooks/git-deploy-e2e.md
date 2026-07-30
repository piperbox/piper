# Manual E2E Runbook — `git push → live HTTPS URL`

This walks a human operator through the **entire** Piper stack end-to-end against
real infrastructure: a public relay, on-box TLS, a per-user GitHub App, and a real
`git push` that builds and publishes an app. It exercises what the automated tests
can't — real GitHub credentials, a real domain, a real tunnel, and a browser-trusted
cert.

If you only want to smoke-test the tunnel/TLS plumbing without GitHub or a public
box, jump to [Appendix A — local loopback smoke test](#appendix-a--local-loopback-smoke-test).

---

## What this proves

```
 git push
    │
    ▼
 GitHub ──webhook──▶ hooks.<base> ─┐
    ▲                              │  (public :443, SNI passthrough — relay never decrypts)
    │  Deployment status           ▼
    │                          ┌────────┐        ┌──────────────── your box ───────────────┐
    └──────────────────────────│ relay  │◀──tunnel(:7000)── piperd ─▶ Caddy :443 (TLS here) │
                               └────────┘                     │        │                     │
   browser ─▶ myapp.<base> :443 ─(SNI)─▶ relay ─▶ tunnel ─────┘        └▶ Docker container   │
                                                                       └──────────────────────┘
```

A push to a linked repo's tracked branch → GitHub webhook rides the tunnel to
`hooks.<base>` → piperd fetches the repo tarball, builds the Dockerfile, runs +
health-checks the container, routes it → the live URL `https://<app>.<base>` is
posted back to GitHub as a Deployment status.

---

## Prerequisites

**Roles** (can be three machines, or your laptop + one VPS):

| Role | What it needs |
| --- | --- |
| **Relay** | A host with a **public IP**, inbound `:443` and `:7000` open. A cheap VPS is fine. |
| **Box** (`piperd`) | Docker running. A Pi, a laptop, anything. Does **not** need a public IP. (Caddy is embedded in `piperd` — nothing to install.) |
| **Operator** | The `piper` CLI + a browser (for the GitHub App approval redirect). Usually the box itself. |

**Accounts / assets:**

- A **domain you control**, used as `<base>` (e.g. `alice.dev`). All apps live at
  `*.<base>`.
- **DNS you can point** `*.<base>` and `<base>` at the relay's public IP.
- For the wildcard cert: **DNS-01 API credentials** for that domain (this runbook
  uses Cloudflare — the only wired provider), **or** a browser-trusted wildcard cert
  you already own (BYO).
- A **GitHub repo** with a `Dockerfile` at its root, and the app inside it listening
  on a known container port (default `8080`).

> **Cert must be publicly trusted.** GitHub will refuse to deliver webhooks to a host
> with an untrusted cert, so this full path requires **Let's Encrypt production**
> (or a real CA) — not staging, not self-signed. Mind LE rate limits while iterating.

**Build the binaries** (on both relay and box, or cross-compile for the Pi):

```bash
make build          # → bin/piperd, bin/piper, bin/piper-relay
# Pi target: CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o bin/piperd ./cmd/piperd  (+ piper-relay)
```

---

## Part A — DNS

Point both the wildcard and the apex at the **relay's public IP**. The wildcard
covers every app host *and* `hooks.<base>`; the apex is where the cert's SAN sits.

```
*.<base>   A   <relay-public-ip>
<base>     A   <relay-public-ip>
```

Verify before continuing:

```bash
dig +short myapp.<base>      # → <relay-public-ip>
dig +short hooks.<base>      # → <relay-public-ip>
```

> DNS-01 issuance validates via a `_acme-challenge.<base>` TXT record that lego
> writes and deletes through your provider API — that's separate from the A records
> above and needs the API token from Part C.

---

## Part B — Relay

On the relay host, install the binary and service unit:

```bash
sudo install -m 0755 bin/piper-relay /usr/local/bin/piper-relay
sudo install -m 0644 packaging/systemd/piper-relay.service \
  /etc/systemd/system/piper-relay.service
sudo systemctl daemon-reload
```

Enrollment is a separate one-shot command, not the service. Run it through a
transient unit so it writes to the same systemd-managed state directory as the
service:

```bash
sudo systemd-run --pipe --wait --collect \
  --property=DynamicUser=yes \
  --property=StateDirectory=piper-relay \
  --setenv=PIPER_RELAY_DATA_DIR=/var/lib/piper-relay \
  /usr/local/bin/piper-relay enroll alice --domain <base>
#   enrolled alice for <base>
#   token: rlyt_XXXXXXXXXXXXXXXX      ← copy this
```

Do not run enrollment directly as root with
`PIPER_RELAY_DATA_DIR=/var/lib/piper-relay`; a root-owned `relay.db` may prevent the
dynamic service user from opening it.

Enable the relay at boot and start it now:

```bash
sudo systemctl enable --now piper-relay
sudo systemctl status piper-relay
sudo journalctl -u piper-relay -n 50 --no-pager
sudo ss -lnt '( sport = :443 or sport = :7000 )'
```

The final command must show listeners on `:443` and `:7000`. Open inbound TCP ports
`443` and `7000` in both the host firewall and the VPS provider firewall.

To override listener addresses, create `/etc/piper-relay.env` before starting the
service:

```bash
PIPER_RELAY_TLS_ADDR=:443
PIPER_RELAY_TUNNEL_ADDR=:7000
```

Then apply changes with `sudo systemctl restart piper-relay`. Keep the enrollment
token — it goes to the box next.

---

## Part C — Box (`piperd` in relay mode)

`piperd` enters relay mode the moment `PIPER_RELAY_ADDR` is set: it obtains the
wildcard cert, loads it into Caddy on `:443`, dials the relay tunnel, and (once a
GitHub App exists) serves webhooks at `hooks.<base>`.

Pick **one** TLS path. The env vars below can be exported for a foreground run
(handy while walking this runbook, since logs stream to your terminal) or, for a
box that should survive reboots, dropped into `/etc/piper/piperd.env` and started
via the systemd unit (see [Run as a service](#run-piperd-as-a-service) below).

### Option 1 — ACME DNS-01 (Cloudflare), the real path

```bash
export PIPER_BASE_DOMAIN=<base>              # MUST equal the enrolled domain
export PIPER_RELAY_ADDR=<relay-public-ip>:7000
export PIPER_RELAY_TOKEN=rlyt_XXXXXXXXXXXXXXXX
export PIPER_ACME_EMAIL=you@example.com
export PIPER_DNS_PROVIDER=cloudflare
export CLOUDFLARE_DNS_API_TOKEN=<token-with-dns-edit-on-the-zone>
# export PIPER_ACME_CA=https://acme-staging-v02.api.letsencrypt.org/directory  # plumbing-only; GitHub rejects staging certs
export PIPER_DATA_DIR=$HOME/.piper

./bin/piperd
#   piperd listening on 127.0.0.1:8088 (apps at *.<base>)
#   no GitHub App configured; run `piper github setup` to enable git deploys
```

### Option 2 — BYO cert (you already hold a trusted wildcard)

```bash
export PIPER_BASE_DOMAIN=<base>
export PIPER_RELAY_ADDR=<relay-public-ip>:7000
export PIPER_RELAY_TOKEN=rlyt_XXXXXXXXXXXXXXXX
export PIPER_TLS_CERT_FILE=/path/fullchain.pem   # must cover *.<base> AND <base>
export PIPER_TLS_KEY_FILE=/path/privkey.pem
export PIPER_DATA_DIR=$HOME/.piper

./bin/piperd
```

### Run piperd as a service

For anything past a one-off test, run `piperd` under systemd so it comes back on
boot and restarts on failure. Put the TLS/relay env from the option above into
`/etc/piper/piperd.env` (mode `0600`) instead of exporting it, then install the
unit and start it with `piper agent up`. State lives at
`PIPER_DATA_DIR=/var/lib/piper` (set by the unit, not `$HOME`); an
`apt install piperd piper` already does all of this (see the README).

```bash
sudo install -m 0755 bin/piperd /usr/local/bin/piperd   # from-source builds; the
sudo install -m 0755 bin/piper  /usr/local/bin/piper    # curl installer places both
sudo install -d -m 0700 /etc/piper
sudo install -m 0600 packaging/systemd/piperd.env.example /etc/piper/piperd.env
# edit /etc/piper/piperd.env — add PIPER_RELAY_ADDR, PIPER_ACME_EMAIL, etc.
sudo install -m 0644 packaging/systemd/piperd.service /etc/systemd/system/piperd.service
sudo systemctl daemon-reload
piper agent up                # starts the system service (self-sudo when needed)
piper agent status
sudo journalctl -u piperd -n 50 --no-pager
```

The unit runs as a `DynamicUser` in the `docker` group and binds `:80`/`:443` via
`CAP_NET_BIND_SERVICE` — no root. Apply later env edits with
`sudo systemctl restart piperd`.

To join the public relay instead of setting `PIPER_RELAY_ADDR`/`PIPER_RELAY_TOKEN`/`PIPER_BASE_DOMAIN`
by hand, run `piper login && piper connect` — on this systemd install `connect`
prints a ready `sudo sh -c … /etc/piper/piperd.env` command that upserts those
three keys for you (see the README's "Join the public relay").

**Health checks before moving on:**

- piperd logs `piperd listening …` and does **not** exit on `relay tls:` — a cert
  error here is the #1 blocker (see Troubleshooting).
- Relay logs show a tunnel client connecting.
- Docker is reachable (`docker ps` works as the piperd user) and `caddy version`
  resolves on `PATH`. If you run your own Caddy, set `PIPER_SKIP_CADDY=1` and route
  `:80`/`:443` yourself.

---

## Part D — Sanity: deploy one app by hand (proves Plan 1 + 2)

Before involving GitHub, prove the tunnel + TLS + deploy path with a manual deploy.
The repo ships a trivial `:8080` sample at `test/e2e/sampleapp` — use it, or any
local directory with a root `Dockerfile` whose app listens on the port you pass.

```bash
export PIPER_ADDR=http://127.0.0.1:8088          # only if piper runs off-box

./bin/piper create myapp --port 8080
./bin/piper deploy myapp --path ./test/e2e/sampleapp
#   deployed myapp: http://myapp.piper.localhost (running)   ← LAN URL in output

./bin/piper list
```

Now hit it **publicly through the relay** — this is the real proof:

```bash
curl -sS https://myapp.<base>/         # 200 from your container, TLS from your box
```

If that returns your app over HTTPS, the entire non-GitHub spine works. If it
doesn't, fix it here before adding GitHub — the git path rides these same rails.

---

## Part E — Create & install the GitHub App (one-time)

Run this **as the operator, with a browser available** (typically on the box, or
tunnel `127.0.0.1:8088` to your laptop). It asks piperd for a GitHub App manifest,
opens a browser to create the App under *your* account (or under an organization if
`--org <name>` is passed), catches the redirect, and
stores the App ID + private key + webhook secret **on the box** (they never leave it).

```bash
./bin/piper github setup [--org <name>]
#   Opening http://127.0.0.1:xxxxx — approve the App in your browser...
#   (browser: GitHub "Create App" screen → Create; it redirects back)
#   GitHub App configured. Install it on your repo, then run: piper app link <name> --repo owner/name
```

The App is named `piper-<base>`, subscribes to **push + pull_request**, points its
webhook at **`https://hooks.<base>`**, and requests `contents:read`,
`deployments:write`, `pull_requests:read`.

Then, in the GitHub UI: **Install the App** on the target repo (App settings →
Install App → pick the repo). piperd starts serving `hooks.<base>` as soon as
`github setup` completes — no restart needed.

**Confirm webhook delivery is reachable:** in GitHub → the App → *Advanced* →
*Recent Deliveries*, the initial `ping` should show a `2xx`. A red delivery here
means DNS/cert/tunnel for `hooks.<base>` isn't right — fix before pushing.

---

## Part F — Link a repo and push (proves Plan 3)

```bash
./bin/piper app link myapp --repo owner/name --branch main
#   linked myapp -> owner/name (main)
```

Now the payoff:

```bash
# in a clone of owner/name, on the tracked branch:
git commit --allow-empty -m "trigger piper deploy"
git push origin main
```

**Watch it happen:**

- **piperd logs:** webhook received → build → run → health → route.
- **GitHub → the repo → Deployments** (or the commit's status): a `production`
  deployment goes **pending → success**, and `success` carries the
  **Environment URL** `https://myapp.<base>`.
- **The live app:**

  ```bash
  curl -sS https://myapp.<base>/        # serves the just-pushed commit
  ```

That's the full loop: `git push` → live HTTPS URL, status reported to GitHub. ✅

To confirm it's really redeploying, change something visible in the app, push again,
and re-`curl` — the response should reflect the new commit, and a second Deployment
should appear.

---

## Part G — Brokered mode: the relay holds the GitHub App

Everything above (Parts A–F) is **bring-your-own (BYO)**: each box creates and holds
its own GitHub App. **Brokered mode** is the alternative — the relay operator
registers *one* App under the `piperbox` org and holds its key; every account's
`piper login` installs that shared App and the relay re-signs and forwards webhooks
to the right box over the tunnel. This is what the public hosted relay runs, and
it's the default flow in [`getting-started.md`](../getting-started.md).

**Prerequisites differ from BYO in exactly these ways:**

- **No `hooks.<base>` DNS record and no publicly trusted certificate are
  required.** GitHub delivers webhooks to the relay's account-API host, not your
  box, so the "cert must be publicly trusted" constraint from Part C/E above
  doesn't apply to you at all. Join with `piper connect` (terminated mode) and
  skip Parts A and C entirely — no domain, no DNS, no cert of your own.
- **No `piper github setup` step.** The App already exists on the relay; there's
  nothing to create or store on the box.
- **The relay must run with the App's credentials set:**
  `PIPER_RELAY_GITHUB_APP_ID`, `PIPER_RELAY_GITHUB_APP_KEY`,
  `PIPER_RELAY_GITHUB_WEBHOOK_SECRET`, `PIPER_RELAY_GITHUB_APP_SLUG`,
  `PIPER_RELAY_GITHUB_CLIENT_ID`, `PIPER_RELAY_GITHUB_CLIENT_SECRET` (extending
  Part B below). The App's webhook URL is the relay's account-API host,
  **`https://api.<apex>/gh`** — `cmd/piper-relay/main.go` mounts `/gh` there
  alongside the control API.

### Registering the App (once per relay)

**Settings → Developer settings → GitHub Apps → New GitHub App.** Two ordering
rules first: deploy a relay that actually serves `/gh` *before* anyone installs
the App (an install landing on an older relay 404s, GitHub retries a few times
and gives up, and that installation stays unlinked); and set permissions before
events, because GitHub only offers the event checkboxes your permissions justify.

Generate the webhook secret up front with `openssl rand -hex 32`.

| Field | Value |
| --- | --- |
| GitHub App name | must be globally unique across GitHub; the slug derives from it |
| Callback URL | `https://api.<apex>/v1/login/callback` |
| Webhook → Active | checked |
| Webhook URL | `https://api.<apex>/gh` |
| Webhook secret | the `openssl` value |

**Repository permissions:** Contents *Read-only*, Deployments *Read and write*,
Pull requests *Read-only*. **Events:** `Push` and `Pull request` — there is
normally no *Installation* checkbox, because GitHub delivers installation
lifecycle events to Apps automatically.

Three toggles decide whether this works at all, and none of them announce
themselves when wrong:

- **Enable Device Flow — ON.** Device flow is the CLI's only login path, and
  GitHub rejects it outright unless this is checked. Without it `piper login`
  cannot work at any point.
- **Request user authorization (OAuth) during installation — OFF.** Login
  never rides an install: the CLI uses the device flow and the browser flow
  uses GitHub's OAuth authorize endpoint (#305), and installations are linked
  by the `installation` webhook in both cases. Leaving it ON only makes GitHub
  bounce the browser to the OAuth callback after each install with a `code`
  but no `state`, which the relay rejects as a stray login ("bad state").
- **Expire user authorization tokens — ON.** Safe here because the relay uses
  the user token once, for a single `GET /user` inside `fetchUser`, and never
  stores it: `Identity` carries only the subject and login, and the extra
  `refresh_token`/`expires_in` fields are ignored by the decoder. Everything
  after login runs on *installation* tokens minted from the App key, so nothing
  ever needs refreshing. Leaving it off just means a leaked user token lives
  until manually revoked.

After creating it, collect what the form doesn't give you: **Generate a private
key** (downloads a PKCS#1 `.pem`) and **Generate a client secret** (shown once).
Read the slug off the App's own URL (`github.com/apps/<slug>`) rather than
assuming it. Note the App's client ID starts `Iv23li…` — a standalone OAuth
app's `Ov23li…` id is a different credential and will not work.

### Relay: add the App credentials to Part B

Before `sudo systemctl enable --now piper-relay`, drop the App's credentials into
`/etc/piper-relay.env` alongside any TLS/listener overrides:

```bash
PIPER_RELAY_GITHUB_APP_ID=123456
PIPER_RELAY_GITHUB_APP_KEY=/run/credentials/piper-relay.service/github-app.pem
PIPER_RELAY_GITHUB_WEBHOOK_SECRET=<whsec>
PIPER_RELAY_GITHUB_APP_SLUG=piper-bot                        # → InstallURL
PIPER_RELAY_GITHUB_CLIENT_ID=<the App's Iv23li... client id>
PIPER_RELAY_GITHUB_CLIENT_SECRET=<the App's client secret>
```

**The key reaches the relay as a systemd credential, not as a path in `/etc`.**
`piper-relay.service` runs `DynamicUser=yes`, so the service user cannot read a
root-owned `0600` file — and making it world-readable is a startup failure, not
a workaround. Hand it over the same way the TLS cert already arrives:

```bash
sudo install -d -m 0700 /etc/piper-relay
sudo install -m 0600 -o root -g root github-app.pem /etc/piper-relay/github-app.pem

sudo tee /etc/systemd/system/piper-relay.service.d/github-app.conf >/dev/null <<'EOF'
[Service]
LoadCredential=github-app.pem:/etc/piper-relay/github-app.pem
EOF
sudo systemctl daemon-reload
```

systemd stages that at `/run/credentials/piper-relay.service/github-app.pem`,
mode `0440` inside a per-unit tmpfs no other user can traverse — which is why
the relay rejects only *world*-readable keys rather than any group-readable one.
Placing the key in the `StateDirectory` instead does not work: systemd only
re-chowns that directory when it has to fix the directory's own ownership, so a
root-created file inside it stays unreadable to the service.

The relay also refuses to start if App credentials are set without
`PIPER_RELAY_GITHUB_WEBHOOK_SECRET` — an empty secret would leave `/gh`
verifying against a forgeable empty HMAC key, so a half-configured App is a
startup failure rather than a silently open ingress.

`sudo systemctl restart piper-relay` and confirm the log line
`relay: GitHub App <id> configured (brokered git deploys enabled)`.

### Box: join with `piper login` + `piper connect`

No Part C TLS setup — the relay terminates HTTPS for you:

```bash
./bin/piper login
#   To log in, open: https://github.com/login/device ... enter the code: XXXX-XXXX
#   logged in to relay as alice
#   Install the Piper GitHub App on the repos you want to deploy:
#     https://github.com/apps/piper-bot/installations/new
#   Waiting…...Installed — 2 repo(s) available.

./bin/piper connect
#   box claimed: ab12-alice.public.getpiper.dev
sudo systemctl restart piperd   # or the relay.json restart hint connect prints
```

`piper login` prints the install URL from the login-poll response and blocks
until the App shows up installed — open the link (another tab, or another
device for a headless box) and it resolves on its own. `piper github repos`
lists what the installation can reach at any point afterward.

### Push (same as Part F, on the relay-assigned hostname)

```bash
./bin/piper create myapp --port 8080
./bin/piper app link myapp --repo owner/name --branch main
# in a clone of owner/name, on main:
git commit --allow-empty -m "trigger piper deploy"
git push origin main
curl -sS https://ab12-alice.public.getpiper.dev/     # or myapp's own routed host
```

### Loopback variant

Mirroring [Appendix A](#appendix-a--local-loopback-smoke-test), start `piper-relay`
with `PIPER_RELAY_FAKE_APPROVE=1` (`NewAutoApproveVerifier`,
`internal/relay/verifier.go:67`) so `piper login` completes without a real GitHub
OAuth round trip — proves the login → connect → deploy plumbing on one machine.
It still can't exercise real webhook delivery (no public host for GitHub to
reach, same limitation as Appendix A), so configure `ghApp` too only if you want
to confirm `piper github repos`/token brokering against a stubbed or real GitHub
API — not for a genuine end-to-end push.

---

## Teardown

```bash
# Box: stop piperd. Foreground run: Ctrl-C. Systemd service:
piper agent down                            # stop the system service
sudo systemctl clean --what=state piperd    # drops /var/lib/piper
sudo systemctl disable piperd               # disables the system unit
sudo rm /etc/systemd/system/piperd.service  # removes it (keeps piperd.env and the binaries)
sudo systemctl daemon-reload
# Piper images are tagged piper/<app>:<ts>; containers get auto-generated names,
# so clean up by image ancestor, per app:
docker rm -f $(docker ps -aq --filter ancestor=piper/myapp) 2>/dev/null
docker images --filter=reference='piper/*' -q | xargs -r docker rmi -f
rm -rf "$PIPER_DATA_DIR"          # foreground run only; drops apps, links, and stored GitHub App creds

# Relay: stop and disable the service; remove its persistent enrollment state.
sudo systemctl disable --now piper-relay
sudo systemctl clean --what=state piper-relay
# GitHub: uninstall / delete the piper-<base> App from your account settings.
# DNS: remove the A records if this was a throwaway.
```

---

## Troubleshooting

| Symptom | Likely cause | Fix |
| --- | --- | --- |
| piperd exits with `relay tls:` | DNS-01 failed (bad/missing `CLOUDFLARE_DNS_API_TOKEN`, token lacks DNS-edit on the zone), or LE rate limit | Verify the token edits the zone; watch for `_acme-challenge` TXT appearing; back off if rate-limited |
| `curl https://myapp.<base>` hangs / conn refused | Relay `:443`/`:7000` not publicly open, or tunnel not connected | Check both firewalls; run `systemctl status piper-relay`, inspect `journalctl -u piper-relay`, and confirm listeners with `ss -lnt`; confirm `PIPER_RELAY_ADDR` uses host:7000 |
| `curl` returns cert error | Wrong/missing wildcard, or staging/self-signed cert | Cert must cover `*.<base>` **and** `<base>` from a trusted CA |
| `create`/`deploy` fails with "name reserved" | You used `hooks` as an app name | `hooks` is reserved for the webhook host; pick another |
| GitHub `ping` delivery is red | `hooks.<base>` unreachable or untrusted cert | Same as the curl cert/tunnel checks — `hooks.<base>` rides the identical path |
| Push does nothing, no piperd log | App not installed on the repo, or repo not linked, or pushed a non-tracked branch | Install the App on the repo; `piper app link … --branch <pushed-branch>` |
| Deploy starts but health-check fails | App doesn't listen on the `--port` you set | Match `piper create --port N` to the container's listen port |
| Webhook 401 in piperd logs | Signature mismatch — stale App creds | Re-run `piper github setup`; ensure only one `piper-<base>` App is installed |
| Relay logs `box rejected delivery: 401` on a brokered box | The box still holds its own App, which outranks the relay's; the log shows `using this box's own GitHub App` instead of `(brokered)` | `piper github reset`, then restart piperd |

---

## macOS (dev box, via Homebrew)

On a Mac dev box the agent runs via `brew services` (see
[manual setup](../manual-setup.md#run-the-agent-on-macos-dev-box)):

```bash
piper agent status          # running / stopped
tail -f ~/.piper/piper.log  # agent logs (errors in ~/.piper/piper.err.log)
piper agent down            # stop it
```

---

## Appendix A — local loopback smoke test

No domain, no VPS, no GitHub — just proves the relay→tunnel→TLS→container plumbing
on one machine. This mirrors `test/e2e/relay_test.go`. It **cannot** test the git
path (GitHub can't reach a private box, nor accept a self-signed webhook cert).

Use unprivileged ports and a self-signed wildcard, and add hosts entries so the
names resolve to loopback:

```bash
base=alice.localhost
# self-signed *.$base cert → cert.pem / key.pem (openssl or the helper in relay_test.go)

# terminal 1 — relay on unprivileged ports
PIPER_RELAY_TLS_ADDR=:8443 PIPER_RELAY_TUNNEL_ADDR=:7000 \
  ./bin/piper-relay enroll alice --domain $base           # copy token
PIPER_RELAY_TLS_ADDR=:8443 PIPER_RELAY_TUNNEL_ADDR=:7000 ./bin/piper-relay

# terminal 2 — box
PIPER_BASE_DOMAIN=$base PIPER_RELAY_ADDR=127.0.0.1:7000 \
  PIPER_RELAY_TOKEN=<token> \
  PIPER_TLS_CERT_FILE=cert.pem PIPER_TLS_KEY_FILE=key.pem \
  PIPER_DATA_DIR=$(mktemp -d) ./bin/piperd

# terminal 3 — deploy + hit it through the relay (SNI must match the cert)
./bin/piper create myapp --port 8080
./bin/piper deploy myapp --path ./test/e2e/sampleapp
curl -k --resolve myapp.$base:8443:127.0.0.1 https://myapp.$base:8443/
```

A `200` here means the tunnel + SNI + on-box TLS + container path is sound. Graduate
to the full runbook above to add the public relay and the GitHub half.
