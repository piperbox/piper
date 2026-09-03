# Contributing to Piper

Piper is an open-source, developer-first PaaS: `git push` → live HTTPS URL, on
hardware you own. Contributions are welcome — bug reports, docs fixes, and code.
The project is Apache-2.0 licensed; there is no CLA.

This file is the human front door. `CLAUDE.md` and `AGENTS.md` carry the same
rules in more depth for AI coding agents — if the two ever disagree, `CLAUDE.md`
wins and this file needs fixing.

## Before you start

Work is tracked in [issues](https://github.com/piperbox/piper/issues). New here?
Start with [`good first issue`](https://github.com/piperbox/piper/labels/good%20first%20issue).

- **Small fixes** (typos, an obvious one-line bug) — just open a PR.
- **Anything larger** — open or claim an issue first, so the approach can be
  agreed before you write code. Piper is opinionated about scope; a PR that
  adds unrequested abstraction or configurability will be asked to shrink.

[`PROGRESS.md`](PROGRESS.md) is the built-vs-stubbed map, and the design
rationale lives in [`docs/superpowers/specs/`](docs/superpowers/specs/) — worth
reading before non-trivial work.

## Development setup

You need **Go 1.26+**. Docker is optional for unit tests, required for the
Docker and end-to-end suites.

```bash
git clone https://github.com/piperbox/piper.git
cd piper
make build   # -> bin/piperd, bin/piper, bin/piper-relay
make test    # go test ./...
```

| Command | What it does |
| --- | --- |
| `make build` | builds all three binaries with `CGO_ENABLED=0` |
| `make test` | `go test ./...`; Docker-dependent tests skip cleanly when Docker is absent |
| `make e2e` | end-to-end suite against real Docker — needs `:80`, `:2019`, `:8088` free |
| `make cross` | `linux/arm64` no-cgo cross-compile; proves the Pi build still works |
| `make fmt` | `gofmt -w .` |
| `make verify` | the full gate: gofmt → `go vet` → `make test` → `make cross` |

**Run `make verify` before you push.** It mirrors CI's required `verify` job;
`make test` alone misses gofmt and vet failures, so a formatting-only slip
passes locally and fails CI.

Running `make e2e` and getting *"no response through Caddy"*? That's usually
another Caddy holding `:80`/`:2019`, not a code bug — `SO_REUSEPORT` lets
piperd's embedded Caddy bind alongside it silently. Check with
`lsof -nP -iTCP:80 -iTCP:2019 -sTCP:LISTEN` and kill the stray process first.

## How we write code

- **Test-first.** Every feature or bugfix starts with a failing test, then the
  implementation that makes it pass.
- **Simplicity first.** The minimum code that solves the problem — no
  speculative features, no abstractions for single-use code, no error handling
  for impossible cases.
- **Surgical changes.** Touch only what the task requires. Don't reformat or
  "improve" adjacent code; match the surrounding style even if you'd do it
  differently. Clean up orphans *your* change created, and mention (don't
  delete) pre-existing dead code.
- Match Go idiom and the conventions of the package you're editing.

### Layering

The single Go module builds three binaries (`cmd/piperd`, `cmd/piper`,
`cmd/piper-relay`) over packages in `internal/`. Each layer knows one thing:
`store` knows persistence, `runtime` knows Docker, `caddy` knows Caddy's admin
API, `deploy` orchestrates the three through interfaces, `api` is transport over
`deploy`+`store`, `client` is the CLI's view of `api`. **Nothing imports "up".**

This is enforced by `test/arch`, which ranks every `internal/` package. A new
package needs an entry there — but if an existing package suddenly needs its
rank changed to pass, the new import is usually the actual mistake.

### Hard constraints

- **No cgo.** Everything must build with `CGO_ENABLED=0` so it cross-compiles to
  arm64/armv7 for a Pi. That rules out cgo SQLite drivers — use pure-Go
  `modernc.org/sqlite` only.
- Module path is `github.com/piperbox/piper`.
- Deployment status strings are exactly `"building"`, `"running"`, `"failed"`,
  `"stopped"`.
- Defaults: control API `127.0.0.1:8088`, Caddy admin `http://127.0.0.1:2019`,
  base domain `piper.localhost`, app container port `8080`.
- Runtime environment configuration belongs in `internal/config`; production
  code must not call `os.Getenv` inline.

### Pre-1.0: break freely

Until 1.0 nobody but us runs Piper, and our boxes are re-provisionable. So
SQLite schemas, the agent↔relay wire protocol, token/config file formats, CLI
flags, and API shapes change **in place**: no compat shims, no deprecation
cycles, no migrations, no readers for older formats. A schema change edits the
`CREATE TABLE` in `schema.sql` directly; old databases are unsupported. If your
change would otherwise add a shim, change the format instead.

## Branches, commits, PRs

Trunk-based: `main` is the only long-lived branch and is always releasable.
**Never commit directly to `main`.**

1. Branch off `main`, named `<your-gh-name>/<short-description>` — e.g.
   `ozykhan/agent-store`.
2. Commit in conventional-commit style (`feat:`, `fix:`, `test:`, `chore:`),
   one logical change per commit. Reference the issue: `Part of #N`.
3. Run `make verify`.
4. Open the PR into `main` (`gh pr create --base main`). Put `Closes #N` in the
   body for anything fully finished; leave the issue open if a real remainder
   survives.
5. PRs are **squash-merged** — one clean commit per feature on `main`.

### CI

- **`verify`** (required) — gofmt, `go vet`, tests, arm64 cross-compile, plus
  goreleaser/packaging/Docker checks when those files change. Must pass.
- **`e2e`** (not required) — the Docker end-to-end suite; skipped entirely on
  docs-only PRs.

## Filing issues

Every issue title starts with a lowercase `[area]` tag:

| Prefix | Surface |
| --- | --- |
| `[agent]` | `piperd` daemon: control-plane API, orchestration |
| `[cli]` | the `piper` CLI |
| `[deploy]` | build → run → health → route → retire flow |
| `[runtime]` | Docker build/run driver |
| `[proxy]` | Caddy routing, TLS |
| `[store]` | SQLite persistence |
| `[relay]` | `piper-relay` + outbound tunnel |
| `[repo]` | governance: CI, branch protection, tooling |
| `[docs]` | docs, design, README |

Label it too — at minimum a type (`bug` / `enhancement` / `documentation`), a
priority (`P1`–`P3`), and a size (`size/XS`–`size/XL`). Add the binary it lands
in (`agent` / `cli` / `relay`) and `good first issue` / `help wanted` where they
fit. Priority and size are orthogonal: a task can be small but `P1`.

For bugs, include your OS/arch, `piper version`, and what you expected versus
what happened. The issue forms ask for all of this and prefill the `[area]`
prefix, so opening one from the **New issue** page is the easiest way to get it
right — the type label is applied for you; priority and size are triage calls
the maintainers make.

Suspected security issues: **don't open a public issue.** Report them privately
through [GitHub's advisory form](https://github.com/piperbox/piper/security/advisories/new);
[`SECURITY.md`](SECURITY.md) covers supported versions and what's in scope.
