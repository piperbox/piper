# Progress

Coarse **map** of what's built vs. what's left — by design. Detail for any 🟡/⬜ item lives in its linked issue (`[#N]`), not here; entries stay to one line so they can't drift from the issue. Design lives in [`docs/superpowers/specs/`](docs/superpowers/specs/); plans in [`docs/superpowers/plans/`](docs/superpowers/plans/); how-to-work in [`CLAUDE.md`](CLAUDE.md).

_Last updated: 2026-07-24 — lifecycle redesign (epic [#340](https://github.com/piperbox/piper/issues/340)) complete: the installer only places binaries; `piper agent up`/`down` start/stop whichever tier is installed and `daemonize [--undo]` owns durability (#341–#345). Earlier: remote control-plane (epic [#49](https://github.com/piperbox/piper/issues/49)) complete: CLI and hosted Vercel-like dashboard drive a box over the existing tunnel through the authenticated control API — the gate (#72), control-stream routing (#73), remote CLI target (#74), health/metrics (#75), onboarding slice (#90), and dashboard (#76). Earlier: relay-held GitHub App (epic [#289](https://github.com/piperbox/piper/issues/289)) complete: one-trip login + install, brokered webhooks and tokens, org-target installs routed to org agents; dashboard repo picker enumerates all installations and mints tokens by repo owner (#308, #315, #321), and the TUI now drives the same login → install → repo-pick wizard ([#325](https://github.com/piperbox/piper/issues/325)). Earlier: per-app BYO domains (epic [#224](https://github.com/piperbox/piper/issues/224)) complete: `myshop.com` attaches to one app with a single CNAME — tokenless TLS-ALPN-01 certs through the relay splice, `:80`+`:443` routed, `piper domains` CLI (#225–#232, #267). Follow-up: [#268](https://github.com/piperbox/piper/issues/268). Earlier: epic #141 (smooth the first-box onboarding flow) closed; all six child fixes landed, remaining polish tracked standalone ([#173](https://github.com/piperbox/piper/issues/173), [#174](https://github.com/piperbox/piper/issues/174), [#175](https://github.com/piperbox/piper/issues/175)). Earlier: epic #43 (install & run piperd as a service) closed; registry publish and macOS launchd ([#56](https://github.com/piperbox/piper/issues/56)) tracked as standalone follow-ups. Plan 3 complete: push-to-deploy plus PR-preview URLs + teardown (`pr-<N>-<app>.<base>`, flattened for the wildcard cert). Live tracker: [issues](https://github.com/piperbox/piper/issues)._

Legend: ✅ done · 🟡 partial / stubbed · ⬜ not started. Issue tag/label conventions: [CLAUDE.md § Issue tracking](CLAUDE.md#issue-tracking--progress).

## Foundation

- ✅ Go module skeleton + `piper version` + Makefile (build/test/cross) — [#12](https://github.com/piperbox/piper/pull/12)
- ✅ Config loading from env with defaults — [#15](https://github.com/piperbox/piper/pull/15)
- ✅ CI `verify` (gofmt/vet/test/cross) gates PRs; no-cgo arm64 cross-compile green — [#13](https://github.com/piperbox/piper/issues/13)
- ✅ Release pipeline (goreleaser: tag → GitHub Release + cross-compiled binaries/checksums); unblocks installer/image — [#58](https://github.com/piperbox/piper/issues/58)
- ✅ apt repo: piperd+piper debs published to apt.piperbox.dev on stable tags, live-verified since v0.13.0 — [#444](https://github.com/piperbox/piper/issues/444)
- ✅ Homebrew tap: `brew install piperbox/tap/piper` + brew services, formula published by goreleaser on stable tags, live since v0.14.0 — [#445](https://github.com/piperbox/piper/issues/445)
- ✅ one-tier lifecycle: agent up/down/status wrap systemctl (Linux) / brew services (macOS); daemonize, rootless tier, and the CLI LaunchAgent deleted — [#446](https://github.com/piperbox/piper/issues/446)
- ✅ Authenticated control API — bearer token on every `piperd` request; on-box `piperd token` bootstrap + `piper login` (creds in `~/.piper/piper`) — [#72](https://github.com/piperbox/piper/issues/72)
- ✅ Tokenless on loopback — local CLI needs no login; bearer stays on the relay path (dedicated authenticated listener) and non-loopback binds — [#221](https://github.com/piperbox/piper/issues/221)

## Plan 1 — Agent core, LAN-only — epic [#9](https://github.com/piperbox/piper/issues/9) ([plan](docs/superpowers/plans/2026-07-04-piper-agent-core.md))

Goal: `piper deploy myapp --path .` → build Dockerfile → run container → health-check → serve at `http://myapp.piper.localhost` via managed Caddy; state in SQLite. No relay, no tunnel, no git.

- ✅ `store` — SQLite apps + deployments (pure-Go driver) — [#17](https://github.com/piperbox/piper/pull/17)
- ✅ `runtime` — Docker build/run/health/stop driver + fake — [#19](https://github.com/piperbox/piper/pull/19)
- ✅ `caddy` — admin-API client (upsert/remove route) + in-process manager (Caddy embedded as a library) — [#3](https://github.com/piperbox/piper/issues/3), [#39](https://github.com/piperbox/piper/issues/39)
- ✅ `deploy` — orchestrator (build → run → health → record → route → retire) — [#22](https://github.com/piperbox/piper/pull/22)
- ✅ `api` — control-plane HTTP API (`/v1/apps`, `/v1/apps/{name}/deploy`) — [#23](https://github.com/piperbox/piper/pull/23)
- ✅ Deployment history + build/deploy logs on the control API — [#101](https://github.com/piperbox/piper/issues/101)
- ✅ App lifecycle: stop + delete on the control API and CLI — [#103](https://github.com/piperbox/piper/issues/103)
- ✅ App lifecycle: start a stopped app on the control API (`POST /v1/apps/{name}/start`) — [#307](https://github.com/piperbox/piper/issues/307)
- ✅ `client` + CLI — `piper create` / `deploy` / `list` — [#24](https://github.com/piperbox/piper/pull/24)
- ✅ Async deploy progress — POST returns a `building` row (202), build runs in the background, `piper deploy` streams live build output by polling — [#140](https://github.com/piperbox/piper/issues/140)
- ✅ `piperd` wiring (config → store → docker → caddy → deploy → api) — [#7](https://github.com/piperbox/piper/issues/7)
- ✅ e2e — real Docker + Caddy, deploy sample app, curl it — [#8](https://github.com/piperbox/piper/issues/8)

## Plan 2 — Relay + tunnel + TLS — epic [#10](https://github.com/piperbox/piper/issues/10) ([plan](docs/superpowers/plans/2026-07-04-piper-relay-tunnel-tls.md))

Goal: public HTTPS from behind NAT/CGNAT — `piperd` dials an outbound yamux tunnel to `piper-relay`, which routes public `:443` by SNI (never decrypts); TLS terminates on-box with a lego-issued wildcard cert. Agent owns the domain + DNS creds (Dokploy-like).

- ✅ `tunnel` — yamux transport + token/base-domain handshake — [#10](https://github.com/piperbox/piper/issues/10)
- ✅ `certs` — lego DNS-01 wildcard issuance + renewal — [#10](https://github.com/piperbox/piper/issues/10)
- ✅ `caddy` — `:443` TLS listener + load-PEM — [#10](https://github.com/piperbox/piper/issues/10)
- ✅ `piper-relay` — enrollment (per-agent tokens), SNI passthrough, tunnel server — [#10](https://github.com/piperbox/piper/issues/10)
- ✅ `piper-relay` managed systemd service + operator docs — [#38](https://github.com/piperbox/piper/issues/38)
- ✅ `piper-relay` infra-only ops endpoints — Prometheus `/metrics` + ring-buffered `/logs`, env-toggled, bind-address isolated from the SNI dispatcher — [#396](https://github.com/piperbox/piper/issues/396)
- ✅ `piperd` — outbound tunnel client + cert wiring (additive; LAN-only unchanged) — [#10](https://github.com/piperbox/piper/issues/10)
- ✅ e2e — loopback relay path (tunnel + SNI + on-box TLS) — [#10](https://github.com/piperbox/piper/issues/10)
- ✅ **Public-relay onboarding slice (Plans 1–3)** — relay accounts + device-flow, `piper login`/`connect`, and relay-terminated shared domain; `login → connect → deploy → curl` e2e green — [#90](https://github.com/piperbox/piper/issues/90) (child of epic [#49](https://github.com/piperbox/piper/issues/49))
  - ✅ `piper login` / `piper connect` self-service onboarding CLI — device-flow login + box claim, writes piperd `relay.json` — [#83](https://github.com/piperbox/piper/pull/83)
  - ✅ Relay-terminated shared domain — typed tunnel streams (`T`/`H`/`C`); relay assigns `<app-hash>-<username>.<apex>`, terminates wildcard TLS, forwards HTTP over the tunnel; free-tier box served on `:80` with no on-box cert — [#89](https://github.com/piperbox/piper/pull/89)
  - ✅ Relay control-stream routing — account-authz'd control plane at `api.<apex>` (SNI-dispatched, wildcard cert), forwarded over `KindControlAPI` tunnel streams with agent-push Token B provisioning — [#73](https://github.com/piperbox/piper/issues/73)
  - ✅ remote CLI target — `piper --remote <base-domain>` / `PIPER_REMOTE` drives a box through the relay control plane — [#74](https://github.com/piperbox/piper/issues/74)
  - ✅ health/metrics surface — relay liveness (`GET /agents/<base>`) + per-app deploy status + `piper status` — [#75](https://github.com/piperbox/piper/issues/75)
  - ✅ GitHub identity — relay accounts on GitHub OAuth (device flow for `piper login`, relay-hosted authorization-code flow for the browser); Google flow removed — [#99](https://github.com/piperbox/piper/issues/99)
  - ✅ account agent list — `GET /agents` on the relay control API returns the caller's enrolled agents with liveness — [#98](https://github.com/piperbox/piper/issues/98)
  - ✅ domain-config API — BYO base domain + DNS creds settable remotely, live cert issuance + relay splice, shared-domain coexistence — [#102](https://github.com/piperbox/piper/issues/102)
  - ✅ TLS-ALPN-01 issuance path — tokenless exact-host certs; `acme-tls/1` passthrough spliced to an in-process solver — [#226](https://github.com/piperbox/piper/issues/226)
  - ✅ relay 1:N custom domains — `custom_domains` table, pending→active lifecycle (routable while pending for TLS-ALPN-01, 1h TTL, lazy eviction, per-agent cap), add/remove/confirm control ops — [#227](https://github.com/piperbox/piper/issues/227)
  - ✅ relay `:80` custom-domain routing — Host-matched plain HTTP pumped down the tunnel to the box's Caddy (custom domains only; enables HTTP-01 fallback) — [#228](https://github.com/piperbox/piper/issues/228)
  - ✅ deploy exact-host `:443` routes for app-owned domains — active domains routed on deploy, dropped on stop/delete, backfill hook for the domain manager — [#230](https://github.com/piperbox/piper/issues/230)
  - ✅ **per-app BYO domains** — epic [#224](https://github.com/piperbox/piper/issues/224) complete: per-domain lifecycle manager (box-wide BYO folded in as the one wildcard-shaped instance) — [#229](https://github.com/piperbox/piper/issues/229); `/v1/apps/<app>/domains` API + app-delete teardown — [#231](https://github.com/piperbox/piper/issues/231) [#267](https://github.com/piperbox/piper/issues/267); `piper domains` CLI — [#232](https://github.com/piperbox/piper/issues/232)
  - ✅ Organizations — org accounts, membership + invites, org-scoped control authz — [#104](https://github.com/piperbox/piper/issues/104)
  - ✅ org naming — users and orgs hold separate username namespaces, so an org can't take a GitHub login; a taken org name 409s instead of silently suffixing (org names stay unverified free text, by decision) — [#411](https://github.com/piperbox/piper/issues/411) [#412](https://github.com/piperbox/piper/issues/412)
  - ⬜ `piper org` CLI — org management is relay-API/dashboard-only; no CLI subcommand group yet — [#314](https://github.com/piperbox/piper/issues/314)
  - ✅ surface the routed public host — persisted on the app row at deploy; in the deploy response + `piper deploy` URL and the apps API + `piper list` — [#93](https://github.com/piperbox/piper/issues/93) [#100](https://github.com/piperbox/piper/issues/100)
  - ⬜ LAN `login` load-mutate-save so it doesn't clobber stored relay creds — [#84](https://github.com/piperbox/piper/issues/84)
  - ⬜ thread `context.Context` through `relayclient` requests — [#85](https://github.com/piperbox/piper/issues/85)
  - ✅ remove a box — store cascade delete, `DELETE /agents/<base-domain>` (org-owner only), `piper box ls`/`rm`, honest quota error — [#401](https://github.com/piperbox/piper/issues/401)
  - ✅ per-box hostname attribution — `hostnames.agent_name`; removal reclaims app slots, and two boxes on one account no longer collide on a shared app name — [#405](https://github.com/piperbox/piper/issues/405)
  - ✅ connect-time app reconciliation — one `sync-apps` op prunes rows for apps a box dropped, restores routes for the rest, and returns the assigned hostnames for the box to persist — [#418](https://github.com/piperbox/piper/issues/418)
- ✅ **Epic [#49](https://github.com/piperbox/piper/issues/49) complete** — the full remote control-plane track: the gate [#72](https://github.com/piperbox/piper/issues/72), control-stream routing [#73](https://github.com/piperbox/piper/issues/73), remote CLI target [#74](https://github.com/piperbox/piper/issues/74), health/metrics [#75](https://github.com/piperbox/piper/issues/75), the onboarding slice [#90](https://github.com/piperbox/piper/issues/90), and the hosted Vercel-like dashboard [#76](https://github.com/piperbox/piper/issues/76).

## Plan 3 — Git-driven deploys — epic [#11](https://github.com/piperbox/piper/issues/11) ([plan](docs/superpowers/plans/2026-07-05-plan3-git-deploys.md))

Goal: `git push → live HTTPS URL` via a per-user GitHub App; webhook rides the Plan-2 tunnel to `hooks.<base>`; status reported to GitHub.

- ✅ `source` — provider seam (Event/Kind/Status + Provider interface) — [#31](https://github.com/piperbox/piper/pull/31)
- ✅ `source/github` — App JWT + installation token, webhook parse (HMAC), tarball fetch, Deployments API, manifest onboarding — [#31](https://github.com/piperbox/piper/pull/31)
- ✅ `webhook` — signed webhook → app lookup → deploy, per-app serialization — [#31](https://github.com/piperbox/piper/pull/31)
- ✅ `api`/`cli` — `github setup`, `app link`, onboarding endpoints — [#31](https://github.com/piperbox/piper/pull/31)
- ✅ `app link --root-dir` — monorepo build subpath persisted per app; deploy builds from `<checkout>/<root_dir>` — [#316](https://github.com/piperbox/piper/issues/316)
- ✅ `piperd` — webhook served over the tunnel in relay mode — [#31](https://github.com/piperbox/piper/pull/31)
- ✅ PR-preview URLs + teardown (`pr-<N>-<app>.<base>`, flattened for the wildcard cert) — [#50](https://github.com/piperbox/piper/pull/50)
- ✅ Previews on a relay-terminated box — relay assigns a single-label hostname per `(account, app, pr)`, released on PR close — [#302](https://github.com/piperbox/piper/issues/302)
- ✅ Relay-held GitHub App: one-trip login + install, brokered webhooks and tokens, org-target installs routed to org agents, BYO unchanged — [#289](https://github.com/piperbox/piper/issues/289)
- ✅ Relay dashboard endpoints — `GET /v1/github/repos` (repo picker) + `GET /v1/github/status` (App install state + install URL) — [#308](https://github.com/piperbox/piper/issues/308), [#315](https://github.com/piperbox/piper/issues/315); picker enumerates all installations, labels each by target, tokens mint by repo owner — [#321](https://github.com/piperbox/piper/issues/321)
- ✅ `piper github reset` — give up a box's own App so a brokered one can take over; startup warns when one shadows the other — [#299](https://github.com/piperbox/piper/issues/299)
- ✅ Manual deploy of a linked app builds from the repo — `POST /v1/apps/{name}/deploy-from-repo`; TUI `d` and pathless `piper deploy` use it — [#331](https://github.com/piperbox/piper/issues/331)
- ✅ e2e — deploy-failure resilience (failed build keeps the old version serving) + synthetic webhook push → deploy → PR-preview lifecycle through the relay — [#337](https://github.com/piperbox/piper/issues/337)

## Install & run piperd as a service — epic [#43](https://github.com/piperbox/piper/issues/43) ✅ closed

Goal: piperd installable and self-sustaining on the box (Pi/VPS/laptop) — service unit, container image, one-line installer — without changing how it uses Docker for apps.

- ✅ Graceful `SIGTERM` shutdown (clean service stop/restart) — [#48](https://github.com/piperbox/piper/issues/48)
- ✅ Native systemd unit (`DynamicUser`+`docker` group, `CAP_NET_BIND_SERVICE`, `StateDirectory`) — [#44](https://github.com/piperbox/piper/issues/44)
- ✅ Container image + compose (host `docker.sock`; registry publish tracked separately) — [#45](https://github.com/piperbox/piper/issues/45)
- ✅ One-line `curl … | sh` installer (OS/arch detect, checksum-verified, `--cli-only`/`--rc`) — [#46](https://github.com/piperbox/piper/issues/46)
- ✅ CLI-owned lifecycle: installer places binaries only; `piper agent up`/`down` per-tier start/stop, `daemonize [--undo]` owns durability — [#340](https://github.com/piperbox/piper/issues/340)
- ✅ installer dispatch: curl|sh configures apt (Debian-family) / hands off to brew (macOS) / diet fallback with printed service steps — [#447](https://github.com/piperbox/piper/issues/447)
- ✅ Standalone `piper` CLI on PATH (`--cli-only`; drives a `piperd` on another host on the same network via `PIPER_ADDR`) — [#47](https://github.com/piperbox/piper/issues/47)

Descoped from the epic, tracked standalone:
- ⬜ launchd plist (best-effort macOS) — [#56](https://github.com/piperbox/piper/issues/56)

## First-box onboarding — epic [#141](https://github.com/piperbox/piper/issues/141) ✅ closed

Goal: turn the first-run gauntlet (fresh box → live public URL) into a clean copy-paste experience; six sharp edges hit during a headless Pi run.

- ✅ Default relay `.co` → live `.dev` — [#135](https://github.com/piperbox/piper/issues/135)
- ✅ `piperd token create` targeted the wrong DB under the shipped systemd unit — [#134](https://github.com/piperbox/piper/issues/134)
- ✅ `piper deploy` on a non-existent app: clearer error — [#139](https://github.com/piperbox/piper/issues/139)
- ✅ `piper deploy` streams build progress (no silent hang) — [#140](https://github.com/piperbox/piper/issues/140)
- ✅ Relay-mode deploy surfaces the app's public URL — [#136](https://github.com/piperbox/piper/issues/136)
- ✅ `piper login` no longer mislabels connectivity failures as "token rejected" — [#138](https://github.com/piperbox/piper/issues/138)

Remaining polish, tracked standalone:
- ⬜ `piper connect` discoverability / fail loudly off-box — [#173](https://github.com/piperbox/piper/issues/173)
- ✅ Onboarding docs: box IP over `*.local`, document `PIPER_API_ADDR` — [#174](https://github.com/piperbox/piper/issues/174)
- ⬜ Explore a `piper setup` onboarding wizard — [#175](https://github.com/piperbox/piper/issues/175)

## Interactive TUI — epic [#183](https://github.com/piperbox/piper/issues/183) ([spec](docs/superpowers/specs/2026-07-12-piper-tui-design.md), [plan](docs/superpowers/plans/2026-07-13-tui-config-and-skeleton.md))

Goal: bare `piper` in a TTY opens a full-screen control surface; every existing subcommand stays scriptable and unchanged.

- ✅ Multi-box client config (schema v2, silent migration) — [#184](https://github.com/piperbox/piper/issues/184)
- ✅ TUI skeleton: bare-piper TTY entry, root model + view stack + 2s poll, status bar, read-only apps table — [#185](https://github.com/piperbox/piper/issues/185)
- ✅ Drill-down: app detail + live deployments table, per-deployment log viewer with follow, breadcrumb navigation — [#191](https://github.com/piperbox/piper/issues/191)
- ✅ Actions: new-app form, deploy (confirm → live build), stop/delete confirms — [#194](https://github.com/piperbox/piper/issues/194)
- ✅ Key discoverability: dim footer legend on every view (the `?` help overlay it shipped with is gone — footers cover it) — [#196](https://github.com/piperbox/piper/issues/196)
- ✅ Boxes view: switcher + add/edit/remove config editor over schema v2 — [#198](https://github.com/piperbox/piper/issues/198)
- ✅ Wizards: GitHub App setup, link repo; unauth hints on apps home point at the boxes menu (LAN) / github wizard (relay) — [#200](https://github.com/piperbox/piper/issues/200) (standalone login view removed as redundant — [#408](https://github.com/piperbox/piper/issues/408))
- ✅ Per-app domains in the app drilldown: inline list + add (CNAME handoff, live issuance status) / remove — [#285](https://github.com/piperbox/piper/issues/285)
- ✅ Relay GitHub onboarding wizard (login → install → repos) + link-form repo picker/root-dir — [#325](https://github.com/piperbox/piper/issues/325)

## Always-green gates

- `make test` (unit; Docker/e2e skip cleanly when absent) · `make cross` (no-cgo arm64 build)
- `make e2e` (real Docker; runs in CI on code-touching PRs, non-required) — [#128](https://github.com/piperbox/piper/issues/128)
