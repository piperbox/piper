# Control API

piperd's HTTP control plane: every route the `piper` CLI, the TUI, and the dashboard use, with request and response shapes and the error statuses each handler returns. `test/docs` fails when a route is registered without a heading here.

All responses are JSON (`Content-Type: application/json`) except deployment logs, which are plain text. Errors are plain-text bodies with the status codes listed per route; a `500` carries only `internal server error`, the detail goes to piperd's log. Every route may also answer `500` on a store failure, so it is not repeated below.

## Authentication

piperd listens on `PIPER_API_ADDR` (default `127.0.0.1:8088`). A loopback bind serves without credentials: whoever can reach the socket owns the box. Any other bind (`0.0.0.0:8088` for LAN use) wraps every route in a bearer check, and a missing, malformed, or revoked token answers `401`. Create a token on the box with `piperd token create` (with `sudo` on a systemd install) and pass it as `Authorization: Bearer <token>`; `piper login --token` stores it for the CLI.

Through the relay the same routes are reachable at `https://api.<apex>/agents/<base-domain>/v1/...`, authenticated with the account credential from `piper login`; the relay swaps it for the box's own token and forwards the request over the tunnel.

## Types

**App** — the stored app plus the daemon's view of it:

```json
{
  "Name": "blog",
  "Port": 8080,
  "Repo": "octocat/blog",
  "Branch": "main",
  "RootDir": "",
  "Hostname": "blog.piper.localhost",
  "CreatedAt": "2026-09-11T10:00:00Z",
  "Status": "running",
  "Scheme": "http"
}
```

`Status` is exactly one of `building`, `running`, `failed`, `stopped`, or `""` when never deployed; a `failed` latest deployment still reports `running` while an older deployment is up. `Scheme` is `http` or `https`, whichever the public reaches this box's apps on. `Hostname` is `""` until the first deploy.

**Deployment:**

```json
{
  "ID": "d_01J...",
  "App": "blog",
  "PR": 0,
  "ImageID": "sha256:...",
  "ContainerID": "...",
  "HostPort": 32768,
  "Status": "running",
  "Hostname": "",
  "CreatedAt": "2026-09-11T10:00:00Z"
}
```

`PR` is non-zero and `Hostname` set for a pull-request preview; production's hostname lives on the app.

**AppDomainStatus** — one per-app custom domain:

```json
{
  "domain": "shop.example.com",
  "app": "blog",
  "status": "active",
  "error": "",
  "cert_not_after": "2026-12-10T10:00:00Z",
  "dns_records": [{ "type": "CNAME", "name": "shop.example.com", "value": "blog.abc123.public.getpiper.dev" }],
  "dns_ok": true,
  "note": ""
}
```

`status` is `pending`, `issuing`, `active`, or `failed`; `cert_not_after` and `note` are omitted when empty.

**Status** — the box-wide custom domain:

```json
{
  "domain": "example.com",
  "dns_provider": "cloudflare",
  "dns_token_set": true,
  "source": "api",
  "serve": "relay",
  "status": "active",
  "error": "",
  "note": "",
  "cert_not_after": "2026-12-10T10:00:00Z",
  "dns_records": [{ "type": "A", "name": "example.com", "value": "203.0.113.7" }],
  "dns_ok": true
}
```

`source` is `api` or `env` (`PIPER_BASE_DOMAIN` set); `serve` is `relay` or `direct`; `status` is `""`, `issuing`, `active`, or `failed`. The DNS token itself is never returned. `note` is omitted when empty; when set (a direct-served box before its public IP is known) it explains why the domain isn't reachable yet.

## Daemon

### GET /v1/version

The running daemon's build and the listen configuration it loaded, so a client can tell a half-applied upgrade from a fix that did not work.

Response `200`: `{"version": "v0.23.1", "http_addr": ":80", "https_addr": ":443", "data_dir": "/var/lib/piper"}`.

## Apps

### POST /v1/apps

Create an app. Body: `{"name": "blog", "port": 8080}`; `port` defaults to 8080 when 0 or absent.

Response `201`: App. Errors: `400` invalid body, name not a DNS label (lowercase letters, digits, hyphens, 1 to 63 characters, no leading or trailing hyphen), or the reserved name `hooks`; `409` app exists.

### GET /v1/apps

Every app. Response `200`: `[App, ...]` (an empty array when there are none).

### GET /v1/apps/{name}

One app. Response `200`: App. Errors: `404` not found.

### DELETE /v1/apps/{name}

Delete the app, its deployments, and its per-app domains; on a box with a domain manager each domain is released first and a release failure aborts the delete, so it stays retryable. Response `204`. Errors: `404` unknown app.

### POST /v1/apps/{name}/stop

Stop the production container, drop its routes, and mark the deployment `stopped`; a no-op when nothing is running. Response `204`. Errors: `404` unknown app.

### POST /v1/apps/{name}/start

Run the latest deployment's image again and restore its routes; a no-op unless the latest deployment is `stopped`. Response `204`. Errors: `404` unknown app.

### POST /v1/apps/{name}/link

Link the app to a GitHub repository. Body: `{"repo": "owner/name", "branch": "main", "root_dir": "apps/web"}`; `root_dir` is optional and must be a relative path inside the repository. When a relay connection exists the binding is also registered there so brokered webhooks route to this box; a relay that is briefly unreachable does not fail the request.

Response `204`. Errors: `400` invalid body, `repo` not `owner/name`, or `root_dir` absolute or escaping the repository; `404` unknown app.

## Deployments

### POST /v1/apps/{name}/deploy

Deploy from a tarball. The body is a tar stream of the source directory containing a Dockerfile. The build runs after the response; poll the deployment's status and logs.

Response `202`: Deployment with `Status` `building`. Errors: `400` bad tar (including an entry that escapes the destination); `404` unknown app.

### POST /v1/apps/{name}/deploy-from-repo

Deploy a linked app from its repository's tracked branch. No body. The fetch happens before the response, the build after it.

Response `202`: Deployment. Errors: `404` unknown app; `409` app not linked, or no GitHub App configured (`run piper github setup first`); `502` the repository fetch failed.

### GET /v1/apps/{name}/deployments

Every deployment of the app, newest first. Response `200`: `[Deployment, ...]` (an empty array when there are none). Errors: `404` unknown app.

### GET /v1/apps/{name}/deployments/{id}/logs

The deployment's build and run log. Response `200`: `text/plain; charset=utf-8`. Errors: `404` unknown deployment.

## GitHub App

The box's own GitHub App, for boxes not using a relay-brokered one.

### POST /v1/github/manifest

Build the GitHub App manifest `piper github setup` posts to GitHub. Body: `{"redirect_url": "http://127.0.0.1:PORT/cb"}`.

Response `200`: `{"manifest": "<JSON string>"}`. Errors: `400` invalid body or empty `redirect_url`.

### POST /v1/github/exchange

Exchange the code GitHub redirected back with for App credentials, store them, and start serving webhooks without a restart. Body: `{"code": "..."}`.

Response `200`: `{"slug": "piper-abc123"}`. Errors: `400` invalid body; `502` GitHub rejected the exchange.

### GET /v1/github

Whether a GitHub App is configured on this box. Response `200`: `{"configured": false}` or `{"configured": true, "app_id": 12345, "slug": "piper-abc123"}`.

### DELETE /v1/github/app

Drop the stored App. The running webhook listener keeps the old credentials until piperd restarts; the response names the provider a restart will pick.

Response `200`: `{"provider": "brokered" | "none" | "unknown"}` (or another provider name).

## Box-wide domain

The box's own domain, `PIPER_BASE_DOMAIN` or one set here. All three routes answer `409` on a LAN-only box: `domain config requires a relay connection, or direct serve (PIPER_BASE_DOMAIN with PIPER_SERVE=direct)`.

### GET /v1/domain

Response `200`: Status.

### PUT /v1/domain

Set or replace the domain. Body: `{"domain": "example.com", "dns_provider": "cloudflare", "dns_token": "...", "serve": "relay"}`; `serve` is `relay` or `direct`.

Response `200`: Status. Errors: `400` invalid body, `invalid domain`, `unsupported dns provider`, `dns_token required`, or invalid `serve`; `409` the domain is env-managed (`PIPER_BASE_DOMAIN` is set) or the box is LAN-only.

### DELETE /v1/domain

Remove the API-managed domain. Response `204`. Errors: `409` env-managed or LAN-only.

## Per-app domains

Custom domains attached to one app. All three routes answer `409` on a LAN-only box, as above.

### GET /v1/apps/{name}/domains

Response `200`: `[AppDomainStatus, ...]` (an empty array when there are none). Errors: `404` unknown app.

### POST /v1/apps/{name}/domains

Attach a domain. Body: `{"domain": "shop.example.com"}`. The domain is stored lowercase.

Response `201`: AppDomainStatus, whose `dns_records` say what to create. Errors: `400` invalid body or invalid domain; `404` unknown app; `409` the domain is the box-wide domain, is already attached to an app, or, on a direct-served box, there is no box-wide domain with a DNS token to issue its certificate with.

### DELETE /v1/apps/{name}/domains/{domain}

Detach the domain and release its certificate. Response `204`. Errors: `404` unknown app, or the domain is not attached to this app.

## App environment

Per-app variables applied on the next deploy or restart.

### GET /v1/apps/{name}/env

Response `200`: `{"env": {"KEY": "value"}, "updated_at": {"KEY": "2026-09-11T10:00:00.123456789Z"}}`. Errors: `404` unknown app.

### POST /v1/apps/{name}/env

Set one variable. Body: `{"key": "DATABASE_URL", "value": "..."}`. Keys match `[A-Za-z_][A-Za-z0-9_]*`.

Response `204`. Errors: `400` invalid body, invalid key, or the reserved key `PORT`; `404` unknown app.

### DELETE /v1/apps/{name}/env/{key}

Remove one variable. Response `204` (also when the key was not set). Errors: `404` unknown app.
