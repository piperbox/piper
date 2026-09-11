# Direct serve

A box with a public IP can terminate its own HTTPS on `:443` and skip the
relay's splice: point DNS at the box, keep the relay for login and webhooks —
or run with no relay at all.

## Box-wide domain

`"serve"` on [the box-wide domain
API](custom-domains.md#via-the-control-api-dashboard--curl--relay-free-tier-boxes)
picks how traffic reaches the box: `"relay"`
(the default there) or `"direct"`. In direct mode the box terminates traffic
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

## A box that was never enrolled

**A box that has never been enrolled can do this too.** `PIPER_BASE_DOMAIN`
(anything but the built-in `piper.localhost`) plus `PIPER_SERVE=direct` and a
cert source is the whole configuration: the box obtains its own wildcard,
serves `PIPER_HTTPS_ADDR` itself, and renews on its own schedule, with no
relay anywhere. `PIPER_SERVE=direct` is the opt-in — a base domain alone keeps
today's plain-HTTP behaviour, so nothing changes for a LAN box that just wanted
a nicer hostname. One thing a never-enrolled box does not get: `dns_ok` and
the filled-in A-record values, which come from the relay-observed public IP
unless you set `PIPER_PUBLIC_IP` (serving is unaffected either way). Per-app
domains work — they issue via DNS-01 with the same token source (see
[Per-app domains on a direct box](#per-app-domains-on-a-direct-box) below).

## Per-app domains on a direct box

On a box whose serve mode is `direct`,
[`piper domains`](custom-domains.md#per-app-domains-piper-domains) and the API
are the same as in relay mode, with three differences:

- **The record is an `A`/`AAAA`, not a CNAME.** `add` prints the box's public
  IP (`myshop.com  A  203.0.113.7`) — the relay-observed address, or
  `PIPER_PUBLIC_IP`. Until an IP is known the status carries a note and
  `dns_ok` stays false; serving is unaffected. Apex domains need no special
  DNS host.
- **No DNS wait.** DNS-01 proves control through the token, not through
  resolution, so issuance starts immediately and the cert can be ready before
  the record exists. `dns_ok` reports whether the name resolves to that IP.
- **The box-wide DNS token is required.** `add` answers `409` on a direct box
  with no DNS-01 source — no box-wide domain, or a static
  `PIPER_TLS_CERT_FILE`/`PIPER_TLS_KEY_FILE` pair. The per-app domain must sit
  in a zone that token can edit; a mismatch surfaces at issuance, naming the
  token.

On an enrolled direct box the relay claim is still made whenever the relay is
connected, so flipping `serve` back to `relay` keeps the domain reachable.
Renewal follows whichever mode is current at renew time; flipping modes
re-issues nothing.
