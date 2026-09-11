# Custom domains

Serve every app under a domain you own with one wildcard cert, or attach one
domain to one app with a single CNAME. TLS ends on the box either way; the
relay only splices bytes by SNI.

Two kinds:

- a **box-wide base domain** — every app served as `<app>.<yourdomain>` under
  one wildcard cert (needs a DNS-provider API token for DNS-01);
- **per-app domains** — `myshop.com` pointed at one specific app; tokenless on
  a relay-served box (see [Per-app domains](#per-app-domains-piper-domains)
  below).

## Box-wide base domain

A box serves apps on a base domain. Two ways to configure it:

### Via the control API (dashboard / `curl`) — relay free-tier boxes

    PUT /v1/domain          {"domain":"example.com","dns_provider":"cloudflare","dns_token":"<token>","serve":"relay"}
    GET /v1/domain          → status, DNS records to create, dns_ok, cert_not_after, serve
    DELETE /v1/domain       → remove the custom domain

The box issues a wildcard cert via ACME DNS-01 (Let's Encrypt) using the
Cloudflare API token, terminates TLS itself, and asks the relay to splice
`*.example.com` SNI down its tunnel. Your existing shared-domain URLs
(`<hash>-<user>.<apex>`) keep working alongside.

Create the DNS records `GET /v1/domain` lists (wildcard + apex → the box's
base domain, or the relay host when the box has no base domain). Issuance
starts immediately — records are needed for traffic, not for the cert.
`dns_ok` flips true once the wildcard resolves to the same address as the base
domain.

### Via environment variables — self-managed boxes

`PIPER_BASE_DOMAIN` + `PIPER_DNS_PROVIDER` (creds via the provider's own env
vars, e.g. `CLOUDFLARE_DNS_API_TOKEN`), or a static `PIPER_TLS_CERT_FILE` /
`PIPER_TLS_KEY_FILE` pair. Unchanged from before. Add `PIPER_SERVE=direct`
alongside `PIPER_BASE_DOMAIN` for the env-managed equivalent of direct serve.

A box with a public IP can skip the relay entirely and serve its own `:443`:
see [Direct serve](direct-serve.md).

### Precedence

**env > API > none.** A box whose base domain comes from the environment
(non-terminated relay mode) reports `"source":"env"` on `GET /v1/domain` and
answers `409` to `PUT`/`DELETE` — unset the env config to manage the domain
remotely. That includes a never-enrolled direct box, which is env-managed by
construction. A box with neither a relay nor direct serve has no domain config
at all and answers `409` to every `/v1/domain` call.

## Per-app domains (`piper domains`)

Attach a domain you own to **one specific app**. How its cert issues follows
the box-wide serve mode — there is no per-domain choice.

On a relay-served box (the default), no DNS-provider API token needed. Per-app
domains are exact hosts, so the box issues each cert via ACME
**TLS-ALPN-01** — the challenge rides the same relay splice as your traffic.
On a box that serves direct the same commands print an A record instead: see
[Direct serve](direct-serve.md#per-app-domains-on-a-direct-box).

    piper domains add myshop.com --app shop   # prints the record to create
    piper domains list [--app shop]           # domain, app, status, cert expiry, dns_ok
    piper domains remove myshop.com

Create the record `add` prints at your DNS host. On a relay-served box the
target is the box's base domain, or the relay host when the box has no base
domain — the base domain is under the relay apex, e.g.
`<box>.public.getpiper.dev`:

    myshop.com  CNAME  <box>.public.getpiper.dev

Unlike DNS-01 above, issuance **waits for DNS**: the cert can only issue once
the name resolves to the same address as that target (the same trade
Vercel/Netlify make).
`piper domains list` shows `dns=ok` when it does, and the status walks
`pending → issuing → active`. Once active, both `https://myshop.com` and
`http://myshop.com` reach the app, and the shared-domain URL keeps working
alongside. Renewal is automatic.

Notes:

- **Apex domains** on a relay-served box need a DNS host that supports CNAME
  at the apex (Cloudflare, or ALIAS/ANAME on others). Otherwise use a subdomain
  (`www.myshop.com`), or point an A/AAAA record at the address the printed
  target resolves to — accepting it may change.
- `www.myshop.com` is its own domain — attach it separately if you want both.
- A domain claim on the relay expires if the cert never issues, so a domain
  you don't control can't be squatted durably.
- TLS stays **end-to-end**: the box holds the key; the relay only splices
  bytes by SNI.
- Same surface for dashboards: `GET`/`POST /v1/apps/<app>/domains`,
  `DELETE /v1/apps/<app>/domains/<domain>`.
- Deleting the app detaches its domains and releases the relay claims.
