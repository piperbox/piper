# Design: login-flow state in Postgres

Second child of the relay scale-out epic (#524, child issue #522). The
[edge design](2026-09-04-relay-edge-ownership-design.md) left one thing
process-bound: everything a login flow remembers between two HTTP requests.
Device-flow polls, dashboard web-login `state`, CLI browser-login handles,
and the login rate limiter all live in maps inside one `piper-relay`
process, so `piper-edge` pins `api.<apex>` to the earliest-started relay and
that relay is a single point of failure for every login. This design moves
that state into the Postgres the relays already share and unpins the edge.

## Decision summary

- **Four tables, each with `expires_at`, swept on insert.** No janitor
  goroutine: every insert first deletes that table's expired rows, the same
  opportunistic sweep the maps do today. Expiry is a server-side predicate
  (`expires_at > now()`), like `relay_instances` liveness, so relay clocks
  never disagree about a flow's lifetime.
- **Single use is a guarded `DELETE … RETURNING`.** One statement claims a
  web state or a finished CLI handle; two relays racing the same redeem
  cannot both win.
- **Credentials are minted by the relay that hands them out.** The CLI
  callback used to mint the account credential and park the plaintext in
  the handle until the CLI polled. Now the callback records the account and
  the poll mints. No secret sits at rest in a login table.
- **Device flows are stateless.** The background goroutine that polled
  GitHub per flow goes away. The row holds GitHub's device code and a
  `next_poll_at`; each CLI poll that arrives after that instant makes one
  upstream token request. Any relay serves any poll, and a relay that dies
  mid-flow strands nothing.
- **The rate limiter is a fixed window in one upsert.** Per-IP key, one
  minute, allow while hits ≤ 30. The token bucket's burst shaping is not
  worth a read-modify-write transaction on every login request.
- **The edge round-robins `api.<apex>`.** Nothing about login depends on the
  process anymore, so the pin and its comment go.

Rejected: sharing only device-flow *results* while the starting relay keeps
polling (a restart strands the flow until the device code expires, and it
fights the graceful-drain child); leaving the rate limiter per-process
(N relays behind the edge means N× the budget for one attacker); storing the
minted credential in the handle row (a bearer secret at rest for up to ten
minutes for no benefit).

## Non-goals

- **The installation-reconcile throttle** (`lastReconcile`, #470) stays
  per-process. It is a cost cap, not correctness; forgetting it on restart
  costs one extra GitHub call per account.
- **Sticky sessions or an edge-side cookie.** Postgres makes any relay
  sufficient; steering would be a second mechanism for the same job.
- **Changing the CLI or dashboard.** Every wire shape (`/v1/login/*`
  requests and responses, cookies, redirects) is unchanged.

## Data model

All four tables are additive to `schema.sql`, which is `CREATE TABLE IF NOT
EXISTS` and applied by `Open`, so the hosted relay picks them up on the next
deploy without a database reset.

```sql
-- login_web_states: a pending dashboard browser login. Redeemed once by the
-- callback; the browser's cookie must also carry the same state.
CREATE TABLE IF NOT EXISTS login_web_states (
    state        TEXT PRIMARY KEY,
    redirect_uri TEXT NOT NULL,
    expires_at   TIMESTAMPTZ NOT NULL
);

-- login_cli_handles: a brokered CLI browser login (#291). confirmed flips
-- when the user types the code; account_id is set by the callback and read
-- by the poll, which mints the credential and deletes the row.
CREATE TABLE IF NOT EXISTS login_cli_handles (
    handle     TEXT PRIMARY KEY,
    user_code  TEXT NOT NULL,          -- normalized: upper-case, no dash
    confirmed  BOOLEAN NOT NULL DEFAULT FALSE,
    account_id TEXT REFERENCES accounts(id) ON DELETE CASCADE, -- NULL until the callback
    expires_at TIMESTAMPTZ NOT NULL
);

-- login_device_flows: a GitHub device-code login. The relay serving a poll
-- asks GitHub only once next_poll_at has passed; the poll that gets a
-- terminal answer returns it and deletes the row.
CREATE TABLE IF NOT EXISTS login_device_flows (
    handle        TEXT PRIMARY KEY,
    device_code   TEXT NOT NULL,
    interval_secs INTEGER NOT NULL,
    next_poll_at  TIMESTAMPTZ NOT NULL,
    expires_at    TIMESTAMPTZ NOT NULL
);

-- login_rate: fixed-window login rate limit per client key (#106).
CREATE TABLE IF NOT EXISTS login_rate (
    key          TEXT PRIMARY KEY,
    window_start TIMESTAMPTZ NOT NULL,
    hits         INTEGER NOT NULL
);
```

`login_cli_handles.account_id` references `accounts` so a handle can never
name an account that does not exist; the operator kill-switch is a flag
(`DisableAccount`), and `denyDisabled` runs in the callback before the handle
is finished, as today. `login_rate` has no `expires_at`: a stale window is simply reset by the next
hit, and rows older than an hour are swept on the same statement.

## Store (`login_state.go`)

New file in `internal/relay` holding the `Store` methods for the four tables,
the way `bindings.go` and `accounts.go` hold theirs. Every method is one
statement or one short transaction; none holds Go-side state.

**Web states**

- `PutWebState(state, redirectURI string, ttl time.Duration) error` — sweeps
  expired rows, inserts.
- `TakeWebState(state string) (redirectURI string, ok bool, err error)` —
  `DELETE … WHERE state = $1 AND expires_at > now() RETURNING redirect_uri`.

**CLI handles**

- `PutCLIHandle(handle, userCode string, ttl time.Duration) error` — sweeps,
  inserts with the normalized code.
- `ConfirmCLIHandle(enteredCode string) (handle string, ok bool, err error)` —
  reads the unconfirmed, unexpired rows, compares in Go with
  `subtle.ConstantTimeCompare` exactly as today, then
  `UPDATE … SET confirmed = TRUE WHERE handle = $1 AND NOT confirmed`;
  zero rows affected means another relay claimed it first, so return not-ok.
- `CLIHandle(handle string) (CLIHandle, bool, error)` — the row, for the
  callback's checks (confirmed, unexpired).
- `FinishCLIHandle(handle, accountID string) error` — sets `account_id`;
  refuses (zero rows) if the handle is gone or unconfirmed.
- `TakeFinishedCLIHandle(handle string) (accountID, username string, state cliHandleState, err error)` —
  one query returning one of: unknown/expired, pending, done. Done is a
  `DELETE FROM login_cli_handles h USING accounts a WHERE h.handle = $1 AND
  h.account_id = a.id AND h.expires_at > now() RETURNING a.id, a.username`,
  folding the account lookup into the delete; the caller mints the credential
  from the returned account.

**Device flows**

- `PutDeviceFlow(handle, deviceCode string, interval int, ttl time.Duration) error`.
- `DeviceFlow(handle string) (DeviceFlow, bool, error)` — row read; the
  returned `Due` reports whether `next_poll_at` has passed.
- `DeferDeviceFlow(handle string, by time.Duration) error` — pushes
  `next_poll_at` `by` into the future (normal interval, or interval + 5 s
  after `slow_down`).
- `DeleteDeviceFlow(handle string) error` — after the poll returns the
  outcome, so a handle redeems once.

**Rate limit**

- `LoginHit(key string, now time.Time, window time.Duration) (hits int, err error)`:

```sql
INSERT INTO login_rate (key, window_start, hits) VALUES ($1, $2, 1)
ON CONFLICT (key) DO UPDATE SET
    hits         = CASE WHEN login_rate.window_start <= $2 - $3 THEN 1 ELSE login_rate.hits + 1 END,
    window_start = CASE WHEN login_rate.window_start <= $2 - $3 THEN $2 ELSE login_rate.window_start END
RETURNING hits
```

The caller allows while `hits <= loginLimitPerMin`. `now` is passed in rather
than `now()` so the existing clock seam and its tests keep working.

## API changes (`api.go`, `weblogin_cli.go`)

The `api` struct loses `webStates` and `cliStates`; `mu` stays for
`lastReconcile`. Handlers change body only, never shape:

- `loginWeb` — `PutWebState`, then cookie + redirect as today.
- `loginCallback` — cookie check as today, then `TakeWebState`; not-ok is the
  same `400 bad state`.
- `cliLoginStart` — `PutCLIHandle`.
- `cliLoginConfirm` — `ConfirmCLIHandle`; not-ok re-renders the page with
  the same message.
- `cliCallback` — `CLIHandle` for the confirmed/unexpired/cookie checks,
  exchange the code, upsert the account, `denyDisabled`, then
  `FinishCLIHandle`. Whether to bounce to the install page is decided here as
  today, from `InstallationsForAccount`. It no longer mints.
- `cliLoginPoll` — `TakeFinishedCLIHandle`. Done: `MintAccountCredential`,
  look up the account for `username`, recompute `install_url` (empty when the
  account has an installation), respond `200`. Pending: `202
  authorization_pending`. Unknown or expired: the same `400`s as today.
  The row is deleted before the mint; a mint failure means the user restarts
  the flow, which is what a `500` on this endpoint already means to the CLI.

A store error anywhere in these paths is a `500`; there is no in-memory
fallback, because two relays with different memories is the bug this
removes.

### Rate limiter

`loginLimiter` becomes a thin type over the store: `allow(ip)` computes
`rateLimitKey(ip)` (unchanged, including the IPv6 /64 mask), calls
`LoginHit`, and returns `hits <= loginLimitPerMin`. `loginLimitBurst` and
`loginLimitMaxIdle` are deleted with the bucket. A store error fails closed
(`429`): the endpoints it guards are the unauthenticated ones, and a relay
that cannot reach Postgres cannot complete a login anyway.

## Verifier changes (`verifier_github.go`)

`NewGitHubVerifier(clientID, clientSecret string, st *Store)`. The `flows`
map, its mutex, `githubFlow`, `flowGrace`, `sweepLocked`, and
`pollUntilDone` are deleted. The `sleep` seam goes with the goroutine; `now`
stays.

- `Start` — asks GitHub for a device code as today, mints the opaque handle,
  `PutDeviceFlow` with `next_poll_at = now + interval` and `expires_at = now
  + expires_in + 1 min` (the same one-minute grace the map had, so a poll
  racing the deadline still sees GitHub's own "expired" error).
- `Poll` — `DeviceFlow(handle)`; unknown ⇒ the same "unknown handle" error.
  If not `Due`, return `ErrAuthPending` without touching GitHub. Else one
  token request:
  - `authorization_pending` ⇒ `DeferDeviceFlow(handle, interval)`, pending.
  - `slow_down` ⇒ `DeferDeviceFlow(handle, interval + 5 s)`, pending.
  - access token ⇒ `GET /user` once, `DeleteDeviceFlow`, return the
    identity.
  - `expired_token`, `access_denied`, other ⇒ `DeleteDeviceFlow`, return the
    error.

GitHub's documented poll semantics are preserved exactly; what changes is
who drives the clock. The CLI already polls at `interval`, so a healthy flow
resolves in the same wall time as before. `FakeVerifier` is untouched: it is
the in-memory test double the API tests already use.

## Edge (`edge_state.go`, `edge.go`)

`pickAPI` returns the live instances in `earlier` order and indexes them
with an atomic counter, `counter % len`. Eviction and resync leave the
counter alone; a modulus over a changed slice just skips or repeats one
relay once. The "pin to one relay" comment in `handleTLS` is deleted.

## Failure handling

- **Postgres unreachable**: login endpoints return `500` (or `429` from the
  limiter); already-issued credentials keep working because
  `AuthenticateAccount` is unchanged. The edge keeps serving its cached
  cluster picture as it does today.
- **Relay dies mid-flow**: nothing is lost. The next request lands on
  another relay and finds the row.
- **Two relays race one redeem**: the guarded `DELETE`/`UPDATE` gives exactly
  one winner; the loser sees not-ok and answers as it would for a bad state.
- **Two relays poll the same device flow concurrently**: both read
  `next_poll_at` in the past and both hit GitHub. GitHub answers the second
  with `slow_down`, which defers the row. One redundant upstream call per
  race, no incorrect outcome. Not worth a lock. If the first poll instead
  got a terminal answer and already deleted the row, the second poll's
  `DeviceFlow` lookup finds nothing and returns the same "unknown handle"
  error as an expired flow.

## Testing

Test-first, in the existing files unless noted. Postgres comes from
`relaytest` (Docker or `PIPER_TEST_POSTGRES_URL`) as for every store test.

- `login_state_test.go` (new): per table, put/read; single use (second
  take is not-ok); expiry (`expires_at` in the past is invisible and
  swept); the CLI handle state machine (unconfirmed → confirmed → finished →
  taken); `LoginHit` counts within a window and resets after it.
- `api_test.go` / `weblogin_cli_test.go`: the existing flow tests run
  unchanged in behaviour. Add one cross-process test per flow: two `api`
  values over one store, start on A, finish or poll on B. For the CLI
  flow, assert the credential returned by the poll authenticates and that
  no plaintext credential appears in `login_cli_handles`.
- `verifier_github_test.go`: rewrite the device-flow tests against the
  existing fake GitHub server for the stateless shape: approved, denied,
  `slow_down` defers, expired, unknown handle, a poll before `next_poll_at`
  makes no upstream call, and the cross-relay case (start on one verifier,
  poll on a second sharing the store). The abandoned-flow pruning tests
  become expiry tests on the table.
- `login_rate_test.go`: rewrite for the window semantics. Keep the per-IP
  independence, shared-across-endpoints, IPv6 prefix, and refill tests;
  refill becomes "a new window admits again". Add: two `api` values share
  one budget.
- `edge_test.go`: `api.<apex>` through the edge alternates `X-Relay`
  between two relays; the pinned-relay assertion in the cluster test is
  updated to accept either.

## Documentation and tracking

- `PROGRESS.md`: one line under the scale-out epic, `[#522]`.
- The edge design's Non-goals and Follow-ups keep their text; this document
  is the record that the follow-up landed.
- Deploy note for the PR body: additive schema, no reset; roll relays in any
  order.
