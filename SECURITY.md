# Security policy

Piper runs on hardware you own and exposes it to the public internet — the agent
terminates TLS, the relay carries traffic for boxes it doesn't control, and the
CLI holds credentials for both. Security reports are welcome and taken
seriously.

## Supported versions

Piper is pre-1.0 and moves fast. Only the **latest release** receives security
fixes; there are no backports to older tags. A fix ships in the next release cut
from `main`.

| Version | Supported |
| --- | --- |
| Latest release | Yes |
| Anything older | No — upgrade |

Piper is self-hosted software, not a service we operate: there is no fleet-wide
push, so applying a fix means upgrading your own box.

## Reporting a vulnerability

**Please don't open a public issue for a suspected vulnerability.**

Report it privately with GitHub's
[**Report a vulnerability**](https://github.com/piperbox/piper/security/advisories/new)
form — also reachable from this repo's **Security** tab. That opens a draft
advisory visible only to you and the maintainers.

Helpful to include:

- what an attacker gains, and what access they need to start
- affected version and component (`piperd`, `piper`, `piper-relay`, packaging)
- steps to reproduce, or a proof of concept
- whether you're reporting something you observed or something you inferred from
  reading the code — both are welcome, just say which

Piper is maintained by a very small team. Expect an acknowledgement within a few
days rather than the same hour; if you hear nothing after a week, please ping the
advisory thread.

## Scope

**In scope** — the three binaries and how they're delivered:

- `piperd`: the authenticated control API (the relay path and non-loopback
  binds), deploy orchestration, container and secret handling, certificate and
  TLS handling
- `piper-relay`: tenant isolation, enrollment and token handling, the tunnel
  protocol, SNI routing
- `piper`: credential storage and handling on the developer's machine
- the GitHub webhook path and PR-preview routing
- `install.sh`, the apt/Homebrew packaging, and the published container images

**Out of scope:**

- vulnerabilities in the applications *you* deploy on Piper — Piper builds and
  runs your code as given, and the security of that code is yours
- flaws in third-party dependencies with no Piper-specific exploit path; report
  those upstream (tell us anyway if Piper's use of them makes it exploitable)
- the loopback control API listener answering local requests without a token —
  that's deliberate ([#221](https://github.com/piperbox/piper/issues/221)): the
  bind *is* the trust boundary, so anyone who can already run processes on the
  box is inside it. A way to reach that listener from off-box, or from a
  context that shouldn't have it, very much is in scope
- attacks that require an adversary who already has root on the box, or who
  already holds a valid agent or relay token
- findings from automated scanners with no demonstrated impact

## Disclosure

We'll work with you on a fix and coordinate timing on disclosure, publishing a
GitHub advisory once a release carrying the fix is out. Reporters are credited
in the advisory unless you'd rather stay anonymous — just say so.
