# Removing a box from a relay account

**Date:** 2026-07-26
**Status:** Approved design, pre-implementation
**Issue:** [#401](https://github.com/piperbox/piper/issues/401)

## Problem

`piper connect` refuses at the per-account agent cap with:

```
error: account agent quota exceeded; remove an existing box or upgrade
```

Neither remedy exists. There is no removal path anywhere — the control proxy
serves `GET /agents` and `/agents/<base>/v1/*` and rejects every other method
with 405 (`internal/relay/proxy.go`), `internal/relayclient` has no removal
method, and no `DELETE FROM agents` appears in the repository. There is no
upgrade path either.

Dead slots accumulate normally. Every re-enrollment after a fresh-DB rebuild
leaves the old row behind, and `piper connect` mints a new agent each time with
no way to reclaim the old one. An account that fills its cap with unreachable
boxes is permanently stuck.

Observed on the hosted relay: the `ozykhan` account held three agents with one
alive, and `EnrollForAccount` refused at `internal/relay/accounts.go`. The only
unblock was raising `PIPER_RELAY_MAX_AGENTS` on the relay host — a **relay-wide**
setting (`st.Configure`), so one stuck account lifts the cap for every tenant.
That is the wrong knob, and it is unavailable to anyone who does not operate the
relay.

## Decision summary

A `DELETE /agents/<base-domain>` on the existing control proxy, reusing the
authorization already written for the sibling GET, plus a `piper box` noun in the
CLI. Removal is refused while the box holds a live tunnel session, and deletes
only rows genuinely attributable to the agent.

## What a removal deletes — and what it cannot

Two tables key on `agents(name)` and are unambiguously the agent's
(`internal/relay/schema.sql`):

- `repo_bindings (agent_name, app)`
- `pending_events (agent_name, app, ref)`

`hostnames` is **not** one of them. It keys on `account_id`:

```sql
CREATE TABLE IF NOT EXISTS hostnames (
    hostname    TEXT PRIMARY KEY,
    account_id  TEXT NOT NULL REFERENCES accounts(id),
    app         TEXT NOT NULL,
    pr          INTEGER NOT NULL DEFAULT 0,
    created_at  TEXT NOT NULL,
    UNIQUE(account_id, app, pr)
);
```

There is no column recording which agent registered a hostname, and the relay
already depends on this being unknowable — `repushRelayApps` (`cmd/piperd/main.go`)
re-pushes hostnames *from the box* precisely because "the relay's hostnames table
is keyed by account, not agent, so it cannot safely hand one box's session the
account's whole app list."

So a removal cannot delete the box's hostname rows. Deleting the account's
hostnames would destroy the URLs of the account's *other* boxes.

**Consequence, accepted deliberately:** removing a box frees an **agent** slot but
not its **app** slots, because `RegisterHostname` caps on
`COUNT(*) FROM hostnames WHERE account_id=? AND pr=0`. This is the difference
between the two caps (`PIPER_RELAY_MAX_AGENTS` and `PIPER_RELAY_MAX_APPS`), and
only the agent cap is what #401 is about.

Two rejected alternatives:

- **Attribute via `repo_bindings`.** For apps with a binding row, the agent→app→
  hostname chain can be walked. But an app deployed by tarball has no binding, so
  the reclaim would be partial and silently so — worse than a documented gap.
- **Add `agent_name` to `hostnames`.** The correct long-term model, and it would
  also fix the latent collision below. But it is a new column on an existing
  table, which under the pre-1.x no-migrations rule does not materialize on an
  existing DB — it needs a fresh relay DB and a full fleet re-enrollment, days
  after the last one.

Both the unreclaimed app slots and the latent collision — two boxes on one
account deploying the same app name collide on `UNIQUE(account_id, app, pr)` and
share a hostname — get a follow-up issue rather than a guess here.

## Connected boxes are refused

A box holding a live tunnel session cannot be removed; the caller stops `piperd`
on it (or waits for it to drop) and retries. This mirrors `DeleteOrg`, which
refuses with `ErrOrgHasAgents` rather than orphaning what it owns, and it makes
destroying a live production box by mistyping a base domain very hard.

Rejected: evicting the session and deleting anyway. Removal is irreversible — the
enrollment token is gone and the box must re-enroll — so a one-step destructive
path guarded only by a typo-prone argument is the wrong default. A `--force`
escape hatch was considered and dropped as unneeded surface; it can be added if a
real case appears (an unreachable box that still somehow connects).

## Components

### `internal/relay` — store

```go
func (s *Store) DeleteAgent(baseDomain string) error
```

One transaction, `DeleteOrg`'s shape. Resolve `agents.name` from `base_domain`
first — both child tables reference the name, not the base domain — then delete
in FK-safe order: `pending_events`, `repo_bindings`, `agents`.

A new sentinel `ErrUnknownAgent` is added for the `sql.ErrNoRows` case. The
existing sentinels do not fit: `ErrUnknownAccount` names a missing *account*, and
`AgentAccount` returns `ErrBadToken` for an unknown base domain, which is the
right answer on an authentication path but misleading on an explicit delete.

The store does **not** check whether the agent is connected. Liveness lives in the
router's in-memory session map; the store knows persistence only, and nothing
imports "up".

### `internal/relay` — HTTP

`DELETE /agents/<base-domain>`, in the control proxy's `tail == ""` branch that
currently answers only GET.

The authorization is already written for the sibling GET and is reused unchanged:
`AgentAccount(base)` then `CanControl(acc.ID, ownerID)`, with both unknown and
foreign agents returning **404** so existence never leaks across tenants.

| Condition | Status |
| --- | --- |
| Unknown or foreign agent | 404 |
| Agent holds a live session (`router.Lookup`) | 409 |
| Removed | 204 |
| Any other method on this path | 405 (unchanged) |

The 409 body names the fix: the box is connected, stop `piperd` on it first. The
removal is logged relay-side with the base domain.

### `internal/relayclient`

Two methods; neither exists today.

```go
func (c *Client) Agents(ctx context.Context, accountCredential string) ([]Agent, error)
func (c *Client) RemoveAgent(ctx context.Context, accountCredential, baseDomain string) error
```

`Agent` carries the four fields the bare `/agents` listing already returns:
`agent` (base domain), `name`, `owner`, `connected`.

### `cmd/piper` — the `box` noun

Singular, matching `piper app`.

- **`piper box ls`** — base domain, name, owner, connected. There is no CLI way to
  see an account's boxes today; finding the dead slots on the hosted relay
  required a hand-written script against `/agents`. `rm` is close to unusable
  without it, since the base domain is the argument.
- **`piper box rm <base-domain> --yes`** — irreversible, so it requires `--yes`,
  matching `piper github reset [--yes]`.

Both need `RelayAPI` + `AccountCredential` from the client config and fail with
the standard "not logged in to a relay; run `piper login` first" without them.

### The quota error

`EnrollForAccount`'s message currently names a remedy that does not exist. It
becomes actionable:

```
error: account agent quota exceeded; run `piper box ls` to see your boxes
and `piper box rm <box>` to free a slot
```

The "or upgrade" clause is dropped — there is still no upgrade path, and naming
one that does not exist is what made the original message misleading.

## Interaction with #400

After a removal the box keeps its now-invalid `relay.json` and keeps retrying.
Since #400 the relay acks the handshake, so the box reports the real reason
instead of claiming success:

```
tunnel: handshake: relay rejected <base-domain>: unknown or revoked enrollment (retry in 30s)
```

Before #400 a removed box would have logged `tunnel: connected` forever. Removal
would have created exactly the invisible-stranding failure #400 fixed, so these
two land in the right order.

## Testing

**Store.** The cascade deletes `agents`, `repo_bindings`, and `pending_events`
rows for the agent; an unknown base domain returns `ErrUnknownAgent`; another
agent's rows are untouched. One test asserts explicitly that **hostnames survive
a removal** — that is the documented limit above, and pinning it keeps a future
change from quietly turning it into cross-box data loss.

**Handler.** 409 while a session is registered in the router; 404 for an agent
owned by another account; 204 with the row gone; 405 preserved for other methods
on the path.

**CLI.** `box ls` renders the listing including the connected column; `box rm`
refuses without `--yes` and makes no request; the not-logged-in path.

## Deployment

Relay and CLI. **No schema change** — the migration-shaped risk is absent because
this only adds `DELETE`s — so no fresh DB and no re-enrollment. Unlike v0.10.0
there is no wire-format change, so relay and boxes need not move together; the
relay can be upgraded alone and boxes pick up `piper box` whenever they update.

## Out of scope

- Reclaiming app-cap slots on removal (the hostname attribution gap above).
- The same-app-name hostname collision between two boxes on one account.
- Any upgrade/paid-tier path for the agent cap.
- Reverting `PIPER_RELAY_MAX_AGENTS=10` on the hosted relay — an operator task
  once the dead slots there are reclaimed, not a code change.
