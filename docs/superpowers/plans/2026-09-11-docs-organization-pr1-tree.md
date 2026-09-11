# Docs Organization PR 1 — Tree, Guides, Map, Manifest Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Reorganize `docs/` into published `guides/`, repo-only `self-host/` and `ops/`, with a `manifest.json` the dashboard syncs from and a Go contract test that keeps every published page well-formed.

**Architecture:** Prose moves out of three monolithic user docs and two runbooks into one page per topic, split by audience folder. A new `test/docs` package (sibling of `test/arch`) reads `docs/manifest.json` and pins the page rules. Old files stay until the tests that read them are repointed in the same commit that deletes them (Task 12), so the existing suite is green at every commit. The new contract test is red only for forward links between Tasks 2 and 10 and green from Task 11 on.

**Tech Stack:** Markdown, Go 1.26 `testing` + `encoding/json` (no new dependencies), `make verify`.

Spec: `docs/superpowers/specs/2026-09-11-docs-organization-design.md`. This plan is PR 1 of 3; reference pages (PR 2) and the dashboard (PR 3) are separate plans.

## Global Constraints

- Branch `ozykhan/docs-organization` off `main`; one commit per task; conventional `[docs]` titles; commit trailer `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>`.
- Page rules (from the spec, pinned by Task 1's test): a page starts with exactly one `# H1`, then a lead paragraph of one to three sentences of plain prose; no frontmatter; relative links resolve against the page's own directory, GitHub-style; a published page never links into `docs/ops/`.
- Cross-link style: `install.md#anchor` within a folder, `../reference/cli.md` across folders, GitHub-slug anchors (lowercase, spaces → `-`, punctuation dropped).
- Prose may be rewritten freely, but every fact carried over is re-checked against the code path named in the task. Nothing about behaviour is invented.
- Old paths are deleted, not redirected. Files under `docs/superpowers/` that mention old paths are history and are **not** edited.
- Phrases pinned by existing Go tests must survive the move verbatim; Task 12 lists each one and where it now lives.
- `go test ./packaging/... ./deploy/...` passes at the end of every task; `make verify` (gofmt → vet → `go test ./...` → arm64 cross-build) passes from Task 11 on and is the gate in Task 14. Judge it by exit status, not by grepping its output.

---

## File structure

| Path | Responsibility |
| --- | --- |
| `docs/manifest.json` | published nav: sections → pages (`slug`, `file`, `title`) |
| `docs/README.md` | the map: who each folder serves, what is published, how to add a page |
| `docs/guides/*.md` | nine published guides (Tasks 2–10) |
| `docs/self-host/piperd.md`, `docs/self-host/relay.md` | repo-only self-hosting (Task 11) |
| `docs/ops/hosted-relay.md`, `docs/ops/e2e-runbook.md` | repo-only piperbox operations (Task 12) |
| `test/docs/manifest_test.go` | contract test: manifest shape, H1 + lead, link resolution, no links into `ops/` |
| `packaging/systemd/piperd_test.go`, `packaging/systemd/piper-relay_test.go`, `packaging/install/install_test.go`, `deploy/compose/compose_test.go`, `install.sh`, `deploy/compose/relay/docker-compose.yml`, `.claude/skills/release/SKILL.md` | existing consumers of the old paths, repointed in Task 12 |
| `README.md`, `CLAUDE.md`, `PROGRESS.md` | index surfaces updated in Task 13 |

Deleted at the end of Task 12: `docs/getting-started.md`, `docs/custom-domains.md`, `docs/manual-setup.md`, `docs/runbooks/`.

---

### Task 1: Contract test and an empty manifest

**Files:**
- Create: `test/docs/manifest_test.go`
- Create: `docs/manifest.json`

**Interfaces:**
- Produces: `docs/manifest.json` with shape `{"sections":[{"title":string,"pages":[{"slug","file","title"}]}]}`. Every later task appends one page object. `file` is relative to `docs/`.

- [ ] **Step 1: Write the failing test**

Create `test/docs/manifest_test.go`:

```go
// Package docs holds the documentation contract tests: the shape
// docs/manifest.json promises the dashboard's sync script, and the page rules
// docs/README.md states for anything the manifest publishes.
package docs_test

import (
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const docsDir = "../../docs"

type page struct {
	Slug  string `json:"slug"`
	File  string `json:"file"`
	Title string `json:"title"`
}

type manifest struct {
	Sections []struct {
		Title string `json:"title"`
		Pages []page `json:"pages"`
	} `json:"sections"`
}

func loadManifest(t *testing.T) manifest {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(docsDir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m manifest
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("manifest.json: %v", err)
	}
	return m
}

func publishedPages(m manifest) []page {
	var out []page
	for _, s := range m.Sections {
		out = append(out, s.Pages...)
	}
	return out
}

func readLines(t *testing.T, file string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(docsDir, filepath.FromSlash(file)))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(string(b), "\n")
}

var slugRE = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// Slugs are the site's URLs (/docs/<slug>): flat, lowercase, stable, unique.
func TestManifestPagesExistWithUniqueSlugs(t *testing.T) {
	m := loadManifest(t)
	for _, s := range m.Sections {
		if s.Title == "" {
			t.Error("a section has no title")
		}
	}
	seen := map[string]string{}
	for _, p := range publishedPages(m) {
		if !slugRE.MatchString(p.Slug) {
			t.Errorf("slug %q: want lowercase words joined by -", p.Slug)
		}
		if prev, dup := seen[p.Slug]; dup {
			t.Errorf("slug %q used by both %s and %s", p.Slug, prev, p.File)
		}
		seen[p.Slug] = p.File
		if p.Title == "" {
			t.Errorf("%s: empty title", p.File)
		}
		if _, err := os.Stat(filepath.Join(docsDir, filepath.FromSlash(p.File))); err != nil {
			t.Errorf("%s: %v", p.File, err)
		}
	}
}

// A published page opens with one H1 and then a lead paragraph. The dashboard
// index and llms.txt render the lead, so it must be plain prose: not a
// heading, list, fence, table, quote, or indented code. No frontmatter either
// (GitHub renders it as a table above the page), which the H1 check also
// enforces since a frontmatter block starts with ---.
func TestPublishedPagesOpenWithH1AndLead(t *testing.T) {
	for _, p := range publishedPages(loadManifest(t)) {
		lines := readLines(t, p.File)
		if len(lines) == 0 || !strings.HasPrefix(lines[0], "# ") {
			t.Errorf("%s: first line must be an H1", p.File)
			continue
		}
		lead := ""
		for _, l := range lines[1:] {
			if strings.TrimSpace(l) != "" {
				lead = l
				break
			}
		}
		if lead == "" {
			t.Errorf("%s: no lead paragraph after the H1", p.File)
			continue
		}
		for _, bad := range []string{"#", "-", "*", "```", "|", "    ", ">", "1."} {
			if strings.HasPrefix(lead, bad) {
				t.Errorf("%s: lead must be plain prose, got %q", p.File, lead)
				break
			}
		}
	}
}

var linkRE = regexp.MustCompile(`\]\(([^)\s]+)\)`)

// Relative links resolve the way GitHub resolves them: against the page's own
// directory. Anything in the repo is fair game except docs/ops/, which is
// never published and whose readers arrive from GitHub, not the site.
func TestPublishedPagesLinkToRealFilesAndNeverIntoOps(t *testing.T) {
	for _, p := range publishedPages(loadManifest(t)) {
		inFence := false
		for n, line := range readLines(t, p.File) {
			if strings.HasPrefix(strings.TrimSpace(line), "```") {
				inFence = !inFence
				continue
			}
			if inFence {
				continue
			}
			for _, m := range linkRE.FindAllStringSubmatch(line, -1) {
				href := m[1]
				if strings.HasPrefix(href, "http://") || strings.HasPrefix(href, "https://") ||
					strings.HasPrefix(href, "#") || strings.HasPrefix(href, "mailto:") {
					continue
				}
				target, _, _ := strings.Cut(href, "#")
				rel := path.Join(path.Dir(p.File), target)
				if strings.HasPrefix(rel, "ops/") {
					t.Errorf("%s:%d: links into docs/ops/ (%s), which is never published", p.File, n+1, href)
				}
				if _, err := os.Stat(filepath.Join(docsDir, filepath.FromSlash(rel))); err != nil {
					t.Errorf("%s:%d: broken link %s (resolved to docs/%s)", p.File, n+1, href, rel)
				}
			}
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./test/docs/ -run TestManifest -v`
Expected: FAIL — `open ../../docs/manifest.json: no such file or directory`

- [ ] **Step 3: Create the empty manifest**

Create `docs/manifest.json`:

```json
{
  "sections": []
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./test/docs/ -v`
Expected: PASS for all three tests (zero pages, nothing to check).

- [ ] **Step 5: Prove the link test bites**

Temporarily add a section with a page whose file exists and contains a link into `ops/`:

```bash
mkdir -p /tmp/docs-probe && printf '# Probe\n\nLead.\n\n[x](../ops/hosted-relay.md)\n' > docs/probe.md
cat > docs/manifest.json <<'EOF'
{ "sections": [ { "title": "Probe", "pages": [ { "slug": "probe", "file": "probe.md", "title": "Probe" } ] } ] }
EOF
go test ./test/docs/ 2>&1 | grep -c 'links into docs/ops/'
```

Expected: `1`. Then restore:

```bash
rm docs/probe.md && printf '{\n  "sections": []\n}\n' > docs/manifest.json && go test ./test/docs/
```

Expected: `ok`.

- [ ] **Step 6: Commit**

```bash
gofmt -l test/docs && go vet ./test/docs/
git add test/docs/manifest_test.go docs/manifest.json
git commit -m "[docs] test/docs: manifest contract test and an empty manifest

Part of the docs organization (spec 2026-09-11). Pins the shape the
dashboard syncs from and the page rules: H1 + lead, resolvable relative
links, nothing published links into docs/ops/.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 2: `guides/install.md`

**Files:**
- Create: `docs/guides/install.md`
- Modify: `docs/manifest.json`
- Source: `docs/getting-started.md:8-95` (the `## Install` section and its four `###` channels)

**Interfaces:**
- Produces: anchors `#apt-debian-family-eg-raspberry-pi-os`, `#homebrew-macos`, `#anywhere-else-diet`, `#from-source` that other guides link to.

- [ ] **Step 1: Create the page**

Create `docs/guides/install.md` with this opening, then the four channel sections carried from `getting-started.md:22-95`:

```markdown
# Install

One command installs `piper` and `piperd` on any platform and lands you on a
real upgrade channel: apt on Debian-family Linux, Homebrew on macOS, verified
binaries everywhere else.

```bash
curl -fsSL https://get.piperbox.dev/install.sh | sh
```

It detects your platform and hands off to the native package channel where
one exists, so every install lands on a real upgrade channel. Everywhere else
it falls back to verified binaries plus printed next steps. The sections below
cover what each channel actually does, and how to skip straight to it by hand.
```

Then carry these sections, applying only the listed edits:

1. `### apt (Debian-family, e.g. Raspberry Pi OS)` — lines 22-51 verbatim. Keep the literal `apt.piperbox.dev` URLs and the sentence containing `apt install piperd piper` (pinned by `packaging/install/install_test.go` and `packaging/systemd/piperd_test.go`).
2. `### Homebrew (macOS)` — lines 53-69. Keep `brew services start piper` (pinned). Change the trailing link from `manual-setup.md#run-the-agent-on-macos-dev-box` to `../self-host/piperd.md#macos-dev-box`.
3. `### Anywhere else (diet)` — lines 71-88. Keep `--cli-only` and `--rc` (pinned). Change the final sentence to: `Then install the systemd unit by hand: see [Run piperd yourself](../self-host/piperd.md).`
4. `### From source` — lines 90-94, reworded to one sentence that keeps the pinned phrase `run piperd in Docker via Compose` verbatim:

```markdown
### From source

Prefer to build `piperd`/`piper-relay` from source, run piperd in Docker via Compose,
run the relay as a service, or wire your own automation instead of the
installer? See [Run piperd yourself](../self-host/piperd.md) and
[Run your own relay](../self-host/relay.md).
```

Drop `### Upgrading from a pre-0.15 install` (lines 96-121) entirely, including "Shell completions are a planned follow-up."

End the page with:

```markdown
Next: [First deploy](first-deploy.md).
```

- [ ] **Step 2: Add the manifest entry**

Replace `docs/manifest.json` with:

```json
{
  "sections": [
    { "title": "Guides", "pages": [
      { "slug": "install", "file": "guides/install.md", "title": "Install" }
    ]}
  ]
}
```

- [ ] **Step 3: Run the contract test**

Run: `go test ./test/docs/`
Expected: FAIL with `broken link` lines only for `first-deploy.md` (Task 3), `../self-host/piperd.md` (three links, Task 11), and `../self-host/relay.md` (Task 11). The test stays red through Task 10, each task removing its own targets from the list, and is green from Task 11 on. Confirm nothing else fails:

Run: `go test ./test/docs/ 2>&1 | grep -c 'broken link'`
Expected: `5`.

- [ ] **Step 4: Commit**

```bash
git add docs/guides/install.md docs/manifest.json
git commit -m "[docs] guides/install: split the install channels out of getting-started

Drops the pre-0.15 upgrade section (pre-1.0: old installs are unsupported).

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 3: `guides/first-deploy.md` (new page)

**Files:**
- Create: `docs/guides/first-deploy.md`
- Modify: `docs/manifest.json`
- Verify against: `cmd/piper/main.go` (`create`, `deploy`, `list`, `status`, `stop`, `start`, `delete` cases), `cmd/piper/env.go`, `internal/runtime/docker.go:104-120` (`WaitHealthy`), `internal/deploy/deploy.go:202-225` (progress lines).

- [ ] **Step 1: Create the page**

Create `docs/guides/first-deploy.md` with exactly this content:

```markdown
# First deploy

Register an app, deploy a directory that holds a Dockerfile, and open it at
`http://<name>.piper.localhost` — all on the box, with no login and no relay.

## Create and deploy

```bash
piper create blog --port 8080   # the port your container listens on (default 8080)
piper deploy blog --path .      # build the Dockerfile in ., run it, health-check, route
```

`deploy` follows the build and prints each stage as it happens:

```
→ building image
→ starting container
→ health-checking
deployed blog: http://blog.piper.localhost (running)
```

The health check is deliberately simple: the container must accept a TCP
connection on its port within 30 seconds. No HTTP path, no status code. A
container that never opens the port ends the deployment as `failed`, and the
previous deployment, if any, keeps serving.

`--path` defaults to the current directory. `--timeout` (default 15 minutes)
bounds how long the CLI follows the deploy; the build itself continues on the
box if the CLI gives up. An app that has been linked to a GitHub repo
([Git deploys](git-deploys.md)) deploys from that repo when `--path` is
omitted.

## Look around

```bash
piper list      # name, port, URL per app
piper status    # the same, plus each app's status and the daemon's version
```

Deployment status is one of `building`, `running`, `failed`, `stopped`.
Bare `piper` opens the [TUI](tui.md), which adds deployment history and live
logs per app.

## Stop, start, delete

```bash
piper stop blog          # stop the container and drop its routes; history stays
piper start blog         # run the last deployment again
piper delete blog --yes  # remove the app, its containers, routes, and images
```

## Environment variables

```bash
piper env blog set DATABASE_URL=postgres://… LOG_LEVEL=debug
piper env blog ls            # names and ages; --show prints the values
piper env blog rm LOG_LEVEL
```

Values are stored on the box and applied on the app's next deploy or restart,
never by bouncing the running container.

## Reaching the app

`*.piper.localhost` resolves to loopback on most systems without any
configuration. Your user must be able to reach a Docker socket: be in the
`docker` group, or set `DOCKER_HOST`. To reach the box from another machine,
see [LAN control](lan-control.md); for a public HTTPS URL, see
[Join the public relay](relay-login.md).
```

These comments match `Deployer.Stop` (stops the container, removes routes, marks the deployment `stopped`, keeps the app and history) and `Deployer.Delete` (stops every deployment, removes routes, deletes the row, prunes the app's images) in `internal/deploy/deploy.go:604-775`. If either function has changed when you implement this, follow the code.

- [ ] **Step 2: Add the manifest entry**

Append to the `Guides` section's `pages` array in `docs/manifest.json`:

```json
      { "slug": "first-deploy", "file": "guides/first-deploy.md", "title": "First deploy" }
```

- [ ] **Step 3: Run the contract test**

Run: `go test ./test/docs/ 2>&1 | grep 'broken link'`
Expected: `first-deploy.md` no longer listed; new broken links are `git-deploys.md`, `tui.md`, `lan-control.md`, `relay-login.md` (Tasks 4–8), plus install.md's two `self-host` targets.

- [ ] **Step 4: Commit**

```bash
git add docs/guides/first-deploy.md docs/manifest.json
git commit -m "[docs] guides/first-deploy: the LAN-only create → deploy → lifecycle walkthrough

New page; the README quick start was the only place this lived.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 4: `guides/tui.md`

**Files:**
- Create: `docs/guides/tui.md`
- Modify: `docs/manifest.json`
- Source: `docs/getting-started.md:123-146`

- [ ] **Step 1: Create the page**

```markdown
# The interactive TUI

Bare `piper` in a terminal opens a full-screen control surface: apps, deploys,
logs, boxes, and the login and GitHub wizards, all interactive. Every
subcommand stays scriptable and unchanged.
```

Then carry lines 126-146 with these edits: drop the first sentence of the paragraph (`piper` is dual-mode …) since the lead now says it; change `Run it on the box and it's authless (see below)` to `Run it on the box and it needs no login ([LAN control](lan-control.md) explains why)`; change the remote sentence to link `[Remote control](remote-control.md)`.

- [ ] **Step 2: Manifest entry**

Append to `Guides.pages`:

```json
      { "slug": "tui", "file": "guides/tui.md", "title": "The TUI" }
```

- [ ] **Step 3: Test** — `go test ./test/docs/ 2>&1 | grep 'broken link'`: `tui.md` gone from the list.

- [ ] **Step 4: Commit**

```bash
git add docs/guides/tui.md docs/manifest.json
git commit -m "[docs] guides/tui: split out of getting-started

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 5: `guides/lan-control.md`

**Files:**
- Create: `docs/guides/lan-control.md`
- Modify: `docs/manifest.json`
- Source: `docs/getting-started.md:147-178`

- [ ] **Step 1: Create the page**

```markdown
# Drive piperd from another machine on the LAN

On the box itself the CLI needs no login. To drive the box from a laptop on the
same network, open the control API off loopback, mint a token on the box, and
log the CLI in once.
```

Then carry lines 149-178 verbatim (they already read as a sequence). Keep the literal `PIPER_ADDR` and `PIPER_TOKEN` mentions (`PIPER_ADDR` is pinned by `packaging/install/install_test.go`). Replace the `## Drive piperd…` H2 with `## Open the API and mint a token` above the `To reach the control API…` paragraph, and put the `**On the box itself…**` and `Once the API leaves loopback…` paragraphs directly under the lead, with no heading between.

- [ ] **Step 2: Manifest entry**

```json
      { "slug": "lan-control", "file": "guides/lan-control.md", "title": "LAN control" }
```

- [ ] **Step 3: Test** — `go test ./test/docs/ 2>&1 | grep 'broken link'`: `lan-control.md` gone.

- [ ] **Step 4: Commit**

```bash
git add docs/guides/lan-control.md docs/manifest.json
git commit -m "[docs] guides/lan-control: split out of getting-started

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 6: `guides/relay-login.md`

**Files:**
- Create: `docs/guides/relay-login.md`
- Modify: `docs/manifest.json`
- Source: `docs/getting-started.md:179-266` (`## Join the public relay`, `### List and remove boxes`)
- Verify against: `cmd/piper/relayonboard.go` and `cmd/piper/enrollflow.go` for the flag list at lines 224-234.

- [ ] **Step 1: Create the page**

```markdown
# Join the public relay

One command on the box signs you in with GitHub and claims the box on the
public relay: your apps get `https://<hash>-<you>.public.getpiper.dev` URLs
with no port forwarding and no domain of your own.
```

Then carry lines 181-266 with these edits:

- Line 179's H2 becomes `## One command`; the `### List and remove boxes` heading stays as `## List and remove boxes`.
- The paragraph at lines 220-224 (`Run login on the box…`) links `[Remote control](remote-control.md)` instead of the in-page anchor.
- The paragraph at lines 246-251 (`Bring-your-own-domain apps stay end-to-end…`) moves to the **end** of the page under `## Your own domain instead`, and its links become `[Custom domains](custom-domains.md)` and `[Direct serve](direct-serve.md)`. Keep the sentence about `PIPER_RELAY_TLS_CERT`/`KEY` and change its subject to link `[Run your own relay](../self-host/relay.md)`.
- The `piper login --relay <url>` paragraph (lines 242-244) stays where it is.

- [ ] **Step 2: Manifest entry**

```json
      { "slug": "relay-login", "file": "guides/relay-login.md", "title": "Join the public relay" }
```

- [ ] **Step 3: Test** — `go test ./test/docs/ 2>&1 | grep 'broken link'`: `relay-login.md` gone; `custom-domains.md`, `direct-serve.md`, `remote-control.md` appear until Tasks 7, 9, 10.

- [ ] **Step 4: Commit**

```bash
git add docs/guides/relay-login.md docs/manifest.json
git commit -m "[docs] guides/relay-login: split out of getting-started

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 7: `guides/remote-control.md`

**Files:**
- Create: `docs/guides/remote-control.md`
- Modify: `docs/manifest.json`
- Source: `docs/getting-started.md:267-286`

- [ ] **Step 1: Create the page**

```markdown
# Drive a box remotely

Any control command can target one of your relay-connected boxes from
anywhere, by the base domain `piper login` printed. Requests travel relay →
tunnel → box; your relay credential never reaches the box.
```

Then carry lines 269-286 verbatim, minus the first sentence (now the lead). Add a closing line: `Bare `piper --remote <base-domain>` opens the [TUI](tui.md) against that box.`

- [ ] **Step 2: Manifest entry**

```json
      { "slug": "remote-control", "file": "guides/remote-control.md", "title": "Remote control" }
```

- [ ] **Step 3: Test** — `remote-control.md` gone from the broken-link list.

- [ ] **Step 4: Commit**

```bash
git add docs/guides/remote-control.md docs/manifest.json
git commit -m "[docs] guides/remote-control: split out of getting-started

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 8: `guides/git-deploys.md`

**Files:**
- Create: `docs/guides/git-deploys.md`
- Modify: `docs/manifest.json`
- Source: `docs/getting-started.md:305-354` (`## Git deploys`, `### Self-hosted relay / bring-your-own GitHub App`)

- [ ] **Step 1: Create the page**

```markdown
# Git deploys

Once a box has joined the relay, a `git push` builds and publishes an app.
The hosted relay holds one shared GitHub App on everyone's behalf, so there is
nothing to create: `piper login` installs it on the repos you choose.
```

Then carry lines 307-354 with these edits:

- The first paragraph loses its opening sentence (now the lead); the `bash` block and the two paragraphs after it stay.
- `### Self-hosted relay / bring-your-own GitHub App` becomes `## Self-hosted relay or your own GitHub App`.
- The final paragraph (lines 351-354) links into `ops/`, which the contract test forbids. Replace it with: `Standing either path up against a real relay, domain, and GitHub App end to end is covered by the repo's [self-host docs](../self-host/relay.md).`
- Add one sentence after the bash block in the first section: `Pull requests get their own preview URL, `pr-<N>-<app>.<base>`, torn down when the PR closes.` (hostname shape from `internal/deploy/deploy.go:137`, `pr-%d-%s.%s`).

- [ ] **Step 2: Manifest entry**

```json
      { "slug": "git-deploys", "file": "guides/git-deploys.md", "title": "Git deploys" }
```

- [ ] **Step 3: Test** — `git-deploys.md` gone from the broken-link list.

- [ ] **Step 4: Commit**

```bash
git add docs/guides/git-deploys.md docs/manifest.json
git commit -m "[docs] guides/git-deploys: split out of getting-started

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 9: `guides/custom-domains.md` (relay mode)

**Files:**
- Create: `docs/guides/custom-domains.md`
- Modify: `docs/manifest.json`
- Source: `docs/custom-domains.md:1-31` (intro, box-wide via API), `:55-82` (env vars, precedence), `:83-129` (per-app, relay mode), and `docs/getting-started.md:287-303` (folded in)

- [ ] **Step 1: Create the page**

```markdown
# Custom domains

Serve every app under a domain you own with one wildcard cert, or attach one
domain to one app with a single CNAME. TLS ends on the box either way; the
relay only splices bytes by SNI.
```

Then carry the source with these edits:

- The `Two kinds:` list (lines 3-9) stays, with its per-app link pointing at `#per-app-domains-piper-domains`.
- `## Box-wide base domain` and `### Via the control API…` (lines 11-31) carry verbatim.
- **Omit** `### Direct serve` (lines 32-54); it moves to Task 10.
- `### Via environment variables…` (lines 55-73): keep the first paragraph. Replace the second paragraph (`**A box that has never been enrolled can do this too.**…`) with one sentence: `A box with a public IP can skip the relay entirely and serve its own `:443`: see [Direct serve](direct-serve.md).`
- `### Precedence` (lines 74-82): carry verbatim.
- `## Per-app domains (`piper domains`)` (lines 83-129): carry, but the two-bullet mode list becomes one paragraph for relay mode, ending with `On a box that serves direct the same commands print an A record instead: see [Direct serve](direct-serve.md#per-app-domains-on-a-direct-box).` The `Notes:` list carries verbatim.
- **Omit** `### Direct-served boxes` (lines 131-153); it moves to Task 10.
- Nothing from `getting-started.md:287-303` needs carrying beyond what the per-app section already says; drop it.

- [ ] **Step 2: Manifest entry**

```json
      { "slug": "custom-domains", "file": "guides/custom-domains.md", "title": "Custom domains" }
```

- [ ] **Step 3: Test** — `custom-domains.md` gone from the broken-link list; `direct-serve.md` remains until Task 10.

- [ ] **Step 4: Commit**

```bash
git add docs/guides/custom-domains.md docs/manifest.json
git commit -m "[docs] guides/custom-domains: relay-mode domains, direct serve split out

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 10: `guides/direct-serve.md`

**Files:**
- Create: `docs/guides/direct-serve.md`
- Modify: `docs/manifest.json`
- Source: `docs/custom-domains.md:32-54` (Direct serve), `:60-73` (never-enrolled paragraph), `:131-153` (Direct-served boxes)
- Verify against: `cmd/piperd/main.go` (`serveMode`, `directServe`, `publicHTTPS`, `terminatesTLS`), `internal/domain/domain.go` (`boxServe`, `hasAppDNSSource`), `internal/domain/appdomain.go` (`appDomainStatus`, the direct branch of `appIssueOnce`).

- [ ] **Step 1: Create the page**

```markdown
# Direct serve

A box with a public IP can terminate its own HTTPS on `:443` and skip the
relay's splice: point DNS at the box, keep the relay for login and webhooks —
or run with no relay at all.

## Box-wide domain
```

Then carry:

1. Lines 34-54 (the `### Direct serve` body) under `## Box-wide domain`, with `"serve"` on `PUT /v1/domain` linked as `[the box-wide domain API](custom-domains.md#via-the-control-api-dashboard--curl--relay-free-tier-boxes)`.
2. A `## A box that was never enrolled` H2 holding lines 61-73 (the `**A box that has never been enrolled can do this too.**` paragraph), with its link to `#direct-served-boxes` becoming `#per-app-domains-on-a-direct-box`.
3. A `## Per-app domains on a direct box` H2 holding lines 133-153 (the `### Direct-served boxes` body), opening sentence changed to `On a box whose serve mode is `direct`, [`piper domains`](custom-domains.md#per-app-domains-piper-domains) and the API are the same as in relay mode, with three differences:`.

Nothing in this page links into `self-host/`; the env-managed form (`PIPER_SERVE=direct` next to `PIPER_BASE_DOMAIN`) is already described in the carried text.

- [ ] **Step 2: Manifest entry**

```json
      { "slug": "direct-serve", "file": "guides/direct-serve.md", "title": "Direct serve" }
```

- [ ] **Step 3: Test**

Run: `go test ./test/docs/ 2>&1 | grep 'broken link'`
Expected: only the `../self-host/piperd.md` and `../self-host/relay.md` targets remain (from Tasks 2, 6, 8).

- [ ] **Step 4: Commit**

```bash
git add docs/guides/direct-serve.md docs/manifest.json
git commit -m "[docs] guides/direct-serve: direct mode gets its own page

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 11: `self-host/piperd.md` and `self-host/relay.md`

**Files:**
- Create: `docs/self-host/piperd.md`
- Create: `docs/self-host/relay.md`
- Source: `docs/manual-setup.md` (all), `docs/runbooks/relay-deploy.md:19-484` and `:606-738`

Neither file goes in the manifest (repo-only). The contract test therefore does not read them, but the pinned phrases below are asserted by Task 12's repointed tests, so keep them verbatim.

- [ ] **Step 1: Create `docs/self-host/piperd.md`**

```markdown
# Run piperd yourself

`apt install piperd piper` and `brew install piperbox/tap/piper` do everything
on this page for you. Use it when you build from source, run a non-Debian
distro, run piperd in Docker, or wire your own automation.
```

Then carry `manual-setup.md`:

- `## Run the agent as a service (manual / from source)` (lines 8-55) → `## Linux: systemd from source`. Keep verbatim: `systemctl enable --now piperd`, `piperd.service`, and the sentence naming `/etc/piper/piperd.env` with `PIPER_SERVE=direct`; change its link to `../guides/direct-serve.md`. Change the runbook link at line 54 to `Verification, logs, and teardown are walked through in the repo's [e2e runbook](../ops/e2e-runbook.md).` (self-host pages are not published, so a link into `ops/` is allowed here).
- `## Run the agent on macOS (dev box)` (lines 56-70) → `## macOS dev box`. The anchor `#macos-dev-box` is what Task 2 linked.
- `## Run piperd in Docker (Compose)` (lines 71-105) → `## Docker Compose`. Keep verbatim: `docker compose -f deploy/compose/docker-compose.yml up -d --build`, `network_mode: host`, `root-equivalent`.

- [ ] **Step 2: Create `docs/self-host/relay.md`**

```markdown
# Run your own relay

`piper-relay` is the same open-source binary the hosted relay runs. This page
installs it as a systemd service or a container, configures it against
Postgres, and upgrades it — including across a schema change.
```

Then, in this order:

1. `## Install as a service` — `manual-setup.md:106-135` (`## Run the relay as a service`). Keep verbatim: `packaging/systemd/piper-relay.service`, `systemctl enable --now piper-relay`. Its two links become `#configure` (in-page) and `../ops/e2e-runbook.md#part-b--relay`.
2. `## What's on disk` — `relay-deploy.md:19-42` verbatim.
3. `## Fresh deploy` with `### 1. Install the binary and unit`, `### 2. Configure`, `### 3. Start and verify` — `relay-deploy.md:44-153` verbatim. `### 2. Configure`'s anchor is `#2-configure`; step 1 above links `#configure`, so rename the heading to `### Configure` and the other two to `### Install the binary and unit` / `### Start and verify` (anchors `#install-the-binary-and-unit`, `#configure`, `#start-and-verify`).
4. `## Upgrade, same schema` and `## Upgrade across a schema change` with its two `###` — `relay-deploy.md:154-296` verbatim, except the link at line 252 (`../manual-setup.md#run-the-relay-as-a-service`) becomes `#install-as-a-service`.
5. `## Ops surface` — `relay-deploy.md:297-308` verbatim.
6. `## Run as a container` — `relay-deploy.md:309-369` verbatim.
7. `## Scale out` — `relay-deploy.md:370-484` verbatim, **stopping before** `### Kubernetes`. The link at line 374 to the edge-ownership spec keeps its `../superpowers/…` form (it resolves from `self-host/` exactly as it did from `runbooks/`).
8. `## Single host with compose` — `relay-deploy.md:606-738` verbatim; its link `../../deploy/compose/relay/docker-compose.yml` resolves unchanged.
9. Close with: `The piperbox-hosted relay's own layout — k3s, Flux, the Hetzner host — is documented for its operators in [ops/hosted-relay.md](../ops/hosted-relay.md).`

- [ ] **Step 3: Run the contract test**

Run: `go test ./test/docs/`
Expected: PASS — every `../self-host/…` link from the guides now resolves. Also run `go test ./packaging/... ./deploy/...` and expect PASS (old files still exist, old tests untouched).

- [ ] **Step 4: Commit**

```bash
git add docs/self-host/
git commit -m "[docs] self-host: piperd and relay pages from manual-setup and the relay runbook

Repo-only for now; one manifest line publishes either.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 12: `ops/`, delete the old files, repoint every consumer

This is one commit on purpose: the old paths disappear and the tests and scripts that read them change together, so the tree is never red.

**Files:**
- Create: `docs/ops/hosted-relay.md` (from `docs/runbooks/relay-deploy.md:1-18` and `:485-605`)
- Move: `docs/runbooks/git-deploy-e2e.md` → `docs/ops/e2e-runbook.md`
- Delete: `docs/getting-started.md`, `docs/custom-domains.md`, `docs/manual-setup.md`, `docs/runbooks/relay-deploy.md`, `docs/runbooks/`
- Modify: `packaging/systemd/piperd_test.go:64-85`, `packaging/systemd/piper-relay_test.go:57-75`, `packaging/install/install_test.go:674-680,690-711`, `deploy/compose/compose_test.go:63-81`, `install.sh:217`, `deploy/compose/relay/docker-compose.yml:2`, `.claude/skills/release/SKILL.md:52`

- [ ] **Step 1: Create `docs/ops/hosted-relay.md`**

```markdown
# The hosted relay

How piperbox runs `public.getpiper.dev`: a single-node k3s cluster on a
Hetzner host, reconciled by Flux from the private `piperbox/relay-ops` repo.
Generic relay install and upgrade steps live in
[self-host/relay.md](../self-host/relay.md); this page is only what differs
for our own deployment.
```

Then carry, verbatim: `relay-deploy.md:485-566` (`### Kubernetes`, promoted to `## Kubernetes`) and `:567-605` (`### Rolling out with Flux`, promoted to `## Rolling out with Flux`). Every in-page link to a section that now lives in `self-host/relay.md` (search the carried text for `](#`) becomes `../self-host/relay.md#<anchor>`. Do **not** carry lines 1-18 as prose; the lead above replaces them.

- [ ] **Step 2: Move the e2e runbook and delete the old files**

```bash
git mv docs/runbooks/git-deploy-e2e.md docs/ops/e2e-runbook.md
git rm -q docs/getting-started.md docs/custom-domains.md docs/manual-setup.md docs/runbooks/relay-deploy.md
```

In `docs/ops/e2e-runbook.md`, fix its three relative links: line 107 `relay-deploy.md#2-configure` → `../self-host/relay.md#configure`; line 330 `../getting-started.md` → `../guides/install.md`; line 532 `../manual-setup.md#run-the-agent-on-macos-dev-box` → `../self-host/piperd.md#macos-dev-box`.

- [ ] **Step 3: Repoint the pinned tests**

`packaging/systemd/piperd_test.go` — `TestPiperdDocumentation` reads three files; replace the body's file loads and assertions:

```go
	install := repositoryFile(t, "docs", "guides", "install.md")
	selfHost := repositoryFile(t, "docs", "self-host", "piperd.md")
	readme := repositoryFile(t, "README.md")

	for name, doc := range map[string]string{"guides/install": install, "self-host/piperd": selfHost, "README": readme} {
		if strings.Contains(doc, "daemonize") {
			t.Errorf("%s still mentions daemonize", name)
		}
		if strings.Count(doc, "systemctl --user") > 1 {
			t.Errorf("%s documents the rootless user unit beyond the one-time cleanup note", name)
		}
	}
	for _, want := range []string{"apt install piperd", "brew services start piper"} {
		if !strings.Contains(install, want) {
			t.Errorf("guides/install.md missing %q", want)
		}
	}
	if !strings.Contains(selfHost, "systemctl enable --now piperd") {
		t.Errorf("self-host/piperd.md missing the manual unit-install command")
	}
```

`packaging/systemd/piper-relay_test.go` — `TestServiceDocumentation`: `repositoryFile(t, "docs", "manual-setup.md")` → `repositoryFile(t, "docs", "self-host", "relay.md")` and the error string `docs/manual-setup.md` → `docs/self-host/relay.md`; `repositoryFile(t, "docs", "runbooks", "git-deploy-e2e.md")` → `repositoryFile(t, "docs", "ops", "e2e-runbook.md")`.

`packaging/install/install_test.go` — in the diet next-steps assertion (line 677) `"docs/manual-setup.md"` → `"docs/self-host/piperd.md"`. In `TestInstallDocumentation`, replace the map keys and split the getting-started list by where each phrase now lives:

```go
		filepath.Join("docs", "guides", "install.md"): {
			"apt.piperbox.dev",
			"brew services start piper",
			"--cli-only",
			"--rc",
		},
		filepath.Join("docs", "guides", "lan-control.md"): {
			"PIPER_ADDR",
		},
		filepath.Join("docs", "self-host", "piperd.md"): {
			"systemctl enable --now piperd",
			"piperd.service",
		},
```

and update the comment above it to name `docs/guides/install.md`.

`deploy/compose/compose_test.go` — `TestDockerDocumentation`: `repositoryFile(t, "docs", "manual-setup.md")` → `repositoryFile(t, "docs", "self-host", "piperd.md")` with the error string updated; the pointer-phrase block reads `guides/install.md`:

```go
	guide := repositoryFile(t, "docs", "guides", "install.md")
	if !strings.Contains(guide, "run piperd in Docker via Compose") {
		t.Errorf("docs/guides/install.md missing pointer phrase %q", "run piperd in Docker via Compose")
	}
```

- [ ] **Step 4: Repoint the scripts and skill**

- `install.sh:217`: `(details: docs/manual-setup.md)` → `(details: docs/self-host/piperd.md)`.
- `deploy/compose/relay/docker-compose.yml:2`: `docs/runbooks/relay-deploy.md` → `docs/self-host/relay.md`.
- `.claude/skills/release/SKILL.md:52`: `../../../docs/runbooks/relay-deploy.md#rolling-out-with-flux` → `../../../docs/ops/hosted-relay.md#rolling-out-with-flux`.

- [ ] **Step 5: Run every affected test**

Run: `go test ./test/docs/ ./packaging/... ./deploy/...`
Expected: PASS. Then: `grep -rn 'getting-started.md\|manual-setup.md\|custom-domains.md\|runbooks/' --include='*.go' --include='*.sh' --include='*.yml' --include='*.md' . | grep -v '^./docs/superpowers/'`
Expected: only `README.md` and `CLAUDE.md` hits remain (Task 13).

- [ ] **Step 6: Commit**

```bash
git add -A docs install.sh deploy/compose/relay/docker-compose.yml .claude/skills/release/SKILL.md packaging deploy/compose/compose_test.go
git commit -m "[docs] ops/: hosted-relay and e2e runbook; delete the old doc paths; repoint tests

The tests that read getting-started, manual-setup, and the runbooks now read
the pages those phrases moved to. install.sh's diet next-steps point at
self-host/piperd.md.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 13: The map, README, CLAUDE.md, PROGRESS.md

**Files:**
- Create: `docs/README.md`
- Modify: `README.md:46,71,93-100`
- Modify: `CLAUDE.md:14`
- Modify: `PROGRESS.md` (Foundation section, after line 20)

- [ ] **Step 1: Create `docs/README.md`**

```markdown
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
```

- [ ] **Step 2: README.md**

- Line 46: `the full walkthrough: [`docs/getting-started.md`](docs/getting-started.md).` → `the guides: [`docs/guides/`](docs/guides/), starting at [Install](docs/guides/install.md).`
- Line 71: `See [`docs/custom-domains.md`](docs/custom-domains.md).` → `See [Custom domains](docs/guides/custom-domains.md) and [Direct serve](docs/guides/direct-serve.md).`
- Replace the `## Docs` table (lines 93-100) with:

```markdown
## Docs

| | |
| --- | --- |
| [Guides](docs/guides/install.md) | install → first deploy → TUI → relay → remote control → git deploys → domains |
| [Self-host](docs/self-host/piperd.md) | run piperd from source or in Docker; run your own relay |
| [Docs map](docs/README.md) | which folder serves whom, how to add a page |
| [PROGRESS.md](PROGRESS.md) | built vs. stubbed map, linked to issues |
| [Design](docs/superpowers/specs/2026-07-04-piper-design.md) | the full design rationale |
```

- [ ] **Step 3: CLAUDE.md**

After the sentence ending `Plan 3 = GitHub webhook + PR-preview URLs.` on line 14, append one sentence: `User-facing docs are organized by audience under [`docs/`](docs/README.md); read that map before adding or moving a doc.`

- [ ] **Step 4: PROGRESS.md**

After line 20 (the last `Foundation` bullet), add:

```markdown
- ✅ Docs organized by audience (`guides/`, `reference/`, `self-host/`, `ops/`) with `docs/manifest.json` as the publish list and a `test/docs` contract test — [spec](docs/superpowers/specs/2026-09-11-docs-organization-design.md); reference pages and the dashboard sync follow
```

Update the `_Last updated:` line's date to today and prepend a clause: `docs organized by audience with a manifest and contract test ([spec](docs/superpowers/specs/2026-09-11-docs-organization-design.md)). Earlier: ` before the existing text.

- [ ] **Step 5: Verify no old path survives outside history**

Run: `grep -rn 'getting-started.md\|manual-setup.md\|docs/custom-domains.md\|runbooks/' --include='*.go' --include='*.sh' --include='*.yml' --include='*.md' . | grep -v '^./docs/superpowers/'`
Expected: no output.

- [ ] **Step 6: Commit**

```bash
git add docs/README.md README.md CLAUDE.md PROGRESS.md
git commit -m "[docs] docs/README map; README, CLAUDE.md, PROGRESS point at the new tree

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 14: Verify and open the PR

**Files:** none new.

- [ ] **Step 1: Full gate**

Run: `make verify; echo EXIT=$?`
Expected: `EXIT=0`. If gofmt flags `test/docs/manifest_test.go`, run `make fmt` and amend the Task 1 commit's formatting into a new commit rather than rewriting history.

- [ ] **Step 2: Read every published page once, top to bottom**

Run: `for f in $(python3 -c "import json;print(' '.join(p['file'] for s in json.load(open('docs/manifest.json'))['sections'] for p in s['pages']))"); do echo "== $f"; head -5 docs/$f; done`
Expected: nine pages, each showing an H1 and a prose lead. Then open each file and check that no sentence refers to "above" or "below" content that now lives on another page; fix wording in place and commit as `[docs] guides: fix dangling cross-references`.

- [ ] **Step 3: Push and open the PR**

```bash
git push -u origin ozykhan/docs-organization
gh pr create --base main --title "[docs] organize docs by audience: guides, self-host, ops, manifest, contract test" --body-file - <<'EOF'
## Summary

PR 1 of 3 for the docs organization spec (`docs/superpowers/specs/2026-09-11-docs-organization-design.md`). `docs/` is now split by audience, with `docs/manifest.json` as the single owner of what the site publishes and a `test/docs` contract test pinning the page rules.

## Changes

- `guides/` — nine published pages split out of getting-started and custom-domains; `first-deploy.md` is new; the pre-0.15 upgrade section is dropped.
- `self-host/` — `piperd.md` (from manual-setup) and `relay.md` (manual-setup + the generic parts of the relay runbook). Repo-only for now.
- `ops/` — `hosted-relay.md` (k3s/Flux/Hetzner from the relay runbook) and `e2e-runbook.md` (moved).
- `docs/README.md` map, `manifest.json`, `test/docs/manifest_test.go`.
- Every Go test and script that read an old path now reads the new one; old paths are deleted, not redirected (pre-1.0).

## Testing

`make verify` green. The contract test was mutation-checked: a page linking into `ops/` fails it.

## Follow-ups

- PR 2: `reference/` pages with drift tests.
- PR 3: dashboard manifest-driven sync, sections, `llms.txt`, raw routes.
- Memory notes pointing at `runbooks/relay-deploy.md` repoint to `ops/hosted-relay.md`.
- `piper deploy`'s timeout hint says "check `piper app <name>`", but `piper app` only has `link`. Small CLI fix, own issue.

🤖 Generated with [Claude Code](https://claude.com/claude-code)
EOF
```

- [ ] **Step 4: Repoint memory**

In `/Users/fco/.claude/projects/-Users-fco-Documents-projects-getpiper-piper/memory/`, edit `hetzner-relay-deployment.md` and `relay-k8s-readiness-epic.md` so any mention of `docs/runbooks/relay-deploy.md` reads `docs/ops/hosted-relay.md`. No MEMORY.md index change is needed.
