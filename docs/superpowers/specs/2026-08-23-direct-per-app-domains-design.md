# Direct-served per-app custom domains (DNS-01 per host)

**Issue:** #506. Follow-up carved out of the direct serve mode design
(`2026-08-13-direct-serve-mode-design.md`, shipped in #508/#509).

Per-app custom domains (`blog.example.org` → the `blog` app) are today forced
through the relay: their certificate comes from a TLS-ALPN-01 challenge that
only reaches the box because the relay splices `acme-tls/1` connections down
the tunnel. On a box whose serve mode is `direct` — including a never-enrolled
box (#509) — pointing a per-app domain straight at the box lands the challenge
on Caddy's `:443`, which cannot solve it, so no cert is ever issued.

This design lets per-app domains on a direct box issue via **DNS-01, reusing
the box-wide token source**, and serve straight from the box's Caddy.
Relay-mode boxes are untouched.

## Decision summary

- **The box decides, not the domain.** Per-app domains follow the box-wide
  serve mode: the `serve` field on the API-managed domain config, or
  `PIPER_SERVE` on env-managed boxes. Never-enrolled boxes are direct by
  construction. No new `serve` column on `app_domains`, no per-domain choice —
  mixed relay/direct on one box is a YAGNI case.
- **DNS-01 reuses the box-wide token source.** API-managed boxes use the
  domain config row's provider + token; env-managed boxes use the provider's
  own env creds (the `newEnvIssuer` path). No per-domain tokens, no new
  providers.
- **No DNS wait in direct mode.** TLS-ALPN-01 required the user's DNS to point
  at the relay before issuing; DNS-01 does not depend on where visitor DNS
  points. Direct issuance starts immediately, same as the box-wide flow, and
  the user flips DNS at their own pace.
- **The relay claim becomes tolerant, mirroring box-wide `issueOnce`**: claim
  + confirm when a relay is connected (both paths serve during the DNS flip —
  the TTL-window migration property for free), skip silently when there is no
  notifier (never-enrolled). Relay mode keeps today's hard requirement.
- **Add-time guard.** Adding a per-app domain on a direct box with no DNS-01
  source (no box-wide domain config, or an env box on a static
  `PIPER_TLS_CERT_FILE`) is rejected with a clear error at `POST` time —
  nothing is stored, no lifecycle spins on an unrecoverable failure.
- **Zone mismatch surfaces at issuance.** A token that cannot edit the
  per-app domain's zone is only discoverable when lego asks Cloudflare; the
  row goes `failed` with a message naming the zone the token must cover.
- **Flip re-issues nothing.** Changing the box's serve mode leaves active
  per-app domains active — the cert is the same either way. Reporting flips
  immediately; renewal follows the mode current at renew time.

## Mechanism

### 1. Mode selection

`Manager` gains a small helper (`boxServe()`) answering "is this box direct?":
the API config row's `Serve` when one exists, else `m.envServe` for
env-managed boxes, else relay. It is consulted by `AddAppDomain`,
`appIssueOnce`, `reissueApp`, and `appDomainStatus` — one fact, one owner.

### 2. Issuance (`internal/domain/appdomain.go`)

`appIssueOnce` branches on mode:

- **relay** — unchanged: require the notifier ("relay not connected"), claim,
  gate on `dnsPointsAt(dnsTarget)` (`errWaitDNS` flat poll), ALPN obtain via
  `m.appIssuer()`.
- **direct** — obtain `[domain]` via a DNS-01 issuer for per-app domains (see
  §3). Claim + confirm only if `m.notifier() != nil`. No DNS gate. The rest of
  the flow — disk-cert reuse, `armApp` (EnsureHTTPS, cert into Caddy, route
  backfill), persist active — is identical.

`reissueApp` (renewal) selects its issuer the same way, so a domain issued
under one mode renews correctly under another after a flip.

### 3. Issuer plumbing (`cmd/piperd/main.go`)

Today `Options.AppIssuer` builds only the ALPN issuer and hard-fails when
`alpnSolver == nil` with an error citing #506. That guard gives way:

- The manager builds the direct-mode DNS-01 issuer from the box-wide token
  source: for an API-managed config, `m.newIssuer(dc.DNSProvider,
  dc.DNSToken)` — the factory already in `Options.Issuer`; for env-managed
  boxes, a new option supplies `newEnvIssuer(cfg)` (nil on static-cert boxes).
- `PIPER_TEST_ISSUER=selfsigned` short-circuits both paths, as today.
- The ALPN path and its solver wiring are untouched for relay mode.

Exact-host SAN (`[domain]`, no wildcard) stays as-is.

### 4. Add-time guard

`AddAppDomain` on a direct box checks that a DNS-01 source exists (API config
row with a token, or the env issuer option). Missing ⇒ a new sentinel
(`ErrNoDNSIssuer`, message: direct-served per-app domains need a box-wide
domain with a DNS token) which `internal/api/api.go` maps to 4xx alongside
`ErrBoxWideDomain`.

### 5. Reporting (`appDomainStatus`)

- **relay** — unchanged: `dns_records` = CNAME → `dnsTarget`, `dns_ok` =
  `cachedDNSPointsAt`.
- **direct** — `dns_records` = `A <domain> → publicIP()` (the #508 discovery:
  `observed_addr` from the tunnel handshake, `PIPER_PUBLIC_IP` override);
  `dns_ok` = cached "does the domain resolve to the box's IP"
  (`cachedDNSResolvesTo`). Unknown public IP ⇒ record value empty and
  `dns_ok` false, same posture as the box-wide status.

The TUI (`internal/tui/domaindetail.go`) and CLI (`cmd/piper/domains.go`)
render `dns_records` generically — no UI change.

### 6. Flip semantics

Flipping the box-wide `serve` (or `PIPER_SERVE`) re-issues nothing and
touches no per-app row. Status reporting follows the new mode on the next
read; renewal follows the mode at renew time. A post-flip zone mismatch
therefore surfaces at renewal as a `failed`/renew-error status — the renewal
window (~30 days before expiry) is the warning period. `renewApp`'s existing
posture (old cert keeps serving, error surfaced, row stays active) already
handles this.

## Error handling

- No DNS-01 source on a direct box: rejected at add (4xx), per §4.
- Token cannot edit the domain's zone: `Obtain` fails; row `failed` with the
  provider's error plus guidance that the box-wide token must cover the zone;
  capped-backoff retries as today.
- Relay connected but claim fails (direct mode): the claim is still load-
  bearing for the migration window, so a claim error fails the attempt and
  retries — identical to box-wide `issueOnce`.
- Public IP unknown: issuance proceeds (DNS-01 does not need it); only
  reporting degrades.

## Testing

Unit tests in `internal/domain` with the existing fakes:

- Direct mode issues via the DNS-01 issuer, never touches the ALPN issuer,
  and skips the DNS gate.
- Direct + no notifier: activation succeeds with no claim; direct + notifier:
  claim and confirm are made; claim failure fails the attempt.
- `AddAppDomain` rejects with `ErrNoDNSIssuer` on a direct box with no token
  source; accepts on a relay box regardless.
- Status: direct reports `A → publicIP` and IP-based `dns_ok`; relay reports
  the CNAME as today.
- Renewal after a flip uses the new mode's issuer both directions.

E2e: extend the direct-serve e2e (`test/e2e/direct_test.go`) — add a per-app
domain with `PIPER_TEST_ISSUER=selfsigned`, assert it activates without a
relay and serves on the box's `:443` with the exact-host cert.

## Out of scope (follow-ups)

- **Per-domain serve choice** (mixed relay/direct on one box) — add a
  per-row field only if a real need appears.
- **Per-domain DNS tokens / non-Cloudflare providers** — a separate,
  larger feature.
- **Relay awareness of direct per-app domains** (dropping unused routes,
  redirecting stragglers) — unnecessary while the claim is harmless.
