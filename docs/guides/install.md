# Install

One command installs `piper` and `piperd` on any platform and lands you on a
real upgrade channel: apt on Debian-family Linux, Homebrew on macOS, verified
binaries everywhere else.

```bash
curl -fsSL https://get.piperbox.dev/install.sh | sh
```

It detects your platform and hands off to the native package channel where
one exists, so every install lands on a real upgrade channel. Everywhere else
it falls back to verified binaries plus printed next steps. The sections below
cover what each channel actually does, and how to skip straight to it by hand.

## apt (Debian-family, e.g. Raspberry Pi OS)

The curl installer runs exactly this on Debian, Ubuntu, and Raspberry Pi OS:

```bash
sudo install -d -m 0755 /etc/apt/keyrings
sudo curl -fsSL https://apt.piperbox.dev/piperbox.gpg -o /etc/apt/keyrings/piperbox.gpg
sudo curl -fsSL https://apt.piperbox.dev/piperbox.sources -o /etc/apt/sources.list.d/piperbox.sources
sudo apt update && sudo apt install piperd piper
```

`apt install piperd piper` already installs, enables, and starts the systemd
service — it's durable from install, no separate step needed, and `apt
upgrade` restarts a running piperd for you. `piper agent up`, `piper agent
down`, and `piper agent status` drive that one system service from then on:

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

## Homebrew (macOS)

```bash
brew install piperbox/tap/piper
brew services start piper
```

`brew services start piper` runs piperd now and at every login (equivalent to
`piper agent up`/`down`/`status`, which wrap the same `brew services` calls).
For a headless Mac where piperd should come up at boot, before any login, use
the system-level variant instead: `sudo brew services start piper`. State
lives at `~/.piper/piperd`, and apps are served at
`http://<name>.piper.localhost`. The relay/public-URL flow works here too:
run `piper login` — piperd applies the enrollment and reconnects itself, no
restart needed. After `brew upgrade`, run `brew services restart piper` to pick
up the new binary. See
[Run piperd yourself](../self-host/piperd.md#macos-dev-box).

## Anywhere else (diet)

Not on a Debian-family distro, no Homebrew, or want just the CLI (e.g. to
drive a box from your laptop)? The same curl command falls back to placing
verified binaries only, no service management:

```bash
curl -fsSL https://get.piperbox.dev/install.sh | sh
```

It detects your OS/arch, downloads the matching release binaries, verifies
their `checksums.txt`, and installs `piper` + `piperd` to `~/.local/bin` (or
`/usr/local/bin` when run as root; `PIPER_PREFIX` overrides). It never runs
`systemctl`, never touches `/etc`, and never prompts for `sudo`. Re-run any
time to upgrade. Add `--rc` to install the latest release candidate instead of
the latest stable release, `--version vX.Y.Z` to pin a specific release, or
`--cli-only` for just `piper`. Then install the systemd unit by hand: see
[Run piperd yourself](../self-host/piperd.md).

## From source

Prefer to build `piperd`/`piper-relay` from source, run piperd in Docker via Compose,
run the relay as a service, or wire your own automation instead of the
installer? See [Run piperd yourself](../self-host/piperd.md) and
[Run your own relay](../self-host/relay.md).

Next: [First deploy](first-deploy.md).
