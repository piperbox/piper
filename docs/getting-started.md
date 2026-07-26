# Getting started

The full journey, in order: install → drive `piperd` (CLI or the interactive
TUI) → join the public relay → drive a box remotely → git deploys. Each section
builds on the previous one, but you can stop wherever your setup is complete —
a LAN-only box never needs the relay sections.

## Install

One line puts the binaries on the box:

```bash
curl -fsSL https://raw.githubusercontent.com/piperbox/piper/main/install.sh | sh
```

The installer **only places binaries**: it detects your OS/arch (Linux and
macOS), downloads the matching release binaries, verifies their
`checksums.txt`, and installs `piper` + `piperd` to `~/.local/bin` (or
`/usr/local/bin` when run as root; `PIPER_PREFIX` overrides). It never runs
`systemctl`, never touches `/etc`, and never prompts for `sudo`. Re-run any
time to upgrade. Add `--rc` to install the latest release candidate instead of
the latest stable release, or `--cli-only` for just `piper` — for driving
`piperd` from another machine, e.g. your laptop and a Pi on the same LAN.

Running the agent is then a `piper` command, with two modes:
**`up` runs it until reboot; `daemonize` makes it permanent.**

### Rootless on Linux (dev boxes)

`piper agent up` gives you a rootless dev agent — piperd runs as **you** on
high ports (`:8080`/`:8443`) under `~/.piper`, as a systemd **user** unit that
`up` materializes itself, seeding `~/.piper/piperd.env` from files embedded in
the CLI — nothing to download, nothing to wire by hand.

```bash
piper agent up            # start it (no sudo)
piper agent status        # running / stopped / not set up; when running,
                          # prints the control-API address, app ports, and data dir
piper agent down          # stop it
```

Rootless is intentionally ephemeral: it does **not** survive a reboot, and
`up` prints a note saying so — re-run it after boot, or `daemonize` (below)
when you want a real service. State lives under `~/.piper/piperd`, and the
embedded Caddy's admin API sits on `:2020`.

Apps are served at `http://<name>.piper.localhost:8080`. Your user must be able to
reach a Docker socket — be in the `docker` group, or set `DOCKER_HOST`.

**If `piper agent up` reports a crash-loop**, the startup error goes to the
`systemd --user` journal, which minimal distros (e.g. Raspberry Pi OS) don't
persist — so `journalctl --user -u piperd` can be empty. Run piperd
in the foreground with the unit's environment to see the real error:

```bash
piper agent down
XDG_DATA_HOME=~/.piper/piperd XDG_CONFIG_HOME=~/.piper/piperd \
  PIPER_HTTP_ADDR=:8080 PIPER_HTTPS_ADDR=:8443 \
  PIPER_CADDY_ADMIN=http://127.0.0.1:2020 ~/.local/bin/piperd
```

A `listen address … already held` error means another piperd (commonly a
leftover system service) owns the port — stop it with `sudo systemctl stop
piperd`.

**Promote to a real daemon.** When you want a durable, boot-surviving service on
`:80`/`:443` (a Pi, a home server), promote it:

```bash
piper agent daemonize
```

This is the one durable, privileged operation: it installs and enables the
systemd **system** service (`enable --now`), and it needs root — no `sudo`
prefix required, `piper` re-runs itself under `sudo` and prompts for your
password (running it as real root works too). It works with or without a prior
`up`, and keeps an existing `/etc/piper/piperd.env`. It's a fresh durable
install — state is **not** migrated from `~/.piper` to `/var/lib/piper`;
re-enroll and redeploy. From then on `piper agent up`/`down`/`status` control
the system service (self-`sudo` when needed).

To demote again:

```bash
piper agent daemonize --undo
```

This stops and disables the system service and removes its unit, keeping
`/etc/piper/piperd.env` and the binaries (again, no state migration back); a
later `piper agent up` runs rootless again.

### macOS (dev boxes)

`piper agent up`/`down`/`status` drive a launchd agent the CLI generates for
you — nothing to install by hand, and it points at whichever `piperd` sits
beside the `piper` you ran. Like Linux rootless it is ephemeral: the plist is
kept out of `~/Library/LaunchAgents`, so login never auto-starts it and a reboot
ends it — run `piper agent up` again. There is no `daemonize` on macOS; it's a
dev box, so durability stays a Linux system-service concern. See
[`manual-setup.md`](manual-setup.md#run-the-agent-on-macos-dev-box).

Shell completions and a Homebrew tap are planned follow-ups.

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
