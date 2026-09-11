# Environment variables

Every `PIPER_*` variable each binary reads, with its default and what it does. `test/docs` fails when a binary reads a name this page does not list.

Unset means the default applies. Booleans are enabled by the exact value `1` unless a row says otherwise.

## piperd

Read by `internal/config` and `cmd/piperd`. The systemd unit sets `PIPER_DATA_DIR` to `/var/lib/piper` and reads the rest from `/etc/piper/piperd.env`; [`packaging/systemd/piperd.env.example`](../../packaging/systemd/piperd.env.example) is the curated subset most installs touch. Values marked *relay.json* also come from the enrollment file `piper login` writes; the variable, when set, overrides it and marks the box operator-managed, so `piper login` refuses to enroll it.

| Name | Default | Meaning |
| --- | --- | --- |
| `PIPER_DATA_DIR` | `~/.piper/piperd` (`/var/lib/piper` under systemd) | Directory for the SQLite database, `relay.json`, and certificates. |
| `PIPER_API_ADDR` | `127.0.0.1:8088` | Control API listen address. A loopback bind serves without a token; any other bind requires a bearer token (see [Control API](api.md#authentication)). `0.0.0.0:8088` opens it to the LAN. |
| `PIPER_WEBHOOK_ADDR` | `127.0.0.1:8089` | Loopback listener for GitHub webhooks arriving through the relay. |
| `PIPER_BASE_DOMAIN` | `piper.localhost`, or the enrolled relay's (*relay.json*) | Apps are served at `<name>.<base>`. Setting it makes the box-wide domain env-managed: `PUT`/`DELETE /v1/domain` answer 409. |
| `PIPER_CADDY_ADMIN` | `http://127.0.0.1:2019` | Admin API of the Caddy piperd manages. |
| `PIPER_HTTP_ADDR` | `:80` | Caddy's HTTP listen address. |
| `PIPER_HTTPS_ADDR` | `:443` | Caddy's HTTPS listen address. |
| `PIPER_RELAY_ADDR` | unset (*relay.json*) | Relay tunnel endpoint, `host:port`. Unset and no `relay.json`: the box is LAN-only. |
| `PIPER_RELAY_TOKEN` | unset (*relay.json*) | Enrollment token presented to the relay. |
| `PIPER_RELAY_TERMINATED` | unset (*relay.json*) | `1`: the relay terminates TLS for the shared domain; the box serves `:80` and holds no certificate. |
| `PIPER_WEBHOOK_SECRET` | unset (*relay.json*) | HMAC key the relay signs brokered GitHub deliveries with. |
| `PIPER_GITHUB_BROKERED` | unset (*relay.json*) | `1`: the relay holds the GitHub App, so this box needs no App credentials of its own. |
| `PIPER_GITHUB_API_BASE` | `https://api.github.com` | GitHub API base URL override, for tests against a fake GitHub. |
| `PIPER_ACME_EMAIL` | unset | ACME account email for public certificates. |
| `PIPER_ACME_CA` | Let's Encrypt production | ACME directory URL; set the staging URL while testing. |
| `PIPER_DNS_PROVIDER` | unset | DNS-01 provider name for lego, for example `cloudflare`. |
| `PIPER_TLS_CERT_FILE` | unset | Static certificate path; with the key, skips ACME (tests, bring-your-own cert). |
| `PIPER_TLS_KEY_FILE` | unset | Static key path, paired with `PIPER_TLS_CERT_FILE`. |
| `PIPER_PUBLIC_IP` | learned from the relay | Public IP the direct-serve DNS guidance names; needed on a never-enrolled box or behind split-horizon NAT. Ignored with a log line when not an IP. |
| `PIPER_SERVE` | `relay` | `direct` serves `PIPER_BASE_DOMAIN` from this box's own `:443` instead of through the relay; see [Direct serve](../guides/direct-serve.md). Any other value is ignored with a log line. |
| `PIPER_SKIP_CADDY` | unset | Any value: do not start or manage a Caddy, because one is already running (the e2e suite). |
| `PIPER_TEST_ISSUER` | unset | **Test only.** `selfsigned` replaces ACME with a self-signed issuer so the e2e suite can serve TLS without real DNS. Never set it on a real box. |

## piper-relay

Read by `cmd/piper-relay`. See [Run your own relay](../self-host/relay.md#configure) for a working configuration.

| Name | Default | Meaning |
| --- | --- | --- |
| `PIPER_RELAY_DB_URL` | required | Postgres DSN, `postgres://user:password@host/dbname`. The relay creates its tables on first start. |
| `PIPER_RELAY_APEX` | `public.getpiper.dev` | The shared domain boxes get subdomains of. |
| `PIPER_RELAY_TLS_ADDR` | `:443` | Public TLS listener (SNI passthrough and shared-domain termination). |
| `PIPER_RELAY_HTTP_ADDR` | `:80` | Public HTTP listener. |
| `PIPER_RELAY_TUNNEL_ADDR` | `:7000` | Listener agents dial for their tunnels. |
| `PIPER_RELAY_API_ADDR` | `:8080` | Account and control API listener (`api.<apex>`). |
| `PIPER_RELAY_TUNNEL_PUBLIC` | unset | Tunnel endpoint handed to boxes at enrollment (`tunnel_endpoint` in the enroll response), for when the dialable address differs from the bind. |
| `PIPER_RELAY_ADVERTISE_HOST` | first non-loopback IPv4 | Host this instance registers in the pool for the edge and other relays to reach it. |
| `PIPER_RELAY_ZONE` | unset | Failure zone label; an agent's two tunnel sessions prefer relays in different zones. |
| `PIPER_RELAY_MAX_AGENTS` | `3` | Boxes per account. |
| `PIPER_RELAY_MAX_APPS` | `10` | Shared-domain app hostnames per account. |
| `PIPER_RELAY_MAX_DOMAINS` | `5` | Custom domains per account. |
| `PIPER_RELAY_TLS_CERT` | unset | Wildcard certificate for shared-domain termination; unset with the key means passthrough only. Re-read when the file changes. |
| `PIPER_RELAY_TLS_KEY` | unset | Key paired with `PIPER_RELAY_TLS_CERT`. |
| `PIPER_RELAY_PROXY_PROTOCOL` | unset | `1`: `:443`, `:80`, and `:7000` require a PROXY protocol v2 header. Set only when a trusted L4 proxy or `piper-edge` is the sole path to those ports. |
| `PIPER_RELAY_GITHUB_CLIENT_ID` | unset | GitHub OAuth client ID for self-service `piper login`. Unset disables self-service login; boxes can only be operator-enrolled. |
| `PIPER_RELAY_GITHUB_CLIENT_SECRET` | unset | Client secret paired with the ID; also required for browser login. |
| `PIPER_RELAY_WEB_REDIRECTS` | unset | Comma-separated `https://host/path-prefix` allowlist of browser-login redirect URIs (the dashboard). Empty disables browser login. |
| `PIPER_RELAY_GITHUB_APP_ID` | unset | Relay-held GitHub App ID; with the key, the relay brokers webhooks and tokens for every box. |
| `PIPER_RELAY_GITHUB_APP_KEY` | unset | Path to that App's private key PEM. |
| `PIPER_RELAY_GITHUB_APP_SLUG` | unset | That App's slug, so boxes can deep-link its install page. |
| `PIPER_RELAY_GITHUB_WEBHOOK_SECRET` | unset | That App's webhook secret. |
| `PIPER_RELAY_OPS_ADDR` | `127.0.0.1:9090` | Ops listener bind; binds when set or when either toggle below is on. Serves `/readyz` and `/livez` whenever bound. |
| `PIPER_RELAY_METRICS` | unset | `1`: serve Prometheus metrics on the ops listener. |
| `PIPER_RELAY_LOGS` | unset | `1`: serve the recent-log ring on the ops listener. |
| `PIPER_RELAY_FAKE_APPROVE` | unset | **Test only.** `1` auto-approves device login when no GitHub client ID is set. Never set it on a public relay. |

## piper-edge

Read by `cmd/piper-edge`. The edge shares the relays' database and fronts their public ports.

| Name | Default | Meaning |
| --- | --- | --- |
| `PIPER_EDGE_APEX` | required | The relays' apex, for example `public.getpiper.dev`. |
| `PIPER_EDGE_DB_URL` | required | The relays' Postgres DSN. |
| `PIPER_EDGE_TLS_ADDR` | `:443` | Public TLS listener. |
| `PIPER_EDGE_HTTP_ADDR` | `:80` | Public HTTP listener. |
| `PIPER_EDGE_TUNNEL_ADDR` | `:7000` | Tunnel listener agents dial. |
| `PIPER_EDGE_PROXY_PROTOCOL` | unset | `1`: the three public listeners require a PROXY protocol v2 header from a trusted balancer in front. |
| `PIPER_EDGE_OPS_ADDR` | `127.0.0.1:9090` | Ops listener bind, same rules as the relay's. |
| `PIPER_EDGE_METRICS` | unset | `1`: serve metrics on the ops listener. |
| `PIPER_EDGE_LOGS` | unset | `1`: serve the recent-log ring on the ops listener. |

## piper CLI

Read by `cmd/piper` and the TUI. The CLI also reads `PIPER_API_ADDR`, `PIPER_HTTP_ADDR`, `PIPER_HTTPS_ADDR`, and `PIPER_DATA_DIR` from the running daemon's environment for `piper agent status`; those are piperd's variables, listed above.

| Name | Default | Meaning |
| --- | --- | --- |
| `PIPER_ADDR` | the saved box's address, else `http://127.0.0.1:8088` | piperd control API to talk to; overrides the saved box. |
| `PIPER_TOKEN` | the saved box's token | Bearer token for that API; overrides the saved box. |
| `PIPER_REMOTE` | unset | Default for `--remote`: base domain of a relay-connected box to drive through the relay. |
| `PIPER_NO_BROWSER` | unset | `1`: never open a browser (`piper login --web`, `piper github setup`, the TUI); print the URL instead. |
