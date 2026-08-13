# Design: Direct serve mode for custom domains

Lets a relay-enrolled box serve a BYO custom domain's traffic **directly** —
DNS A record pointing at the box, TLS terminated by the box's own Caddy — while
the relay keeps everything it is uniquely needed for: `piper login`, control
streams for remote `piper box`, brokered GitHub webhooks, and the free-tier
`<box>.public.getpiper.dev` URLs. Direct mode is a **data-plane property of the
domain**, not a box-wide "no relay" branch; the relay stays in the picture.

## Problem

Today every byte of public app traffic flows through the relay, even when the
box has a public IP (a VPS, or a home box with port-forwarding):

- **Double bandwidth + latency.** The relay receives each request and splices
  it down the tunnel; for a box that is itself publicly reachable this is pure
  overhead, paid on the hosted relay's metered egress.
- **Data-plane SPOF.** A relay restart or outage takes user frontends down even
  though the box could have answered directly.

The machinery to serve directly already exists. In the BYO custom-domain path
the box **already terminates TLS itself**: `EnsureHTTPS` binds `:443` on all
interfaces (`internal/caddy/client.go:90`), the wildcard cert is issued by the
box via DNS-01 and loaded into Caddy's `load_pem` set, and routes are plain
host-matchers. The relay's role for these domains is an SNI splicer in front.
Pointing an A record at the box would very likely serve correctly *today* —
but it would be accidental: the DNS guidance says "CNAME → relay"
(`internal/domain/domain.go:693`), `dns_ok` checks resolution against the
relay, and nothing documents or tests the topology. This design makes it
supported.

## Decision summary

- **A `serve` field on the box-wide domain config**: `"relay"` (default,
  today's behavior) or `"direct"`. Straight `schema.sql` edit per the pre-1.0
  policy — no migration, no compat shim.
- **Direct changes only reporting**: DNS record guidance (A/AAAA → box public
  IP instead of CNAME → relay) and the `dns_ok` check (compares against the
  box, not the relay). Nothing else.
- **Cert issuance is untouched.** DNS-01 Cloudflare wildcard (API path) or
  static cert files (env path) work identically regardless of where traffic
  flows. Issuance, renewal, restart-resume: zero changes.
- **Caddy is untouched.** The existing `EnsureHTTPS` + `UpsertRouteTLS` +
  `:80→https` redirect already serve the domain publicly.
- **The relay claim stays.** `AddCustomDomain`/`ConfirmCustomDomain` are still
  made for direct domains. While the user's DNS still points at the relay,
  both paths serve the same content with the same cert — the DNS flip is the
  only cutover action, gradual and reversible. (TTL-window migration for free.)
- **Public IP discovery via the tunnel handshake**: the relay tells the box the
  source address it sees (`observed_addr` in the handshake ack — tiny wire
  change, pre-1.0 break-freely). `PIPER_PUBLIC_IP` overrides.
- **Per-app custom domains stay relay-served** in this design (see Out of
  scope): their TLS-ALPN-01 issuance depends on the relay splicing
  `acme-tls/1` to the box's solver.
- **Never-enrolled direct boxes** (HTTPS with no relay config at all) are a
  separable follow-up, not part of this change.

## Mechanism

### 1. The `serve` field

`domain_config` (`internal/store/schema.sql:38`) gains
`serve TEXT NOT NULL DEFAULT 'relay'` (values `relay` | `direct`; enforced in
`store.SetDomainConfig`). Surfaces:

- **API**: `PUT /v1/domain` (`internal/api/api.go:510`) accepts
  `"serve": "direct"`; `GET /v1/domain` reports it. Changing only `serve` on
  an active config must **not** re-issue: the cert is the same either way, so
  the handler updates the row and refreshes status without bumping the
  issuance generation.
- **No TUI/CLI surface in this change**: the TUI domain views
  (`internal/tui/domainform.go`, `domaindetail.go`) cover *per-app* domains
  only; the box-wide domain has no TUI/CLI client today (the e2e drives the
  API raw). `serve` lands in the API and store; any future box-wide domain UX
  inherits it.
- **Env-managed shape** (`PIPER_BASE_DOMAIN` boxes, where API writes are 409
  `ErrEnvManaged`): `PIPER_SERVE=direct` in the environment is the equivalent
  pin. Same semantics: reporting only.

### 2. DNS guidance and `dns_ok`

`domain.Manager` currently bakes one `dnsTarget` at construction
(`internal/domain/domain.go:193`). It becomes mode-aware:

- `serve: relay` → unchanged: `CNAME *.<domain> → <dnsTarget>`, `dns_ok` =
  domain resolves to the same IPs as the relay.
- `serve: direct` → records are `A` (or `AAAA`, matching the discovered
  address family) for `<domain>` and `*.<domain>` pointing at the box's public
  IP; `dns_ok` = `piper-probe.<domain>` resolves to that IP.
- Public IP unknown (never connected, no override): records render with an
  empty value and the status carries a human-readable note ("box public IP
  not yet known — connect to the relay once or set PIPER_PUBLIC_IP");
  `dns_ok` stays false. Honest failure, no guessing.

A CGNAT box whose user flips to direct simply sees `dns_ok: false` forever —
correct, and the note plus docs say why.

### 3. Public IP discovery

The tunnel handshake ack (`internal/tunnel/tunnel.go:106`, today just
`{error}`) gains `observed_addr` — the remote address the relay accepted the
tunnel connection from, host part only. The agent's tunnel client surfaces the
last-seen value; `domain.Manager` reads it through a small getter injected via
`domain.Options` (nil-tolerant, like `RelayNotifier`). Precedence:
`PIPER_PUBLIC_IP` env > relay-observed > unknown.

The observed address is advisory (it can be a NAT egress that doesn't accept
inbound). `dns_ok` remains the truth signal: guidance may start slightly
wrong, but the check only goes green when the user's DNS actually points at
an address the relay can see the box behind — and the docs tell port-forward
users to verify with a real request.

### 4. What deliberately does not change

- **Issuance/renewal/resume** (`internal/domain/domain.go`, `lifecycle.go`):
  DNS-01 never touches the traffic path.
- **Caddy config** (`internal/caddy/client.go`): `EnsureHTTPS`,
  `UpsertRouteTLS`, redirect pairing — all already public-facing.
- **Deploy routing** (`internal/deploy/deploy.go:304`): custom-domain routes
  are armed the same way whichever path delivers the bytes.
- **Relay claim lifecycle** (`AddCustomDomain` → `ConfirmCustomDomain` →
  `RemoveCustomDomain`): kept verbatim, for the migration property above and
  so `Remove` semantics stay identical.
- **Webhooks, login, control streams, shared-domain URLs**: relay, unchanged.

## Testing

- **Store**: `serve` round-trips; invalid values rejected; default `relay`.
- **API**: PUT with `serve: direct` persists and reports; serve-only change on
  an active config does not re-enter issuance (status stays `active`, no new
  Obtain on the fake issuer).
- **Domain manager (fakes)**: `dnsRecords`/`dns_ok` switch shape per mode;
  unknown-IP direct mode reports the empty-value records + note; precedence of
  `PIPER_PUBLIC_IP` over observed address.
- **Tunnel (fake relay)**: handshake ack carries `observed_addr`; the client
  exposes the last-seen value; reconnect updates it.
- **E2E** (self-signed issuer seam, in-process relay): enroll, set a direct
  domain, deploy an app, then TLS-dial the box's `:443` directly (SNI
  `<app>.<domain>`) — a path that by construction never touches the relay —
  and assert the same request through the relay's public port still serves
  (migration property).

## Out of scope (follow-up issues)

- **Per-app custom domains served direct.** TLS-ALPN-01 challenges must reach
  the box's solver via the relay splice; direct DNS would land `acme-tls/1`
  on Caddy's `:443`, which cannot solve them. Direct per-app domains need
  DNS-01 per host (reusing the box-wide token) — its own design.
- **Never-enrolled direct boxes.** Serving HTTPS with no relay config requires
  hoisting the domain manager, `WithHTTPS`, and cert bootstrap out of the
  `cfg.RelayAddr != ""` gates in `cmd/piperd/main.go:506-724`, plus a URL
  scheme signal for the TUI (`internal/tui/render.go:13`). Same destination,
  separable diff.
- **Relay awareness of direct domains** (e.g. dropping the unused route, or
  redirecting stragglers). Unnecessary while the claim is harmless.
