# Design: Onboarding & Packaging — apt, Homebrew, and a one-tier service model

> **Status:** Design approved in brainstorming (2026-07-30). Not yet implemented.
> Supersedes the two-tier lifecycle of
> [2026-07-14-linux-rootless-toggleable-piperd-design.md](2026-07-14-linux-rootless-toggleable-piperd-design.md)
> and the macOS LaunchAgent generation of
> [2026-07-14-macos-launchd-rootless-piperd-design.md](2026-07-14-macos-launchd-rootless-piperd-design.md).

## Problem

Installing Piper today is a two-step ritual with an alien verb: `curl | sh`
places binaries (#345's "installer diet"), then `piper agent daemonize`
promotes a rootless user service into the durable system service (#340). The
promote/demote machinery (`daemonize`, `--undo`, binary copying into
`/usr/local/bin`, two systemd tiers, a self-generated macOS LaunchAgent that
dies at reboot) exists to serve a two-tier model nobody asked for. Users expect
what Tailscale/Docker deliver: **installing the thing means the service is
running**, via the package manager they already trust.

## Decision summary

| Decision | Choice |
| --- | --- |
| Service model (Linux) | **One tier**: the systemd system service on :80/:443. Rootless tier, `daemonize`, `--undo`, promotion all deleted. |
| Service model (macOS) | **brew services owns it.** `brew services start piper` (login-durable) / `sudo brew services start piper` (boot-durable). Self-generated LaunchAgent machinery deleted. macOS stops being "dev-only". |
| apt hosting | **Self-hosted static repo on GitHub Pages** at **apt.piperbox.dev** (dedicated `piperbox/apt` repo). Not Cloudsmith. |
| Homebrew | Tap **`piperbox/homebrew-tap`**, one formula `piper` (both binaries + `service` block), published by goreleaser `brews:`. |
| curl installer | **Package-manager bootstrap** (Tailscale-style): deb-family → apt repo + install; macOS with brew → brew; everything else → today's diet path + printed commands. |
| Domains | `piperbox.dev` is the project domain — infrastructure like the apt repo lives there. `getpiper.dev` stays for user apps + relay (users get arbitrary subdomains there; keep infra off it). |

Pre-1.x compatibility policy applies throughout: no migrations, no legacy
readers, no shims. Existing boxes re-provision; docs get a short manual
cleanup note.

## End-state UX per channel

| Channel | Install | Service | Upgrade |
| --- | --- | --- | --- |
| Debian-family (incl. Raspberry Pi OS) | `curl \| sh` → adds apt repo, `apt install piper piperd` | deb postinst enables + starts | `apt upgrade` (postinst restarts piperd) |
| macOS | `brew install piperbox/tap/piper` | `brew services start piper` | `brew upgrade` |
| Non-deb Linux; `--rc`; `--version`; `--cli-only` | `curl \| sh` **diet**: verified binaries into the prefix | prints exact commands (fetch unit → `systemctl enable --now piperd`) | re-run installer |
| From source | `make build` | manual unit install (documented) | rebuild |

`piper agent daemonize` ceases to exist on every platform.

## Why these choices (research, verified 2026-07-30)

**Self-hosted apt over Cloudsmith.** Cloudsmith's OSS tier is healthy (≥50GB
storage / 200GB/mo, no application) but the free tier has **no custom domain**:
every user's sources.list would encode `dl.cloudsmith.io/public/piperbox/...`
forever — the exact lock-in that burned k6 (Bintray shutdown) and pushed
Grafana and Netdata off packagecloud. Caddy, Cloudsmith's flagship OSS user,
has hit 402 bandwidth-cap outages and a GPG key expiry there. Meanwhile GitHub
CLI's apt repo is literally static files on GitHub Pages (`cli.github.com` →
`cli.github.io`) — the identical stack at enormous scale. A project whose
pitch is "self-hostable, no lock-in" should not park its apt repo somewhere it
can't exit cleanly. With `apt.piperbox.dev` as a CNAME, hosting can move later
without breaking a single installed box.

**brew can genuinely mimic Linux on macOS.** Two verified facts: (1) since
macOS 10.14, unprivileged processes may bind ports below 1024 (wildcard binds
like piperd's `:80`/`:443`; only specific-IP binds still need root), so a
user-level brew service serves :80/:443 directly; (2) `brew services start`
starts now **and at every login**; `sudo brew services start` registers a
LaunchDaemon that starts at boot (headless Macs). To verify during
implementation: whether `brew upgrade` auto-restarts the running service, or
docs say `brew services restart piper`.

**Key operational risk = GPG key expiry.** It is the #1 way apt repos actually
break users (Caddy 2025, Grafana 2019/2023, GitLab). Mitigation: a dedicated
**no-expiry** signing key, held only in the `piperbox/apt` repo's Actions
secrets, public half served at a stable URL on apt.piperbox.dev.

## Components

### 1. deb packages (nfpm, inside goreleaser)

Two packages, arches amd64 / arm64 / armhf:

- **`piperd`** — `/usr/bin/piperd`; unit at
  `/usr/lib/systemd/system/piperd.service` with `ExecStart=/usr/bin/piperd`
  (same hardening as today's `packaging/systemd/piperd.service`:
  DynamicUser, StateDirectory=piper, CAP_NET_BIND_SERVICE, docker group);
  `/etc/piper/piperd.env` as a **conffile** (dpkg preserves operator edits);
  postinst = daemon-reload + `enable --now` on fresh install, restart a
  *running* piperd on upgrade (a deliberately stopped one stays stopped —
  Debian's try-restart convention); prerm = stop + disable. The upgrade
  restart permanently kills the "new binary on disk, old process running"
  trap (#375) on apt boxes.
- **`piper`** — `/usr/bin/piper` only, so laptops can `apt install piper`.

No Docker dependency — it stays a documented prerequisite (users legitimately
choose `docker.io` vs upstream `docker-ce`; a Recommends would auto-install
one of them on default apt configs).

### 2. apt repository (`piperbox/apt` repo → GitHub Pages)

A dedicated repo whose Pages site **is** the apt repo, served at
`apt.piperbox.dev` (CNAME → `piperbox.github.io`, free HTTPS). Workflow fires
on `repository_dispatch` from piper's release workflow (plus manual
`workflow_dispatch`):

1. Download the `.deb`s from the GitHub Release (`gh release download`).
2. Rebuild `Packages`/`Release` metadata with **apt-ftparchive** (a ~30-line
   script, the NoPorts pattern — chosen over the reprepro-wrapping
   morph027/apt-repo-action to avoid a low-traffic third-party action in the
   publish path; pruning is scripted alongside).
3. Sign `InRelease` + `Release.gpg` with the no-expiry key from Actions
   secrets.
4. Publish via `upload-pages-artifact` + `deploy-pages`; state lives in the
   published site itself (re-imported each run), never in git history.
5. Prune to the last ~3 releases (~10MB/deb × 2 pkgs × 3 arches keeps us far
   under Pages' 1GB site / 100GB-month soft caps).
6. **Self-verify**: `apt update && apt install piperd` against the freshly
   published repo in a Debian container; the workflow fails loudly if a user
   would.

Users get a deb822 `.sources` file with `Signed-By:` pointing at the keyring
fetched from the same domain. **Stable releases only** — RCs stay on the
diet/GitHub-Releases path.

### 3. Homebrew tap (`piperbox/homebrew-tap`)

One formula `piper`: installs both binaries from the darwin tarballs
(amd64 + arm64), plus a `service` block that runs piperd with keep_alive,
logs under brew's var/log, and env pinning the data dir (the port pinning the
old LaunchAgent wrapper did is no longer needed — unprivileged low-port binds
work; Caddy admin stays on its :2019 default). goreleaser's `brews:`
publisher updates the formula on release. `brews:` is deprecated in favor of
casks, but **casks cannot declare services** — using it is deliberate and the
config carries a comment saying so.

### 4. CLI surface (`piper agent`)

**Deleted:** `daemonize` + `--undo`; the rootless tier (user unit,
`materializeRootless`, embedded user unit/env assets, `systemTier()`
detection); binary promotion into `/usr/local/bin`; the macOS LaunchAgent
machinery (plist template, `materializeLaunchd`, legacy-plist eviction, mac
env seeding).

**Remains:** `piper agent up|down|status` as thin lifecycle wrappers:

- **Linux**: wrap the system unit (`systemctl start/stop/is-active piperd`);
  `selfExecSudo` re-exec stays so a bare `piper agent up` works. No unit
  installed → "piperd is not installed — see the install docs" (never
  materialize anything).
- **macOS**: delegate to `brew services start/stop/info piper`; no brew or no
  formula → clear error + install hint.
- `status` keeps `printAgentVersions` (running vs on-disk mismatch warning)
  and crash-loop detection.

### 5. install.sh

Keeps its shape (POSIX sh, checksum verification, `--cli-only` / `--rc` /
`--version`, shadow warnings) and gains platform dispatch:

- **Debian-family with systemd** (`/etc/os-release` ID/ID_LIKE + apt-get
  present): write keyring + deb822 sources, `apt-get update`,
  `apt-get install piper piperd`, re-exec under sudo Tailscale-style. Warn
  when stale `/usr/local/bin/piper{,d}` copies would shadow the new
  `/usr/bin` ones (the #375 trap in new clothes, caught at install time).
- **macOS with brew**: `brew install piperbox/tap/piper` +
  `brew services start piper` (the sudo/boot-durable variant stays a
  documented alternative, not something the installer chooses).
- **Everything else** (non-deb Linux, no systemd, containers, no brew, and
  always for `--cli-only` / `--rc` / `--version`): today's diet path —
  verified binaries into the prefix — plus printed next-step commands. To
  make those commands work, the GitHub Release gains
  `packaging/systemd/piperd.service` (the `/usr/local/bin` ExecStart
  variant) as a goreleaser `extra_files` artifact, alongside the existing
  `piper-relay.service`.

### 6. Docs

- **README quick start**: the `daemonize` line dies. One curl command (or the
  brew line) → `piper login` → deploy. `piper login` now claims the box too —
  see [`2026-07-31-one-command-login-design.md`](2026-07-31-one-command-login-design.md),
  which supersedes the `piper connect` step this section originally described.
- **docs/getting-started.md**: per-channel install section — apt manual
  lines, brew tap, non-deb diet install, source build.
- **docs/manual-setup.md**: keeps deep manual paths; gains the non-deb
  unit-install commands the installer prints.
- **Migrating an existing box** (short note): remove the old rootless user
  unit / hand-placed `/etc/systemd/system/piperd.service` (which would shadow
  the deb's `/usr/lib` unit) and stale `/usr/local/bin` binaries. No code
  shims, per the pre-1.x policy.

## Error handling

- installer: apt path failures (no sudo, apt locked, repo unreachable) fall
  back with a clear message, never half-configured — sources+keyring writes
  are the last step before `apt-get update`, and a failed update removes
  them.
- deb postinst guards `systemctl` invocations (containers/chroots without
  systemd get the files but skip enable/start, matching Debian convention).
- `piper agent` never invents an install: missing unit/formula is a stated
  condition with the next command to run, not a trigger to materialize files.
- apt publish workflow is atomic per Pages deploy; the self-verify step gates
  a broken metadata/signing state from ever being the live repo (a stale
  cached `Packages` vs fresh `InRelease` mid-propagation resolves on retry —
  tiny repo, tolerated).

## Testing

TDD throughout, extending harnesses that exist:

- **install.sh**: the Go harness in `packaging/install` (fake GitHub via
  httptest) gains platform-dispatch tests with stubbed `apt-get`/`sudo`/
  `brew`/`uname` on PATH: deb path writes sources+keyring and invokes apt;
  brew path invokes brew; diet path byte-identical behavior to today;
  `--cli-only`/`--rc` force diet.
- **cmd/piper agent tests**: existing tables (which already stub
  systemctl/launchctl) rewritten for the simplified dispatch, including brew
  delegation and the "not installed" states.
- **Packages CI job**: `goreleaser check` + snapshot build; assert deb
  contents and maintainer scripts via `dpkg-deb -c` / `-I` (systemd-in-Docker
  is deliberately out of CI; the real enable/start is covered by the apt
  repo's self-verify container and the release smoke).
- **apt repo workflow**: self-verifies every publish (above).
- **Tap CI**: `brew audit --strict` + `brew style` in `piperbox/homebrew-tap`.
- **Release skill/runbook**: gains end-to-end smokes — apt install on a clean
  Debian box, brew install + `brew services start` on a Mac.

## Delivery (4 issues → 4 plans → 4 PRs, in order)

1. **[repo] deb packages + apt repo pipeline** — nfpm in goreleaser, the
   `piperbox/apt` repo + workflow, apt.piperbox.dev live and self-verified.
2. **[repo] Homebrew tap + formula** — tap repo, formula with service block,
   goreleaser `brews:` publishing.
3. **[cli] one-tier lifecycle** — the deletions (daemonize/rootless/
   LaunchAgent) and the up/down/status rewrite. Lands only after both
   channels exist, so there is never a release with no durable path.
4. **[repo] installer dispatch + docs** — install.sh platform bootstrap,
   README/getting-started/manual-setup overhaul.

### One-time manual setup (owner: Faruk)

- DNS: `apt.piperbox.dev` CNAME → `piperbox.github.io`.
- Generate the no-expiry apt signing key; store private half as an Actions
  secret in `piperbox/apt`; publish public half into the Pages site.
- Fine-grained PAT (push to `piperbox/homebrew-tap` + dispatch to
  `piperbox/apt`) as a secret in the piper repo for goreleaser/release
  workflow.
- Create `piperbox/apt` and `piperbox/homebrew-tap` repos (can be done via
  `gh` during implementation).

## Out of scope

- rpm/yum, AUR, nix, Windows — add per demand later.
- An apt `rc`/`edge` suite — RCs stay on GitHub Releases via the diet path.
- `piper-relay` packaging — stays tarball + manual service
  (docs/manual-setup.md), unchanged.
- Any migration tooling for existing rootless/daemonized boxes (pre-1.x
  policy; manual cleanup note only).
