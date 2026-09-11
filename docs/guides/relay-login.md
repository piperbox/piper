# Join the public relay

One command on the box signs you in with GitHub and claims the box on the
public relay: your apps get `https://<hash>-<you>.public.getpiper.dev` URLs
with no port forwarding and no domain of your own.

## One command

On a box running `piperd`, one command signs you in and claims the box:

```bash
piper login
#   To log in, open: https://github.com/login/device ... enter the code: XXXX-XXXX
#   logged in to relay as alice
#   claiming this box… enrolled as ab12-alice.public.getpiper.dev
#   applying… piperd connected — this box is live
```

`piper login` is the whole flow: GitHub device-flow identity, then it hands
the account credential to `piperd` over a local enrollment socket. `piperd`
itself calls the relay, validates the token with a real tunnel handshake,
persists `relay.json`, and applies it — draining and re-executing its own
process so the enrollment is live within seconds. No sudo command to
copy-paste, no manual restart, on any install (systemd, Homebrew, or a manual
dev box).

`login` opens the verification page in a browser as a convenience; the URL and
code are always printed too. Set `PIPER_NO_BROWSER=1` to skip the launch — for
a headless box, an SSH session, or a test harness driving the CLI.

Run `login` **on the box**: on a machine with no piperd install (no systemd
install, launchd agent, or existing data dir) it stops after identity —
*"identity only — no piperd on this machine; run `piper login` on a box to
connect it."* Exit 0; that's the expected shape for a laptop that only drives
boxes remotely (see [Remote control](remote-control.md)), not an
error.

`piper login` claims the box in **terminated** mode: piperd holds no cert and
serves apps on `:80`; the relay assigns each app a single-label hostname
`<app-hash>-<username>.public.getpiper.dev`, terminates its HTTPS with its
wildcard cert, and forwards plaintext HTTP over the tunnel.

```bash
piper login                  # GitHub sign-in + claims this box, terminated
piper deploy blog --path .   # → https://<hash>-<you>.public.getpiper.dev
```

Re-running `piper login` converges instead of redoing work: a saved credential
skips the device flow, an already-enrolled box skips the claim (`already
enrolled as …`), and a tunnel that's merely down gets re-verified. Flags shape
the claim:

- `--org <slug>` — enroll the box for a GitHub org you own instead of your
  personal account.
- `--no-enroll` — stop after identity; leave this box's enrollment untouched
  (the laptop/remote-management shape, forced).
- `--re-enroll` — force a fresh claim on an already-enrolled box — recovery
  after `piper box rm`, or after switching accounts or relays.
- `--relogin` — authenticate again even though the saved credential still
  works, for signing in as a different GitHub account.
- `--data-dir <path>` — the piperd data directory to probe for the enrollment
  socket, when it isn't the default.

An operator who pins `PIPER_RELAY_ADDR`/`PIPER_RELAY_TOKEN`/`PIPER_BASE_DOMAIN`
in `/etc/piper/piperd.env` (or piperd's process environment) locks the
enrollment: `piper login` prints *"this box's enrollment is operator-managed
via /etc/piper/piperd.env — nothing to do"* and exits 0 rather than touching
it.

`piper login --relay <url>` targets a self-hosted relay instead of the default
`https://api.public.getpiper.dev`. Environment variables (`PIPER_RELAY_ADDR`,
`PIPER_RELAY_TOKEN`, `PIPER_BASE_DOMAIN`) still override `relay.json`.

## List and remove boxes

```bash
piper box ls                                          # base domain, owner, connected
piper box rm ab12-alice.public.getpiper.dev --yes      # frees the box slot
```

Removal frees the box slot for a fresh claim — `piper login --re-enroll` on
the box; a connected box must be stopped first (the relay refuses with a
conflict otherwise). The box's relay-assigned `<hash>-<user>.<apex>` app URL
stays reserved on the account, but any custom domains it held are released and
can be re-claimed elsewhere.

## Your own domain instead

Bring-your-own-domain apps stay **end-to-end** (the box terminates TLS; the relay
only splices SNI) — set `PIPER_BASE_DOMAIN` + cert/DNS config instead of
claiming through `piper login`; see [Custom domains](custom-domains.md).
Add `PIPER_SERVE=direct` and the box serves `:443` itself with no relay at all
([Direct serve](direct-serve.md)).
[Run your own relay](../self-host/relay.md) passthrough-only by leaving
`PIPER_RELAY_TLS_CERT`/`KEY` unset.
