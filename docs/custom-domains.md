# Custom domains (BYO)

Two kinds:

- a **box-wide base domain** — every app served as `<app>.<yourdomain>` under
  one wildcard cert (needs a DNS-provider API token for DNS-01);
- **per-app domains** — `myshop.com` pointed at one specific app, tokenless
  (see [Per-app domains](#per-app-domains-piper-domains--no-dns-token) below).

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

### Direct serve

`"serve"` on `PUT /v1/domain` picks how traffic reaches the box: `"relay"`
(default, above) or `"direct"`. In direct mode the box terminates traffic
itself — point `<domain>` and `*.<domain>` A/AAAA records straight at the
box's public IP, which `GET /v1/domain` fills in from the relay-observed
address (override with `PIPER_PUBLIC_IP` for split-horizon or NAT setups, and
the only source on a box that was never enrolled). On an enrolled box the
relay claim is kept regardless, so both paths serve the same cert while
your DNS still points at the relay — the flip to direct is gradual and
reversible, not a cutover. A box behind CGNAT will never show `dns_ok: true`
in direct mode (nothing can dial it); port-forwarded boxes should confirm
with a real request to the domain rather than trust `dns_ok` alone.

Flipping `serve` alone on an otherwise-unchanged config re-sends the same
`domain`/`dns_provider`/`dns_token` — the row updates in place and issuance is
left alone. Change the token too and it's treated as a config replacement:
issuance restarts from scratch.

Secrets never leave the box: the DNS token is write-only (`dns_token_set`
signals presence), and the cert's private key and ACME account key live in
piperd's data dir with 0600 permissions.

### Via environment variables — self-managed boxes

`PIPER_BASE_DOMAIN` + `PIPER_DNS_PROVIDER` (creds via the provider's own env
vars, e.g. `CLOUDFLARE_DNS_API_TOKEN`), or a static `PIPER_TLS_CERT_FILE` /
`PIPER_TLS_KEY_FILE` pair. Unchanged from before. Add `PIPER_SERVE=direct`
alongside `PIPER_BASE_DOMAIN` for the env-managed equivalent of direct serve.

**A box that has never been enrolled can do this too.** `PIPER_BASE_DOMAIN`
(anything but the built-in `piper.localhost`) plus `PIPER_SERVE=direct` and a
cert source is the whole configuration: the box obtains its own wildcard,
serves `PIPER_HTTPS_ADDR` itself, and renews on its own schedule, with no
relay anywhere. `PIPER_SERVE=direct` is the opt-in — a base domain alone keeps
today's plain-HTTP behaviour, so nothing changes for a LAN box that just wanted
a nicer hostname. Two things a never-enrolled box does not get: `dns_ok` and
the filled-in A-record values, which come from the relay-observed public IP
unless you set `PIPER_PUBLIC_IP` (serving is unaffected either way); and per-app
domains, whose TLS-ALPN-01 challenges only arrive over a relay splice.

### Precedence

**env > API > none.** A box whose base domain comes from the environment
(non-terminated relay mode) reports `"source":"env"` on `GET /v1/domain` and
answers `409` to `PUT`/`DELETE` — unset the env config to manage the domain
remotely. That includes a never-enrolled direct box, which is env-managed by
construction. A box with neither a relay nor direct serve has no domain config
at all and answers `409` to every `/v1/domain` call.

## Per-app domains (`piper domains`) — no DNS token

Attach a domain you own to **one specific app**. Unlike the box-wide domain
this needs no DNS-provider API token: per-app domains are exact hosts, so the
box issues each cert via ACME **TLS-ALPN-01** — the challenge rides the same
relay splice as your traffic.

    piper domains add myshop.com --app shop   # prints the CNAME to create
    piper domains list [--app shop]           # domain, app, status, cert expiry, dns_ok
    piper domains remove myshop.com

Create the record `add` prints at your DNS host. The target is the box's base
domain, or the relay host when the box has no base domain — for a relay-mode
box the base domain is under the relay apex, e.g. `<box>.public.getpiper.dev`:

    myshop.com  CNAME  <box>.public.getpiper.dev

Unlike DNS-01 above, issuance **waits for DNS**: the cert can only issue once
the name resolves to the same address as that target (the same trade
Vercel/Netlify make).
`piper domains list` shows `dns=ok` when it does, and the status walks
`pending → issuing → active`. Once active, both `https://myshop.com` and
`http://myshop.com` reach the app, and the shared-domain URL keeps working
alongside. Renewal is automatic.

Notes:

- **Apex domains** need a DNS host that supports CNAME at the apex
  (Cloudflare, or ALIAS/ANAME on others). Otherwise use a subdomain
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
