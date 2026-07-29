# App environment variables — design

**Date:** 2026-07-29
**Status:** approved design, pre-implementation
**Motivating case:** the dashboard app deployed on a box cannot log in because a
required environment variable is not set on its container. Piper has no way to
set one.

## Goal

Per-app environment variables with Vercel-shaped semantics: saved on the box,
injected into the app's container at run time, applied on the next deploy or
restart (never by surprise), manageable from the CLI and the HTTP API (which
the dashboard consumes through the relay control proxy).

## Non-goals (v1)

- **Build-time injection** (`--build-arg`): server-side/runtime vars only.
  Client-side framework vars (`VITE_*`, `NEXT_PUBLIC_*`) need build args and
  Dockerfile `ARG` cooperation — out of scope until a real app needs it.
- **Per-environment values** (production vs PR preview): one env set per app;
  previews inherit it.
- **Encryption at rest**: values sit in the box's SQLite at the same trust
  tier as the DNS token and GitHub App private key already stored there.
- **TUI editor**: follow-up issue.
- **Bulk `.env` import/export.**
- **Auto-restart on change**: rejected in design discussion in favor of
  apply-on-next-deploy.

## Design

### Schema & store (`internal/store`)

```sql
CREATE TABLE IF NOT EXISTS app_env (
    app   TEXT NOT NULL REFERENCES apps(name),
    key   TEXT NOT NULL,
    value TEXT NOT NULL,
    PRIMARY KEY (app, key)
);
```

Pre-1.x rules apply: `schema.sql` is edited in place, no migration.

Store methods:

- `SetAppEnv(app, key, value string) error` — upsert.
- `DeleteAppEnv(app, key string) error` — idempotent.
- `AppEnv(app string) (map[string]string, error)` — empty map when none.
- `DeleteApp` also deletes the app's `app_env` rows, alongside the app's
  other child rows.

### Injection (`internal/deploy`)

Both `runtime.Run` call sites — the fresh deploy and the stop/start restart —
build the container env as: the app's stored env, then `PORT` overwritten on
top. `PORT` is reserved (rejected at write time), so the overwrite is a
belt-and-braces invariant, not a merge policy.

A store read failure **fails the deploy** rather than running the container
without its configured env: a half-configured app is worse than a failed
deploy.

PR preview deployments run through the same call site and inherit the app's
env unchanged.

Apply semantics: changing env never touches a running container. The next
deploy (API, git push, or TUI) or stop/start picks it up.

### API (`internal/api`)

Mirrors the per-app domains endpoints' shape:

- `GET /v1/apps/{name}/env` → `200 {"env": {"KEY": "value", ...}}`.
  Values are returned in full: the API is admin-bearer-authed and
  single-tenant, and the dashboard needs read-back to edit.
- `POST /v1/apps/{name}/env` body `{"key": "...", "value": "..."}` → upsert.
  `400` when the key does not match `^[A-Za-z_][A-Za-z0-9_]*$` or equals
  `PORT` (case-insensitive); `404` unknown app.
- `DELETE /v1/apps/{name}/env/{key}` → `204`, idempotent.

No relay changes: the dashboard reaches these through the existing
`/agents/<base-domain>/v1/...` control proxy.

### CLI (`cmd/piper`, `internal/client`)

- `piper env <app> set KEY=VALUE [KEY2=VALUE2 ...]` — loops the upsert
  endpoint; prints `saved — applies on next deploy or restart`.
- `piper env <app> ls` — keys with masked values; `--show` reveals.
- `piper env <app> rm KEY`.

Client methods in `internal/client`; `--remote <base-domain>` works
unchanged since it only swaps the client base URL.

### Security posture

Values are plaintext in the box's SQLite (root-only 0700 state dir under the
shipped systemd unit) — the same tier as `domain_config.dns_token` and
`github_app.private_key`. They transit only the authed control API: loopback
locally, bearer-authed relay tunnel remotely. Masking in `env ls` is a display
default, not a security boundary.

## Testing

Test-first throughout, at existing seams:

- **store**: set/overwrite/list/delete round-trip; rows gone after
  `DeleteApp`.
- **deploy**: fake runtime asserts the container env contains stored vars
  plus `PORT`; `PORT` cannot be shadowed by a stored var.
- **api**: CRUD round-trip; `404` unknown app; `400` invalid key; `400`
  `PORT`.
- **cli**: `KEY=VALUE` parsing, masked `ls` output, `--show`.

No new e2e: the merge is covered at unit seams and the existing deploy e2e
stays green.

## Layering check

`store` gains persistence only; `deploy` reads env through its existing store
interface; `api` is transport over both; `client`/CLI sit above `api`. Nothing
imports up.
