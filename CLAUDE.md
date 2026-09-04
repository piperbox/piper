# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

An open-source, developer-first PaaS that gives you `git push → live HTTPS URL` on hardware you own — including a Raspberry Pi behind CGNAT. A single Go module produces multiple binaries:

- `piperd` — the agent that runs on your box (control-plane, deployer, tunnel-client).
- `piper-relay` — the optional self-hostable cloud relay (SNI passthrough + tunnel server).
- `piper-edge` — the L4 entrypoint in front of N relays: routes each connection to the relay that owns the agent's tunnel (container-only, like the relay).
- `piper` — the CLI, a thin HTTP client to `piperd`.

Full design rationale lives in [`docs/superpowers/specs/`](docs/superpowers/specs/) — read it before non-trivial work. Implementation *plans* live in [`docs/superpowers/plans/`](docs/superpowers/plans/); work is delivered plan-by-plan, task-by-task, TDD-style. **Plan 1 of 3** is the agent core, LAN-only (build a Dockerfile → run a container → health-check → serve at `http://<app>.piper.localhost` via managed Caddy, SQLite state). Plan 2 = relay + outbound tunnel + DNS-01 TLS. Plan 3 = GitHub webhook + PR-preview URLs.

## Coding Principles

### 1. Think Before Coding
Don't assume. Don't hide confusion. Surface tradeoffs.

Before implementing:

- State your assumptions explicitly. If uncertain, ask.
- If multiple interpretations exist, present them - don't pick silently.
- If a simpler approach exists, say so. Push back when warranted.
- If something is unclear, stop. Name what's confusing. Ask.

### 2. Simplicity First
Minimum code that solves the problem. Nothing speculative.

- No features beyond what was asked.
- No abstractions for single-use code.
- No "flexibility" or "configurability" that wasn't requested.
- No error handling for impossible scenarios.
- If you write 200 lines and it could be 50, rewrite it.
- Ask yourself: "Would a senior engineer say this is overcomplicated?" If yes, simplify.

### 3. Surgical Changes
Touch only what you must. Clean up only your own mess.

When editing existing code:

- Don't "improve" adjacent code, comments, or formatting.
- Don't refactor things that aren't broken.
- Match existing style, even if you'd do it differently.
- If you notice unrelated dead code, mention it - don't delete it.

When your changes create orphans:

- Remove imports/variables/functions that YOUR changes made unused.
- Don't remove pre-existing dead code unless asked.
- The test: Every changed line should trace directly to the user's request.

### 4. Goal-Driven Execution
Define success criteria. Loop until verified.

Transform tasks into verifiable goals:

- "Add validation" → "Write tests for invalid inputs, then make them pass"
- "Fix the bug" → "Write a test that reproduces it, then make it pass"

For multi-step tasks, state a brief plan with clear verification steps.

## Development

- **Test-first.** Plans are written failing-test-first; keep that discipline. Every feature or bugfix starts with a test that fails, then the implementation that makes it pass.
- **Layering.** `store` knows only persistence; `runtime` knows only Docker; `caddy` knows only Caddy's admin API; `deploy` orchestrates the three through interfaces (so it unit-tests with fakes); `api` is transport over `deploy`+`store`; `client` is the CLI's view of `api`. **Nothing imports "up".**
- Match Go idiom and the style of the surrounding package.

## Commands

Shortcuts live in the `Makefile`:

- `make test` — `go test ./...`. Docker-dependent tests skip cleanly when Docker is absent; e2e needs real Docker + Caddy.
- `make build` — builds `bin/piperd` and `bin/piper` with `CGO_ENABLED=0`.
- `make cross` — `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ./...`; proves the Pi cross-compile still works.
- `make verify` — mirrors CI's `verify` gate: gofmt check → `go vet` → `make test` → `make cross`. `make fmt` (`gofmt -w .`) fixes formatting in place.

Always run `make verify` before claiming work is done or pushing — it catches the gofmt/vet failures `make test` alone misses.

## Hard constraints

- **No cgo.** All builds must pass with `CGO_ENABLED=0` so we can cross-compile to arm64/armv7 for a Pi. This forbids cgo SQLite drivers — use `modernc.org/sqlite` (pure Go) only.
- **Module path** is `github.com/piperbox/piper` (GitHub org `piperbox`).
- **Deployment status strings** are exactly `"building"`, `"running"`, `"failed"`, `"stopped"`.
- Defaults: control API `127.0.0.1:8088`, Caddy admin `http://127.0.0.1:2019`, base domain `piper.localhost`, app container port `8080`.

## Compatibility policy (pre-1.x)

Until release **1.x.x** we are **not at a user-facing release**: nobody but us runs Piper, and our own boxes are freely re-provisionable. Therefore:

- **Break freely.** SQLite schemas, the agent↔relay wire protocol, token/config file formats, CLI flags, and API shapes change in place — no compat shims, no deprecation cycles, no version negotiation.
- **No migrations.** Each `schema.sql` is always the complete current shape; a schema change edits the `CREATE TABLE` directly. Old databases are unsupported — deployed boxes get a fresh DB and re-enroll.
- **No legacy readers.** Never keep code that parses an older file or wire format.

When a change would otherwise add a shim or migration, change the format directly instead (precedent: #260). This section is revoked at v1.0.0.

## Commits

One commit per plan task step, conventional-commit style (`feat:`, `test:`, `chore:`). End commit messages with a co-author trailer naming the current model, e.g.:

```
Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>
```

## Branch workflow

**Trunk-based.** `main` is the single long-lived branch and is always green/releasable. **Never commit directly to `main`** — all work goes through a PR.

- Branch off `main`, named `<gh-name>/<short-description>` (e.g. `ozykhan/agent-store`).
- Reference the issue in commits and the PR body (`Part of #N`); put `Closes #N` in the PR body so merge closes fully-finished issues.
- Run `make verify` before pushing — it runs the same gofmt/vet/test/cross gate CI does, which must pass.
- Open the PR into `main` (`gh pr create --base main`) and **squash-merge** (one clean commit per feature on `main`).

There is no `dev`/`master` split: Piper's software is installed and run by users on their own hardware — not a service we deploy — so there's no environment for a second integration branch to gate. (The eventual hosted relay is the one scoped exception; see below.)

### Releases

Releases are **semver git tags** (`v0.1.0`), which a release workflow turns into a **GitHub Release** with cross-compiled binaries — including linux/arm64 + armv7 for the Pi. Pre-1.0 (`0.x`), breaking changes may land in minor bumps. Tooling ([goreleaser](https://goreleaser.com)) gets wired in at the first real release, not before. An "edge" channel, if wanted, is CI artifacts built from `main` — not a branch.

> **Future — hosted relay (convenience, never lock-in):** piperbox will eventually run a hosted relay so users don't *have* to stand up their own. It runs the **same open-source `piper-relay`** — no privileged fork, no closed features — and the relay stays **always fully self-deployable**. Piper is open source at heart and always will be; the hosted instance is a convenience, not the product. That hosted deployment gets its own pipeline **scoped to that service** (GitHub Environments + deploy-on-tag), kept separate from this repo-wide trunk flow.

## Issue tracking & progress

This is a **public, open-source project** — the tracker is how contributors (and future sessions) see what's done, what's in flight, and what's open. Keep the trail visible and proportional: a typo fix needs none of it; a plan task or feature gets the full loop.

- **Source of truth (one fact, one owner):** GitHub issues hold task state, acceptance criteria, and discussion. [`PROGRESS.md`](PROGRESS.md) is a coarse **built-vs-stubbed map that links to issues, never restates them** — one line + `[#N]` per item. A PR body is one change's record. Agent memory is machine-local and holds pointers only. Update `PROGRESS.md` when work lands; keep entries terse — detail belongs in the issue.
- **Start:** find the linked issue and read it for scope. Don't hand-flip board columns if a Project board is wired — it auto-moves cards on PR open/merge.
- **During:** reference the issue in commits / PR body (`Part of #N`).
- **Finish:** the PR body carries `Closes #N` for anything *fully* done. Don't close issues by hand mid-session; let merge do it. Leave an issue open when a real remainder survives.

### Title prefix — `[area]`

Every issue title starts with a lowercase `[area]` tag naming the surface it touches, so a glance down the board (or `gh issue list --search "in:title tunnel"`) finds everything in an area.

| Prefix | Surface |
| --- | --- |
| `[agent]` | `piperd` daemon: control-plane API, orchestration |
| `[cli]` | the `piper` CLI |
| `[deploy]` | build → run → health → route → retire flow |
| `[runtime]` | Docker build/run driver |
| `[proxy]` | Caddy routing, TLS |
| `[store]` | SQLite persistence |
| `[relay]` | `piper-relay` + outbound tunnel (Plan 2+) |
| `[repo]` | governance: CI, branch protection, tooling |
| `[docs]` | docs, design, README |

Add a new prefix only when a genuinely new surface appears; otherwise reuse one.

### Labels

**Always label a new issue when you open it** — don't leave it bare. At minimum give it a **type** (`bug`/`enhancement`/`documentation`), a **priority** (`P1`/`P2`/`P3`), and a **size** (`size/*`); add the **binary** label(s) it lands in, and any of the standard open-source labels that fit.

- **`agent`** / **`relay`** / **`cli`** — which binary the work lands in.
- **`P1`** / **`P2`** / **`P3`** — priority: golden-path user pain → real bugs/hardening/roadmap gaps → cleanups, test gaps, docs, polish.
- **`size/XS`** / **`size/S`** / **`size/M`** / **`size/L`** / **`size/XL`** — scope/effort (how *much* work, not how hard): a few lines/minutes → up to ~half a day → ~a day → several days → epic-scale (split it). Orthogonal to priority — a task can be small but P1, or large but P3.
- **`epic`** — a large multi-part tracker (e.g. a whole Plan); sub-tasks are checkboxes linking child issues.
- Standard open-source labels stay useful: `good first issue`, `help wanted`, `bug`, `enhancement`, `documentation`.
