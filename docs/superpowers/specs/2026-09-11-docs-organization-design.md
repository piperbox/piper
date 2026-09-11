# Docs organization: one tree for humans and agents

Piper's documentation is reorganized so that the piperbox website can publish
it, humans can find the page for their situation, and agents — both coding
agents working on the repo and end-user agents driving Piper — can navigate it
without scraping. This spec covers the piper repo (source of truth) and the
dashboard repo (renderer). It is delivered as three PRs, in order.

## Background

Today `docs/` holds three user docs, two ops runbooks, and the `superpowers/`
design and plan directories. `getting-started.md` is a 354-line walkthrough
spanning install through git deploys; `custom-domains.md` mixes the relay and
direct serve paths, which is how it went stale after #511 (fixed in #560);
`runbooks/relay-deploy.md` mixes "run your own relay" with the piperbox hosted
relay's k3s/Flux/Hetzner specifics.

The dashboard already has a `/docs` renderer
(`dashboard/docs/superpowers/specs/2026-07-25-docs-site-design.md`): a sync
script that fetches a hard-coded list of three files from piper `main`, a
hand-authored `manifest.ts` that ships empty, and a markdown→React pipeline
mapped onto the terminal design system. Its spec says piper's prose needs
rework before publishing. This is that rework.

Decisions carried over from the dashboard spec and not reopened here:
markdown stays in `piperbox/piper`; the dashboard is a renderer, never the
origin; no frontmatter, because GitHub renders it as a table above every page;
no docs framework, no versioned docs, no runtime fetch.

## Audiences

- **Humans reading on the website** — install, first deploy, relay, domains,
  reference.
- **Humans reading on GitHub** — the same pages, plus self-host and ops
  material that is not published yet.
- **Coding agents on the repo** — need a map of which folder serves whom.
  Design, plans, and progress tracking are unchanged; the user's assessment
  is that this side does not hurt today.
- **End-user agents** — need `llms.txt` and raw markdown at stable URLs.

## Piper repo

### Tree

```
docs/
  README.md          the map: who each folder serves, what is published, how to add a page
  manifest.json      published nav: sections → pages (slug, file, title)
  guides/            published
    install.md         universal installer, apt, brew, diet, from source
    first-deploy.md    create → deploy → list/status/stop/delete on piper.localhost
    tui.md             the interactive TUI
    lan-control.md     drive piperd from another machine on the LAN
    relay-login.md     join the public relay, box ls / rm, operator-pinned env
    remote-control.md  --remote / PIPER_REMOTE through the relay
    git-deploys.md     push-to-deploy, PR previews, self-hosted relay + BYO GitHub App
    custom-domains.md  box-wide and per-app domains, relay mode
    direct-serve.md    direct mode, never-enrolled box, per-app domains on direct
  reference/         published
    cli.md             every piper verb and subcommand, flags, exit codes
    env.md             piperd, relay, edge, and CLI env vars with defaults
    api.md             the control API, one entry per route
  self-host/         repo-only for now; one manifest line publishes a page
    piperd.md          systemd from source, macOS dev box, Docker Compose
    relay.md           run your own relay: systemd, container, upgrade paths
  ops/               repo-only, never published
    hosted-relay.md    the piperbox relay: k3s, Flux, Hetzner specifics, drain recipe
    e2e-runbook.md     the from-scratch verification walkthrough
  superpowers/       unchanged
```

### Where today's content goes

| Today | Becomes |
| --- | --- |
| `getting-started.md` § Install (all channels) | `guides/install.md` |
| README § quick start (`create` → `deploy` → `list`/`status`/`stop`/`delete` on `piper.localhost`), which no doc page covers today | `guides/first-deploy.md`, new |
| `getting-started.md` § The interactive TUI | `guides/tui.md` |
| `getting-started.md` § Drive piperd from another machine on the LAN | `guides/lan-control.md` |
| `getting-started.md` § Join the public relay, § List and remove boxes | `guides/relay-login.md` |
| `getting-started.md` § Drive a box remotely | `guides/remote-control.md` |
| `getting-started.md` § Point your own domain at an app | folded into `guides/custom-domains.md` |
| `getting-started.md` § Git deploys, § Self-hosted relay / BYO GitHub App | `guides/git-deploys.md` |
| `getting-started.md` § Upgrading from a pre-0.15 install | **dropped** — pre-1.0 policy: old installs are unsupported |
| `custom-domains.md` box-wide + per-app, relay mode | `guides/custom-domains.md` |
| `custom-domains.md` § Direct serve, never-enrolled box, § Direct-served boxes | `guides/direct-serve.md` |
| `manual-setup.md` § agent as a service, § macOS, § Docker | `self-host/piperd.md` |
| `manual-setup.md` § Run the relay as a service | `self-host/relay.md` |
| `runbooks/relay-deploy.md` § What's on disk, Fresh deploy, Upgrade (both), Ops surface, Run as a container, Scale out, Single host with compose | `self-host/relay.md` |
| `runbooks/relay-deploy.md` § Kubernetes, § Rolling out with Flux, and every Hetzner/`hetzner-box`/`relay-ops` specific | `ops/hosted-relay.md` |
| `runbooks/git-deploy-e2e.md` | `ops/e2e-runbook.md`, unchanged |

Prose may be rewritten freely in the move: tighten, unify voice, drop
history. Facts are re-verified against code during the rewrite, the same
discipline as #560. Old paths are not redirected or kept; nothing automated
depends on them, and pre-1.0 policy allows the break.

### Page rules (stated in `docs/README.md`, pinned by the contract test)

- A page starts with a single `# H1` followed by a lead paragraph of one to
  three sentences. The dashboard index and `llms.txt` render the lead.
- No frontmatter.
- Cross-links are relative `.md` paths with GitHub-slug anchors:
  `install.md#homebrew-macos` within a folder, `../reference/cli.md` across.
- A published page links only to published pages or to repo files that are
  not docs (a unit file, a script). It never links into `self-host/` or
  `ops/`; those readers arrive from GitHub, not the site.
- Headings inside fenced or indented code do not count as headings (the
  dashboard's TOC extractor already enforces this; authors just need to know).

### `docs/manifest.json`

The single owner of what is published, in what order, under which title:

```json
{
  "sections": [
    { "title": "Guides", "pages": [
      { "slug": "install",      "file": "guides/install.md",      "title": "Install" },
      { "slug": "first-deploy", "file": "guides/first-deploy.md", "title": "First deploy" }
    ]},
    { "title": "Reference", "pages": [
      { "slug": "cli", "file": "reference/cli.md", "title": "CLI" }
    ]}
  ]
}
```

Slugs are flat and stable: `/docs/custom-domains` survives a future folder
move. A file not listed is repo-only. Section titles are display strings; the
dashboard renders them as sidebar headers in manifest order.

### `docs/README.md`

One screen. For each folder: who it serves and whether it is published. How
to add a page: write it, add the manifest line, run `bun run sync:docs` in
the dashboard. For coding agents: design in `superpowers/specs/`, plans in
`superpowers/plans/`, state in `PROGRESS.md`, working rules in `CLAUDE.md`.
The repo README's docs table shrinks to one link per section plus
`PROGRESS.md` and the design spec. `CLAUDE.md` gains one line pointing at
`docs/README.md` and changes nothing else.

### Reference pages

Three hand-written pages, each pinned to code by a drift test (below). The
layout is chosen so a future generator can emit the same shape, keeping
slugs and anchors stable across the switch.

- **`reference/cli.md`** — one `##` per verb, one `###` per subcommand. Each
  entry opens with the synopsis exactly as the binary prints it (the text
  after `usage: `), then a flags table, what the command does, and the exit
  code (0 ok, 1 error, 2 usage).
- **`reference/env.md`** — one table per binary: piperd, piper-relay,
  piper-edge, and the CLI's own (`PIPER_REMOTE`, `PIPER_NO_BROWSER`, …).
  Columns: name, default, meaning. Test-only knobs (`PIPER_TEST_ISSUER`,
  `PIPER_RELAY_FAKE_APPROVE`) get a marked row, not an exemption list.
  `packaging/systemd/piperd.env.example` stays the curated subset.
- **`reference/api.md`** — one `###` per route headed `METHOD /v1/path`:
  auth requirement, request and response JSON shapes from the types in
  `internal/api`, and the error statuses the handler returns (e.g. the 409
  for a per-app domain on a direct box without a DNS token).

### `test/docs` package

New Go package holding the manifest contract test and the three drift
tests, run by `go test ./...` and therefore by `make verify` and CI. One
helper serves all three drift tests: parse a package directory with
`go/parser`, skip `_test.go`, collect string literals matching a predicate.

- **Manifest contract** — `docs/manifest.json` parses; every `file` exists;
  slugs are unique; every listed file has an H1 then a lead paragraph; every
  relative `.md` link in a listed file resolves to another listed file, and
  no listed file links into `self-host/` or `ops/`.
- **CLI drift** — every literal in `cmd/piper` beginning with `usage: piper`
  appears, minus the `usage: ` prefix, verbatim in `reference/cli.md`.
- **Env drift** — every literal matching `^PIPER_[A-Z_]+$` in
  `internal/config`, `cmd/piper`, `cmd/piper-relay`, `cmd/piper-edge` appears
  backticked in `reference/env.md`.
- **API drift** — every route literal registered on the mux in
  `internal/api` appears as a `###` heading in `reference/api.md`.

Each drift test is verified by mutation before merge: add a fake verb, env
var, or route and confirm the test fails. The tests deliberately do not
check prose, flag descriptions, or JSON field lists; those stay
human-reviewed.

### Step two: generation (follow-up issue, not built here)

A generator needs descriptions in code: a command table with help text in
`cmd/piper` (dispatch today is a `switch` with hand-typed usage strings),
doc comments on `internal/config` fields, and a route table with a one-line
purpose per endpoint. When that lands, the generator overwrites the three
reference files and the drift tests become "generated output is committed
and current". Filed as `[docs]`/`[cli]` P3 after PR 2 merges.

## Dashboard repo

All changes sit inside the existing docs machinery.

- **Sync follows the manifest.** `scripts/sync-docs.ts` fetches
  `docs/manifest.json` from piper `main`, then every file it names, and
  writes them flattened by slug to `src/content/docs/<slug>.md`, plus a
  committed copy of the manifest and the existing `source.json`. Fail-loud
  on any 404 or empty body is unchanged. Sync stays a manual command.
- **`manifest.ts` is deleted.** Layout, index, and routes import the synced
  `manifest.json`. `DocEntry` gains `section`; the sidebar renders section
  headers in manifest order. JSON imports work under Vite and `bun test`, so
  the `import.meta.glob` quarantine in `docs-content.ts` is unchanged.
- **Folder-aware `docHref`.** A guide links to `install.md` and to
  `../reference/cli.md`. `docHref(href, fromFile)` resolves the target
  relative to the source file's folder, then maps file path → slug through
  the manifest. Anything not in the manifest falls back to a GitHub blob
  URL, which is the right result for a link into `self-host/`.
- **Two agent-facing routes.** `/llms.txt` at the site root, built by a pure
  function from the manifest plus each page's lead paragraph, in the
  llmstxt.org shape: title, one-paragraph description, then one bulleted
  link per page with its lead as the summary, each pointing at
  `/docs/<slug>.md`. `/docs/<slug>.md` serves the raw markdown as
  `text/plain`. Both are server routes with no React rendering.
- **The landing page links `/docs`.** The docs-site spec kept `/docs`
  unlinked until content existed. Content now exists, so the header and
  landing-page docs links flip from GitHub to the site.

## Rollout

Three PRs, in order, each with its own implementation plan under
`superpowers/plans/` (the dashboard plan lives in the dashboard repo and
links here). Memory notes on the maintainers' machines that point at
`runbooks/relay-deploy.md` are repointed to `ops/hosted-relay.md` when PR 1
lands.

1. **piper — tree, guides, map, manifest, contract test.** Moves, splits,
   rewrite, `docs/README.md`, `manifest.json`, `test/docs` with the manifest
   contract test, README docs table, every in-repo link, the one-line
   `CLAUDE.md` pointer. `PROGRESS.md` gets a line under Foundation.
2. **piper — reference pages and drift tests.** Separate PR so prose review
   and code-enumeration review do not compete. Files the step-two issue on
   merge.
3. **dashboard — manifest-driven sync, sections, llms.txt, raw routes,
   landing links.** After both piper PRs merge; its first commit includes
   the synced snapshot.

## Testing

- **Piper:** `make verify` picks up `test/docs` through `go test ./...`.
  Drift tests are mutation-checked before merge as described above.
- **Dashboard:** `bun run verify`. `scripts/sync-docs.test.ts` rewritten for
  the manifest-driven fetch with an injected `fetch`, including fail-loud on
  a missing manifest file. `src/lib/docs.test.ts` extended for
  folder-relative links and the blob fallback for unlisted files. The layout
  test asserts section headers. A new test covers the `llms.txt` builder
  against a fixture manifest and pages.

## Success criteria

1. In the dashboard, `bun run sync:docs` then `bun run verify` produces the
   complete site with zero hand edits.
2. `/llms.txt` lists every published page with its lead paragraph;
   `/docs/<slug>.md` returns the same bytes the sync wrote.
3. Adding a guide upstream is one markdown file plus one manifest line.
4. Each drift test fails on an undocumented verb, env var, or route.
5. No published page links into `self-host/` or `ops/`.

## Rejected alternatives

- **Flat `docs/` with more files, manifest kept in the dashboard.** Least
  movement, but no human/agent boundary in the tree, every new page is a
  two-repo edit, and runbooks sit beside published pages with nothing
  marking them repo-only.
- **Move published docs into the dashboard repo.** Rejected by the docs-site
  spec; breaks reading the docs next to the code on GitHub.
- **Frontmatter for titles and order.** GitHub renders it as a table above
  every page.
- **A `tutorials/` vs `how-to/` split.** Nine guides do not need a second
  axis.
- **Generating reference pages now.** Nothing structured to read: the CLI
  has no command table and no descriptions live in code. Drift tests give
  most of the safety at a fraction of the change; generation is step two.
- **Publishing self-host docs in this round.** The user's call; the tree
  makes it a one-line manifest change later.
- **`llms.txt` as a file in piper.** Only meaningful at a URL, and it would
  duplicate the manifest plus every lead paragraph.
