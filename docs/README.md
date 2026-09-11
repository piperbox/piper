# Docs map

Who each folder is for, what the website publishes, and how to add a page.

| Folder | For | Published |
| --- | --- | --- |
| [`guides/`](guides/) | people using Piper: install → first deploy → relay → domains | yes |
| [`reference/`](reference/) | CLI verbs, env vars, control API — pinned to code by `test/docs` | yes (PR 2) |
| [`self-host/`](self-host/) | running piperd or a relay yourself, from source or containers | not yet — one manifest line away |
| [`ops/`](ops/) | piperbox's own hosted relay and the e2e verification runbook | never |
| [`superpowers/specs/`](superpowers/specs/), [`superpowers/plans/`](superpowers/plans/) | design rationale and task-by-task plans; a fresh session reads the spec before non-trivial work | no |

[`manifest.json`](manifest.json) is the single owner of what is published, in
what order, under which title. The dashboard's `bun run sync:docs` fetches it
and every file it names; a file not listed is repo-only.

## Adding a published page

1. Write it under `guides/` or `reference/`: one `# H1`, then a lead paragraph
   of one to three sentences (the site index and `llms.txt` show it), no
   frontmatter, relative links (`install.md#anchor` in-folder,
   `../reference/cli.md` across).
2. Add its `{ "slug", "file", "title" }` line to `manifest.json`.
3. `go test ./test/docs/` checks the rules; `make verify` runs it too.
4. In the dashboard repo, `bun run sync:docs` and commit the snapshot.

Never link a published page into `ops/`. Linking into `self-host/` is fine;
the site renders it as a GitHub link.

## For coding agents

Working rules and repo conventions are in [`CLAUDE.md`](../CLAUDE.md); what is
built versus open is [`PROGRESS.md`](../PROGRESS.md), which links issues
rather than restating them. Design decisions are in `superpowers/specs/`, one
dated file per feature; check the date and PROGRESS before treating a spec as
current.
