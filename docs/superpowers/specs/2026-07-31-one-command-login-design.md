# Design: One-command login — daemon-owned enrollment

Merges `piper login` and `piper connect` into a single `piper login` that ends
with the box live behind the relay — no printed sudo commands, no copy-paste
env-file upsert, no visible restart step. Supersedes the "`piper login` →
`piper connect` → deploy" golden path fixed a day earlier in
[`2026-07-30-onboarding-packaging-design.md`](2026-07-30-onboarding-packaging-design.md)
(§6 Docs); everything else in that spec stands.

This is a UX + mechanism doc. The trust model is unchanged from
[`2026-07-07-relay-control-trust-model-design.md`](2026-07-07-relay-control-trust-model-design.md):
the three anchors (enrollment token, account credential, Token B) stay separate
secrets in separate trust domains — this design only *chains* their acquisition
into one command, which that spec explicitly blesses ("the caller still
experiences one login").

## Problem

Onboarding today is three user actions, two of them hostile to newcomers:

1. `piper login` — GitHub identity against the relay. Fine.
2. `piper connect` — claims the box, then **prints a sudo shell one-liner** the
   user must copy-paste (systemd installs: the CLI can't write the DynamicUser
   `StateDirectory`, so enrollment goes into root-owned `/etc/piper/piperd.env`
   — `cmd/piper/relayonboard.go:301-315`). The printed command also lands the
   enrollment token and webhook secret in the user's shell history.
3. A manual restart (`sudo systemctl restart piperd` / `brew services restart
   piper`) — piperd reads config exactly once at boot (`config.Load`,
   `internal/config/config.go:46-76`); connect deliberately never restarts it.

The root cause is architectural: **the CLI does the daemon's writing.** The CLI
lacks permission to piperd's state on exactly the tiers we ship (deb DynamicUser
`/var/lib/piper`; sudo-brew `/var/root/.piper/piperd`), while piperd itself owns
its data dir on *every* tier — even under `ProtectSystem=strict`, the
StateDirectory is its one writable path.

## Decision summary

- **One verb.** `piper login` runs identity → box claim → apply → verify as one
  pipeline. `piper connect` is deleted (pre-1.x, no alias, no shim).
- **piperd-driven enrollment.** The CLI hands the account credential to the
  local piperd; **piperd itself** calls the relay's `POST /v1/enroll`, validates
  the returned token with a real tunnel handshake, and persists `relay.json`
  into its own data dir. The CLI never writes enrollment files again, on any
  tier.
- **Unix-socket local surface.** The enrollment + status endpoints are served
  on a unix socket only piperd can create — never on the TCP control API, never
  on the relay-facing authenticated listener. This kills the three loopback-TCP
  attacks found in adversarial review (browser CSRF, port squatting, hostile
  relay reaching the endpoint).
- **Apply = validate → persist → drain → re-exec.** After responding, piperd
  runs its existing tested shutdown path, then `syscall.Exec`s its own image
  (same PID — systemd/launchd see nothing). The fresh boot re-runs today's
  relay wiring unchanged; no hot-start refactor.
- **Idempotent claim.** piperd mints a durable per-box id; the relay's enroll
  upserts on `(account, box_id)` — re-running login never burns a second quota
  slot or changes the base domain.
- **Env channel demoted to operator pin.** `PIPER_RELAY_*` env keys stay
  readable (BYO end-to-end-TLS mode, e2e harness, container installs) but the
  golden path stops writing them; their presence locks the enrollment endpoint
  (409 "env-managed"). Existing env-enrolled boxes keep working untouched.
- **Terminated-mode only.** The daemon-owned path covers the golden public-relay
  flow (`Terminated: true`). BYO/non-terminated stays the documented operator
  env flow — its boot does synchronous ACME with fatal failure modes that must
  not be reachable from a drive-by local POST.

## End-state UX

```
$ piper login
To sign in, open https://… and enter the code: ABCD-1234
logged in to relay as ozykhan
claiming this box… enrolled as a1b2-ozykhan.public.getpiper.dev
applying… piperd connected — this box is live
Install the Piper GitHub App on the repos you want to deploy: …
```

Re-running `piper login` converges: valid credential → device flow skipped;
box enrolled and matching the account → claim skipped; tunnel down → just
re-verified. On a laptop with no piperd (remote-management login), it stops
after identity with an explicit note: *"identity only — no piperd on this
machine; run this on a box to connect it."* Exit 0 — that flow is first-class
(`--remote`, `piper box ls`).

### Flag surface (decided here, not discovered later)

| Flag | Meaning | Stage it affects |
| --- | --- | --- |
| `--web` | brokered browser login (unchanged) | identity |
| `--relay <url>` | non-default relay (unchanged) | identity + claim |
| `--token <t> --addr <u>` | LAN box login (unchanged, identity-only — never enrolls) | identity |
| `--org <slug>` | enroll the box under an org (owner role; rides the enroll body the relay already accepts, `internal/relay/api.go:262-282` — previously CLI-unreachable) | claim |
| `--no-enroll` | pin the laptop meaning; stop after identity | claim |
| `--re-enroll` | force a fresh claim on an already-enrolled box (recovery after `piper box rm`, account/relay switch) | claim |

`--remote` stays rejected: login is a local operation.

## Components

### 1. piperd: local enrollment socket

A second, tiny HTTP surface on a unix socket. **Why a socket, not the loopback
TCP API:** adversarial review found the tokenless TCP endpoint (a) reachable by
CSRF from any web page the box owner visits — a no-preflight cross-origin
`fetch` to `127.0.0.1:8088` can re-enroll the box against an attacker's relay
account, after which `provisionRelayControl` pushes an admin bearer to the
hostile relay; (b) squattable — in the piperd-not-yet-running window any local
process can bind 8088 and harvest the enrollment POST's secrets; and (c) one
mux-registration mistake away from being reachable by the relay's own Token B
through the tunnel. A socket in a piperd-owned directory ends all three:
browsers can't speak unix sockets, squatters can't create the socket file, and
the relay-facing listener never learns the routes.

Socket placement (authenticity comes from *who can create it*):

- systemd: unit gains `RuntimeDirectory=piper`, `RuntimeDirectoryMode=0755` →
  `/run/piper/piperd.sock`. The directory is created by systemd for the
  DynamicUser; only piperd (or root) can create the socket inside it. Socket
  mode 0666 — any local user may *talk* to it, same trust stance as today's
  tokenless loopback listener, but nobody else can *be* it.
- running as root outside systemd (sudo-brew): `/var/run/piper/piperd.sock`,
  same ownership logic. This fixes the sudo-brew gap where the CLI-written
  `relay.json` landed in the wrong home.
- per-user (brew default, dev): `<dataDir>/piperd.sock` — the 0700 data dir the
  CLI user already owns.

The CLI probes the system path first, then the default data dir. It never
derives this from `cc.Addr` — after a LAN `piper login --token --addr
http://pi:8088`, `cc.Addr` names a *remote* box and using it would invert the
on-box guard and ship enrollment secrets over the LAN.

Routes (built in `cmd/piperd` as a separate mux with injected callbacks,
mirroring the `onGitHubApp` pattern — **never registered in `api.New`**; a
contract test asserts the authenticated listener answers 404 for these paths
even with a valid admin bearer):

- `GET /v1/version` — positive identification of piperd (probe target).
- `GET /v1/relay/status` — fixed shape, built field-by-field, never a marshal
  of `RelayFile`: `{enrolled, env_managed, relay_addr, base_domain, tunnel:
  "connected"|"retrying"|"off", last_tunnel_error}`. A test asserts the
  enrollment token and webhook secret never appear in the body.
  (`TunnelClient` gains last-error recording — today a rejected token only
  `log.Printf`s inside the retry loop and is invisible to any surface.)
- `POST /v1/relay/enroll` — body `{relay_api, account_credential, org,
  replace}`. Guards, in order: 409 `env-managed` when `PIPER_RELAY_ADDR` is in
  the process env; 409 `already-enrolled` (with current base domain) unless
  `replace` — write-once is the defense-in-depth against drive-by local
  re-pointing; 409 `busy` while a deployment is `building` or another enroll is
  in flight (serialized; closes the concurrent-login TOCTOU).

### 2. piperd-driven claim (fixes the quota-orphan hole)

Adversarial review confirmed a real hole in CLI-side orchestration: the relay
stores only the token's hash (`internal/relay/store.go`), so a crash between a
successful `Enroll` and local persistence strands a quota slot with an
unrecoverable token; the retry burns a second slot and a new base domain — at
cap 3, a flaky box bricks the account. Both fixes land here:

1. **Durable box identity.** On first enroll piperd mints a UUID, persisted to
   `<dataDir>/box-id` *before* any relay call. It survives re-enrollment and
   never leaves the machine except inside the enroll request.
2. **Idempotent relay upsert.** `POST /v1/enroll` gains `box_id`;
   `EnrollForAccount` upserts on `(account_id, box_id)`: fresh token minted
   (hash replaced), base domain and quota slot reused. Schema edit in place
   per pre-1.x policy — `agents` gains a `box_id` column; existing hosted-relay
   rows (ours only) predate it and simply get re-enrolled.

Enroll handler flow: guards → load-or-mint box-id → relay `Enroll(cred,
box_id, org)` → **validate**: complete a real `tunnel.Dial` handshake against
the returned endpoint with the returned token, then close → persist
`relay.json` via the existing atomic `config.SaveRelayFile` (`Terminated:
true`) → respond `200 {base_domain, relay_addr}` with `Content-Length` and an
explicit flush → apply (below). Validate-before-persist means a bad enrollment
can never poison the next boot. The account credential is used for the one
relay call and **never persisted on the box**.

Relay errors pass through to the CLI with today's exact UX: `ErrBadCredential`
→ "run `piper login` again"; `ErrQuotaExceeded` → the `piper box ls` /
`piper box rm` guidance (never "upgrade").

### 3. Apply: drain, then re-exec

`syscall.Exec` of piperd's own image replaces the process without the PID
changing — systemd and launchd observe nothing; the fresh boot runs
`config.Load` and all of today's boot-time relay wiring (ALPN solver, domain
manager, `TunnelClient`, registrar-before-Run ordering #373, webhookStarter)
with zero new code paths. The in-process hot-start alternative was examined and
rejected: `BaseDomain` is frozen at construction into `deploy.New`, `api.New`,
the webhook route, domain options and both HTTP servers, and Caddy is
process-global with a one-manager invariant — a cross-cutting refactor that
duplicates what re-exec gets for free.

Raw exec, however, skips every shutdown invariant, so apply is **drain first**:
after the response is flushed, run the existing `shutdownWithContext` teardown
— `FailBuildingDeployments` (no #158 regression: no deployment rows stuck at
`building` forever), API drain, tunnel join (#242), Caddy stop, store close
with clean WAL — then exec:

- Linux: exec `/proc/self/exe` (immune to the binary being replaced or
  unlinked on disk since boot). macOS: `os.Executable()`.
- Integrity gate: piperd stats its executable at boot (dev+ino) and refuses to
  exec if it no longer matches — on the sudo-brew tier the program path is
  admin-user-writable while piperd runs as root, and an unchecked exec would
  turn "replace the binary + POST to the socket" into local root. On refusal
  or exec failure (`ENOENT`, `ETXTBSY`): `os.Exit(1)` — `Restart=on-failure`
  and brew's `keep_alive` relaunch it supervised (re-reading `EnvironmentFile`
  and re-applying sandboxing); never exit 0, which would leave a systemd box
  permanently down. An unsupervised dev-terminal piperd dies with a clear log
  line; acceptable for that tier.
- Environment: pass `os.Environ()` — the unit's `Environment=`/
  `EnvironmentFile=` values (`PIPER_DATA_DIR`, `XDG_*`) must survive or the
  fresh boot silently uses a different, empty data dir.

### 4. CLI pipeline, guards, and exit-code staging

Pipeline: identity (unchanged three modes) → local probe → claim → apply-wait →
advisory GitHub-App poll (unchanged, #297).

**On-box guard** (#173 evolves, doesn't regress): the file heuristics
(`agentInstalled`: data dir / `SystemManaged` / resolvable binary) stay and are
*combined* with the socket probe, because reachability alone merges four states
the user needs distinguished:

| Probe | `agentInstalled()` | Meaning | Action |
| --- | --- | --- | --- |
| socket answers `/v1/version` | — | piperd live | proceed to claim |
| no socket | true | installed but not running | "this box is NOT connected — start it: `piper agent up`, then re-run `piper login`" (agent up self-sudos; exit non-zero) |
| no socket | false | no piperd here | laptop path: identity-only, exit 0, explicit note |
| socket answers, no `/v1/relay/status` route | — | pre-merge piperd still running (#375 trap) | "piperd is too old / stale — restart the service" |

The relay `Enroll` call happens **only inside piperd**, which is itself the
proof the box exists — the guard can no longer be fooled into burning a quota
slot off-box, and there is no window where secrets are POSTed at an
unidentified port-holder.

**Staleness cross-check** (fixes the `box rm` deadlock): when status says
`enrolled`, the CLI compares the reported base domain against the account's
relay-side box list. Enrolled-locally-but-unknown-to-the-relay (removed via
`piper box rm`, account switch, relay switch) → prompt for
`piper login --re-enroll` instead of silently skipping the claim forever.
`piper box rm`'s "run `piper connect` again" message updates accordingly.

**Exit-code staging** (extends #297's discipline explicitly):

1. Credential persisted → identity is durable. Everything before that failing
   → exit 1.
2. Enrollment persisted by piperd (the 200 from `/v1/relay/enroll`) → claim is
   durable. A hard claim failure before that 200 (guard 409s, relay errors,
   installed-but-stopped) exits non-zero even though identity persisted — the
   message says so ("logged in, but this box is not connected"), and re-running
   `piper login` skips straight to the claim. A failure after a relay upsert
   but before persistence cannot strand a slot anymore (upsert is idempotent),
   so no compensation logic is needed.
3. After the 200: the CLI treats any socket drop as "apply started" and polls
   status (~60s deadline). Tunnel not yet connected at deadline → **advisory**:
   exit 0 with a note — `TunnelClient`'s retry loop self-heals a briefly-down
   relay, exactly the #297 shape. `last_tunnel_error` showing an auth reject →
   real error message. Socket never returns → consult the supervisor
   (`systemctl is-active piperd` needs no root; `brew services info piper
   --json`) and print the journalctl / log-path hint (mirrors the
   `waitActive` crash-loop detection, #211/#392).
4. GitHub-App install poll: advisory, unchanged.

### 5. Env channel: operator pin, not enrollment channel

`config.Load`'s env-over-file precedence is untouched. What changes is *who
writes what*: the golden path never touches `PIPER_RELAY_*` again, and their
presence in piperd's environment makes `/v1/relay/enroll` refuse (409) and
`status` report `env_managed: true`. This kills the shadowing trap (a
piperd-written `relay.json` silently overridden by stale env keys) without
deleting the channel that BYO end-to-end-TLS mode, `test/e2e/relay_test.go` /
`webhook_test.go`, the container image, and self-hosted manual setups are built
on. Existing env-enrolled boxes (the Pi fleet) keep working across the upgrade
with no operator action. `piperd.env.example` comments are reworded: the relay
keys are the operator/BYO surface, not the onboarding surface.

## What the adversarial review found → what answers it

Three independent review passes (security / operations / protocol-UX) attacked
the draft; every blocker maps to a mechanism above.

| Finding (severity) | Answer |
| --- | --- |
| Loopback-TCP enroll is CSRF-able from any web page; squattable pre-start; response secrets harvestable (blocker) | unix socket in piperd-creatable dir (§1) |
| Persist-then-apply of a bad enrollment → 2s crash loop, root-only rollback (blocker) | validate-before-persist + terminated-only scope (§2) |
| Crash between relay Enroll and local persist strands an unrecoverable quota slot; retry burns another (blocker) | box-id + idempotent upsert, claim executed by the process that owns durability (§2) |
| Re-exec as root of an admin-writable binary = local privilege escalation on sudo-brew (blocker) | boot-time dev+ino integrity gate; refuse + supervised restart (§3) |
| Raw exec skips shutdown: #158 building-rows regression, WAL cut, 202-flush race (serious) | drain-then-exec reusing `shutdownWithContext`; Content-Length + flush; CLI tolerates dropped socket (§3, §4) |
| One mux-registration mistake exposes enroll to the relay's Token B (serious) | routes never in `api.New`; contract test: 404 via authenticated listener (§1) |
| Reachability-only on-box guard misdiagnoses stopped/stale/absent piperd (serious) | probe × `agentInstalled` matrix (§4) |
| `box rm` → local state stale → no re-enroll path left (blocker) | staleness cross-check + `--re-enroll` (§4) |
| Deleting env channel breaks BYO mode, e2e, containers (serious) | demote-don't-delete; env presence locks the endpoint (§5) |
| Status endpoint leaking `RelayFile` secrets to the relay (minor) | fixed response shape + no-secrets test (§1) |
| Any local process can re-point a live box (minor) | write-once + explicit `replace` + WARN-log of accepted relay addr (§1) |

**Accepted residual risk:** the account credential transits the box once,
inside the enroll POST over the piperd-owned socket, and is never persisted
there. This is chained use of an anchor, not anchor merging. The stricter
alternative — a relay-minted, short-lived, single-use *claim grant* the CLI
fetches and hands to piperd so the credential itself never touches the box — is
**deferred**: one more relay endpoint, worth doing if box-side compromise of a
just-onboarded machine becomes a realistic concern. Noted for the trust-model
doc's next revision.

## Testing

- **Relay:** upsert semantics — same `(account, box_id)` re-enroll keeps base
  domain and slot count, rotates the token hash; org body still honored; quota
  still enforced for distinct box ids.
- **piperd unit:** enroll handler with faked relay + tunnel-handshake seams —
  guard order (env-pin 409, already-enrolled 409/replace, busy 409),
  validate-fails-means-nothing-persisted, box-id minted once and reused; status
  shape contains no secret strings; socket perms/placement per tier.
- **Contract:** authenticated (relay-facing) listener 404s both new routes with
  a valid admin bearer; unit files still pass `TestPiperdServiceContract` /
  `TestDebUnitOnlyDiffersInExecStart` with `RuntimeDirectory=` added to both.
- **CLI:** rewrite the deliberately-pinned tests — `TestConnectSystemManagedGuidesEnvInstall`,
  `TestConnectEnrollsAndWritesRelayFile`, `TestRestartHintPerPlatform`,
  connect's off-box tests — into the pipeline equivalents (probe matrix,
  staging exit codes, laptop path, `--re-enroll`, staleness prompt). The #84
  (LAN login preserves relay creds) and #297 (advisory-poll exit-0) pins stay
  as-is.
- **e2e:** merged-login happy path against the in-repo relay using a real
  socket + re-exec on a dev-tier piperd; existing env-var-driven relay e2e
  stays untouched (it now doubles as the env-pin regression test).
- **Drain:** enroll during an in-flight deploy → 409 busy; enroll then verify
  no `building` rows survive the re-exec.

## Delivery

Three issues, in order (each its own plan/PR): **(1)** `[relay]` idempotent
enroll upsert + `box_id` schema; **(2)** `[agent]` socket surface, daemon-driven
claim, drain-then-exec apply, tunnel last-error; **(3)** `[cli]` merged `piper
login` pipeline, `piper connect` removal, docs (README quick start,
getting-started §login/connect, `box rm` message, TUI wizard spec note,
onboarding-packaging spec §6 golden-path line).

## Out of scope

- **LAN token minting still mentions sudo** (`sudo piperd token create` on
  systemd boxes; `TestLoginUsageMentionsSudo`). Different surface, same root
  cause (a second process writing service-owned state); the socket built here
  is the obvious future home for a no-sudo token mint — later.
- **BYO/non-terminated onboarding** stays the documented operator env flow.
- **Claim-grant hardening** (credential never transits the box) — deferred, see
  above.
- **TUI login/connect wizard** — targets the merged verb when its phase lands;
  spec note only.
