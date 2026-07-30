# Getting started

The full journey, in order: install → drive `piperd` (CLI or the interactive
TUI) → join the public relay → drive a box remotely → git deploys. Each section
builds on the previous one, but you can stop wherever your setup is complete —
a LAN-only box never needs the relay sections.

## Install

### Linux (Debian-family, e.g. Raspberry Pi OS)

```bash
sudo install -d -m 0755 /etc/apt/keyrings
sudo curl -fsSL https://apt.piperbox.dev/piperbox.gpg -o /etc/apt/keyrings/piperbox.gpg
sudo curl -fsSL https://apt.piperbox.dev/piperbox.sources -o /etc/apt/sources.list.d/piperbox.sources
sudo apt update && sudo apt install piperd piper
```

`apt install piperd piper` already installs, enables, and starts the systemd
service — it's durable from install, no separate step needed. `piper agent
up`, `piper agent down`, and `piper agent status` drive that one system
service from then on:

```bash
piper agent up             # start it (self-sudo when needed)
piper agent status         # running / stopped / not installed; when running,
                           # prints the control-API address and data dir
piper agent down           # stop it
```

`up`/`down` need root: `piper` re-runs itself under `sudo` and prompts for
your password (running it as real root works too). `status` needs none. State
lives at `PIPER_DATA_DIR=/var/lib/piper`, and it binds `:80`/`:443` via
`CAP_NET_BIND_SERVICE` — no root needed once running.

Apps are served at `http://<name>.piper.localhost`. Your user must be able to
reach a Docker socket — be in the `docker` group, or set `DOCKER_HOST`.

Not on a Debian-family distro, want just the CLI (e.g. to drive a box from
your laptop), or building from source? The installer places binaries only, no
service management:

```bash
curl -fsSL https://raw.githubusercontent.com/piperbox/piper/main/install.sh | sh
```

It detects your OS/arch, downloads the matching release binaries, verifies
their `checksums.txt`, and installs `piper` + `piperd` to `~/.local/bin` (or
`/usr/local/bin` when run as root; `PIPER_PREFIX` overrides). It never runs
`systemctl`, never touches `/etc`, and never prompts for `sudo`. Re-run any
time to upgrade. Add `--rc` to install the latest release candidate instead of
the latest stable release, or `--cli-only` for just `piper`. Then install the
systemd unit by hand: see [`manual-setup.md`](manual-setup.md).

### macOS (dev boxes)

`piper agent up`/`down`/`status` wrap `brew services` — install via
`brew install piperbox/tap/piper`, then `brew services start piper` (or
`piper agent up`, equivalent). State lives at `~/.piper/piperd`, and apps are
served at `http://<name>.piper.localhost`. This path is LAN-only; the
relay/public-URL flow is Linux/Pi (systemd) only. See
[`manual-setup.md`](manual-setup.md#run-the-agent-on-macos-dev-box).

### Upgrading from a pre-0.15 install

The rootless tier and CLI-generated LaunchAgent are gone. One-time cleanup:

- Linux rootless: `systemctl --user disable --now piperd` and delete
  `~/.config/systemd/user/piperd.service`, then install via apt.
- macOS: `launchctl bootout gui/$(id -u)/com.piperbox.piperd` and delete
  `~/.piper/com.piperbox.piperd.plist`, then `brew install piperbox/tap/piper
  && brew services start piper`. Data in `~/.piper/piperd` is picked up as-is.

Shell completions are a planned follow-up.

Prefer to build from source, run piperd in Docker via Compose, run the relay as
a service, or wire your own automation? See [`manual-setup.md`](manual-setup.md).

## The interactive TUI

`piper` is dual-mode. Every subcommand stays scriptable and byte-for-byte
unchanged, but bare `piper` in a terminal opens a full-screen TUI — a complete
control surface, not just a dashboard:

```bash
piper            # opens the TUI against the current box
```

- **Apps table** (home) — NAME · STATUS · URL · LAST DEPLOY, refreshed every 2s.
- **Drill down** — `↵` opens an app's detail, deployments, and logs (with live
  follow).
- **Actions** — deploy, new app, stop, delete, right from the TUI.
- **Boxes** — `t` opens a box switcher and config editor to add/edit/remove
  targets.
- **Wizards** — login, `piper connect`, GitHub App setup, and repo linking run
  interactively.

Keys: `↵` open · `esc` back · `t` boxes · `q` quit. Every screen lists its own
keys in a dim legend along the bottom.
Run it on the box and it's authless (see below); point it at a remote box with
`piper --remote <base-domain>`. Non-TTY invocation (scripts, pipes) is
untouched — bare `piper` with no terminal still prints usage and exits 2.

## Drive piperd from another machine on the LAN

**On the box itself, the CLI needs no login**: the control API binds to
loopback (`127.0.0.1:8088`) by default and serves it tokenless — being able to
run `piper` on the box is itself the proof you own it. `piper list`, `piper
deploy`, etc. just work.

Once the API leaves loopback it requires a bearer token, so mint one on the box
and log the CLI in first. Running `piperd token create` on the box needs no
auth either; on a systemd install it needs `sudo` to reach the service's data
dir and will say so if you forget.

To reach the control API from another machine on the LAN set
`PIPER_API_ADDR=0.0.0.0:8088` on the box — uncomment it in
`/etc/piper/piperd.env` and restart:

```bash
# on the box:
echo 'PIPER_API_ADDR=0.0.0.0:8088' | sudo tee -a /etc/piper/piperd.env
sudo systemctl restart piperd
sudo piperd token create --name laptop         # prints a token once
# on the client — address the box by its IP; mDNS *.local names often
# don't resolve on home LANs (run `hostname -I` on the box to find it):
piper login --token <token> --addr http://192.168.1.50:8088
piper list                                     # now authenticated
```

`piper login` verifies the token against the box and saves it (with the
address) to `~/.piper/piper/config.json`, mode `0600`; `PIPER_TOKEN` /
`PIPER_ADDR` override the saved values per command. Manage tokens on the box
with `sudo piperd token list` and `sudo piperd token revoke <name>`.

## Join the public relay (self-service)

On a box running `piperd`, log in and claim the box as your normal user:

```bash
piper login          # opens a GitHub device-flow login; stores your account credential
piper connect        # enrolls this box on the relay
```

Where `piper connect` installs the enrollment depends on the install:

- **Manual / dev** (piperd reads `~/.piper/piperd`): `connect` writes
  `relay.json` there directly, then just `sudo systemctl restart piperd`.
- **Daemonized systemd service** (piperd runs as a `DynamicUser`, state under
  `/var/lib/piper`): that directory isn't writable by your login user, so
  `connect` instead prints a ready `sudo sh -c … /etc/piper/piperd.env` command
  that stores the enrollment in piperd's root-owned EnvironmentFile (systemd
  injects it into the service at start, so its `DynamicUser` never needs to read
  it). Run it, then `sudo systemctl restart piperd`.

Run `connect` **on the box**: on a machine with no piperd install (no systemd
install, rootless user unit, launchd agent, or existing data dir) it errors out
instead of writing a `relay.json` nothing would read.

Either way piperd picks up the enrollment at startup and dials the tunnel.

`piper connect` claims the box in **terminated** mode: piperd holds no cert and
serves apps on `:80`; the relay assigns each app a single-label hostname
`<app-hash>-<username>.public.getpiper.dev`, terminates its HTTPS with its
wildcard cert, and forwards plaintext HTTP over the tunnel.

```bash
piper login                  # GitHub device-flow; stores your account credential
piper connect                # claims this box (terminated) and writes relay.json
sudo systemctl restart piperd
piper deploy blog --path .   # → https://<hash>-<you>.public.getpiper.dev
```

`piper login --relay <url>` targets a self-hosted relay instead of the default
`https://api.public.getpiper.dev`. Environment variables (`PIPER_RELAY_ADDR`,
`PIPER_RELAY_TOKEN`, `PIPER_BASE_DOMAIN`) still override `relay.json`.

Bring-your-own-domain apps stay **end-to-end** (the box terminates TLS; the relay
only splices SNI) — set `PIPER_BASE_DOMAIN` + cert/DNS config instead of using
`piper connect`; see [`custom-domains.md`](custom-domains.md). Self-hosters run
the relay passthrough-only by leaving `PIPER_RELAY_TLS_CERT`/`KEY` unset.

### List and remove boxes

```bash
piper box ls                                          # base domain, owner, connected
piper box rm ab12-alice.public.getpiper.dev --yes      # frees the box slot
```

Removal frees the box slot for a fresh `piper connect`; a connected box must be
stopped first (the relay refuses with a conflict otherwise). The box's
relay-assigned `<hash>-<user>.<apex>` app URL stays reserved on the account,
but any custom domains it held are released and can be re-claimed elsewhere.

## Drive a box remotely

Any control command (`create`, `deploy`, `list`, `status`, `app link`,
`github setup`) can target one of your relay-connected boxes from anywhere, by
the base domain `piper connect` printed:

```bash
piper --remote ab12-alice.public.getpiper.dev list
piper --remote ab12-alice.public.getpiper.dev status  # box up? what's deployed?
export PIPER_REMOTE=ab12-alice.public.getpiper.dev    # or set it once
piper deploy blog --path .
```

Requests travel relay → tunnel → box: the CLI authenticates to the relay with
the account credential `piper login` saved in `~/.piper/piper/config.json`
(mode `0600`), and the relay swaps it for the box's own token — your relay
credential never reaches the box, and the box still enforces its own auth.
The `--remote` flag overrides `PIPER_REMOTE`; `login` and `connect` are
inherently local and reject `--remote`.

## Point your own domain at an app

A relay-connected box can serve one specific app on a domain you own — no
DNS-provider API token, just a CNAME:

```bash
piper domains add myshop.com --app shop   # prints the CNAME record to create
# create it at your DNS host, then watch it go active:
piper domains list                        # myshop.com  app=shop  status=active  dns=ok
```

The cert issues through the relay tunnel (ACME TLS-ALPN-01) once the name
resolves to the relay; the box terminates TLS itself, and the app's
shared-domain URL keeps working alongside. Apex-domain caveats and the API
shape: [`custom-domains.md`](custom-domains.md).

## Git deploys

Once you've joined the relay (above), a `git push` can build and publish an app.
The **public hosted relay holds one shared GitHub App** on everyone's behalf, so
there's nothing to create yourself — `piper login` walks you through installing
it on the repos you want:

```bash
piper login                                          # ... then: install the App (link printed); login waits for it
piper create myapp --port 8080                       # register the app (needed before it can be linked)
piper app link myapp --repo owner/name --branch main # bind the repo to an app
git push origin main                                 # → live at the app's routed URL
```

Every push to the tracked branch builds the Dockerfile at the repo root,
health-checks the container, and serves it. The live URL shows up on GitHub as
a Deployment status. `piper github repos` lists what the installation can reach
at any point; re-run `piper login` to install the App on more repos later.

A box that ever ran `piper github setup` keeps its own App, and that always
wins over the relay's — so brokered deliveries fail their signature check until
you give it up:

```bash
piper github reset                                   # drop this box's own App
sudo systemctl restart piperd                        # the provider is picked at start
```

### Self-hosted relay / bring-your-own GitHub App

Running your own `piper-relay` without a configured App, or serving on your own
domain outside the public relay? Each box then creates and holds its **own**
GitHub App instead — the private key and webhook secret never leave it:

```bash
piper create myapp --port 8080                       # register the app (needed before it can be linked)
piper github setup [--org name]                      # create the GitHub App (one-time; use --org for org-owned apps)
# install the App on your repo in GitHub, then:
piper app link myapp --repo owner/name --branch main # bind the repo to an app
```

After that, every push to the tracked branch builds the Dockerfile at the repo
root, health-checks the container, and serves it at `https://myapp.<your-domain>`.
The live URL shows up on GitHub as a Deployment status. Webhooks ride the same
tunnel as your traffic (delivered to `hooks.<your-domain>`); nothing else on the
box is exposed.

Standing either path up against a real relay, domain, and GitHub App end-to-end:
[`runbooks/git-deploy-e2e.md`](runbooks/git-deploy-e2e.md) (BYO in Parts A–F,
brokered mode in Part G).
