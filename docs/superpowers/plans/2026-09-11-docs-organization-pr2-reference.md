# Docs organization PR 2: reference pages and drift tests

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add the three published reference pages (`docs/reference/cli.md`, `env.md`, `api.md`) and the three `test/docs` drift tests that fail when a CLI verb, env var, or control-API route changes without its page.

**Architecture:** One helper in `test/docs` parses a package directory with `go/parser`, skips `_test.go`, and returns the string literals matching a predicate. Each drift test is that helper plus one predicate plus one `strings.Contains` check against a page. The pages are hand-written now, in a layout a future generator can reproduce (spec § Reference pages). This is PR 2 of the docs organization spec; PR 1 (#561, merged as db4915e) built the tree, guides, manifest, and the manifest contract test.

**Tech Stack:** Go 1.26 standard library only (`go/ast`, `go/parser`, `go/token`, `strconv`, `regexp`). Markdown. No new dependencies.

**Spec:** [`docs/superpowers/specs/2026-09-11-docs-organization-design.md`](../specs/2026-09-11-docs-organization-design.md) § Reference pages, § `test/docs` package, § Rollout item 2.

## Global Constraints

Copied from the spec and the repo rules; every task's requirements include these.

- **Branch and base.** Work on `ozykhan/docs-reference`, branched from `origin/main` at `db4915e` (PR 1 merged). Never commit to `main`. Squash-merge at the end.
- **Worktree.** All commands run in the current worktree. Never `cd` to the main checkout. Never bare `git stash` / `git stash pop`.
- **Files under `docs/superpowers/` are history.** Never edit any file there other than this plan's own checkboxes.
- **Page rules (contract-tested by `test/docs/manifest_test.go`, already merged).** A published page is exactly one `# H1` as line 1, then a lead paragraph of one to three sentences of plain prose (no heading, list, table, fence, quote, or indented code as the first non-blank line after the H1). No frontmatter. Cross-links are relative `.md` paths resolved against the page's own directory, with GitHub-slug anchors: `env.md#piperd` within `reference/`, `../guides/tui.md` across. A published page never links into `ops/`. No second `# ` line anywhere outside a code fence.
- **Manifest shape.** `docs/manifest.json` is `{"sections":[{"title","pages":[{"slug","file","title"}]}]}`, `file` relative to `docs/`. The three reference pages go in a new section titled `Reference`, after `Guides`, slugs `cli`, `env`, `api`, in that order.
- **Reference page layouts (spec § Reference pages, verbatim).**
  - `reference/cli.md` — one `##` per verb, one `###` per subcommand. Each entry opens with the synopsis exactly as the binary prints it (the text after `usage: `), then a flags table, what the command does, and the exit code (0 ok, 1 error, 2 usage).
  - `reference/env.md` — one table per binary: piperd, piper-relay, piper-edge, and the CLI's own (`PIPER_REMOTE`, `PIPER_NO_BROWSER`, …). Columns: name, default, meaning. Test-only knobs (`PIPER_TEST_ISSUER`, `PIPER_RELAY_FAKE_APPROVE`) get a marked row, not an exemption list. `packaging/systemd/piperd.env.example` stays the curated subset and is not edited.
  - `reference/api.md` — one `###` per route headed `METHOD /v1/path`: auth requirement, request and response JSON shapes from the types in `internal/api`, and the error statuses the handler returns.
- **Drift tests (spec § `test/docs` package).** One helper serves all three: parse a package directory with `go/parser`, skip `_test.go`, collect string literals matching a predicate. CLI: every literal in `cmd/piper` beginning with `usage: piper` appears, minus the `usage: ` prefix, verbatim in `reference/cli.md`. Env: every literal matching `^PIPER_[A-Z_]+$` appears backticked in `reference/env.md`. API: every route literal registered on the mux in `internal/api` appears as a `###` heading in `reference/api.md`. The tests deliberately do not check prose, flag descriptions, or JSON field lists.
- **Env scan directories.** The spec lists `internal/config`, `cmd/piper`, `cmd/piper-relay`, `cmd/piper-edge`. This plan adds `cmd/piperd`: the two test-only knobs the spec itself names as marked rows, `PIPER_TEST_ISSUER` and `PIPER_SKIP_CADDY`, are read only there. `internal/relay/relaytest` (`PIPER_TEST_POSTGRES_URL`) is a test helper package, not a binary, and stays out.
- **Mutation check before merge.** Each drift test is proven by adding a fake verb, env var, or route, watching the test fail, and reverting. The step is in each task; the commit never contains the mutation.
- **Facts come from code.** Every synopsis, default, status code, and JSON field on these pages is read from the named source file, not from memory or from older docs. Where this plan supplies a table, the implementer still checks each row against the file it names.
- **Verification.** `make verify` (gofmt → vet → `go test ./...` → arm64 cross-build) must exit 0 before any task is reported done. Judge it by exit status, not by grepping output.
- **Commits.** One commit per task step where the plan says commit, conventional-commit style, ending with the trailer `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>`. Reference the spec in the PR body; the PR body ends with `🤖 Generated with [Claude Code](https://claude.com/claude-code)`.
- **Surgical.** Touch only the files each task names. No edits to guides, self-host, or ops pages. No code changes under `cmd/` or `internal/` (the two CLI wording bugs noted in "After merge" are filed as issues, not fixed here).

---

## File structure

| File | Responsibility |
| --- | --- |
| `test/docs/literals_test.go` (new) | `repoRoot`, `stringLiterals(t, dir, keep)`, `readDoc(t, file)` — the one helper the three drift tests share |
| `test/docs/cli_drift_test.go` (new) | CLI usage-line drift test |
| `test/docs/env_drift_test.go` (new) | env var drift test |
| `test/docs/api_drift_test.go` (new) | route drift test |
| `test/docs/manifest_test.go` (exists) | untouched; its `docsDir` const is reused, and its three tests start covering the new pages the moment they enter the manifest |
| `docs/reference/cli.md` (new) | every `piper` verb and subcommand |
| `docs/reference/env.md` (new) | every `PIPER_*` variable, one table per binary |
| `docs/reference/api.md` (new) | every control-API route |
| `docs/manifest.json` | gains the `Reference` section |
| `docs/README.md`, `README.md`, `PROGRESS.md` | one-line updates: reference is now published |

---

### Task 1: literal helper, CLI drift test, `reference/cli.md`

**Files:**
- Create: `test/docs/literals_test.go`
- Create: `test/docs/cli_drift_test.go`
- Create: `docs/reference/cli.md`
- Modify: `docs/manifest.json`
- Read (source of truth for the page): `cmd/piper/main.go`, `cmd/piper/agent.go`, `cmd/piper/box.go`, `cmd/piper/domains.go`, `cmd/piper/env.go`, `internal/config/config.go:333-362` (`LoadClient`), `internal/relayclient/relayclient.go:69` (`DefaultAPI`)

**Interfaces:**
- Consumes: `docsDir` (`"../../docs"`) from `test/docs/manifest_test.go`, package `docs_test`.
- Produces: `stringLiterals(t *testing.T, dir string, keep func(string) bool) []string` — sorted, de-duplicated values of every string literal in the non-test `.go` files directly under `<repoRoot>/<dir>` for which `keep` returns true. `readDoc(t *testing.T, file string) string` — the contents of `docs/<file>`. `const repoRoot = "../.."`. Tasks 2 and 3 use all three.

- [ ] **Step 1: Confirm the branch**

The branch `ozykhan/docs-reference` already exists, cut from `origin/main` at `db4915e`, with this plan as its only commit. Run `git switch ozykhan/docs-reference` if not already on it and confirm `git log --oneline -2` shows the plan commit on top of the PR 1 squash commit.

- [ ] **Step 2: Write the helper**

Create `test/docs/literals_test.go`:

```go
package docs_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const repoRoot = "../.."

// stringLiterals returns, sorted and de-duplicated, the value of every string
// literal in the non-test Go files directly under dir (a repo-relative package
// directory) for which keep returns true. Comments are not literals, so a
// name that only appears in a comment never counts — which is what the drift
// tests want: they pin what the binaries read and print, not what they say.
func stringLiterals(t *testing.T, dir string, keep func(string) bool) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(repoRoot, dir))
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	seen := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(repoRoot, dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			if v, err := strconv.Unquote(lit.Value); err == nil && keep(v) {
				seen[v] = true
			}
			return true
		})
	}
	out := make([]string, 0, len(seen))
	for v := range seen {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// readDoc returns docs/<file> as a string.
func readDoc(t *testing.T, file string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(docsDir, filepath.FromSlash(file)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
```

- [ ] **Step 3: Write the failing CLI drift test**

Create `test/docs/cli_drift_test.go`:

```go
package docs_test

import (
	"strings"
	"testing"
)

// Every usage line the CLI can print appears verbatim (minus the "usage: "
// prefix) in reference/cli.md, so a verb cannot be added or reshaped without
// its reference entry moving with it. Prose and flag descriptions are not
// checked; they stay human-reviewed.
func TestCLIReferenceCoversEveryUsageLine(t *testing.T) {
	usages := stringLiterals(t, "cmd/piper", func(s string) bool {
		return strings.HasPrefix(s, "usage: piper")
	})
	if len(usages) == 0 {
		t.Fatal("no \"usage: piper\" literals found under cmd/piper; the predicate or path is wrong")
	}
	page := readDoc(t, "reference/cli.md")
	for _, u := range usages {
		synopsis := strings.TrimPrefix(u, "usage: ")
		if !strings.Contains(page, synopsis) {
			t.Errorf("reference/cli.md lacks the synopsis %q", synopsis)
		}
	}
}
```

- [ ] **Step 4: Run it to verify it fails**

Run: `go test ./test/docs/ -run TestCLIReferenceCoversEveryUsageLine`
Expected: FAIL — `readDoc` fatals with `open ../../docs/reference/cli.md: no such file or directory`.

- [ ] **Step 5: Write `docs/reference/cli.md`**

The page below is complete. The 25 synopsis lines inside the ```` ```text ```` blocks are the exact `usage:` strings from `cmd/piper` with the `usage: ` prefix removed; copy them byte-for-byte (note the two spaces before the parenthetical in the `delete` and `login` variants). Before committing, read each verb's `case` in the named file and confirm every flag name, default, and exit code in the tables against it.

````markdown
# CLI reference

Every `piper` verb, its flags, and what it does. Each synopsis is the exact usage line the binary prints, and `test/docs` fails when a verb changes and this page does not.

## Invocation

```text
piper [--remote <base-domain>] [--version] <version|login|create|deploy|list|status|stop|start|delete|app|env|domains|github|box|agent> [args]
```

With no verb in an interactive terminal, `piper` opens the [TUI](../guides/tui.md). With no verb and no terminal it prints the usage and exits 2.

Global flags come before the verb:

| Flag | Default | Meaning |
| --- | --- | --- |
| `--remote <base-domain>` | `$PIPER_REMOTE` | Drive a relay-connected box through the relay instead of the local piperd; see [Remote control](../guides/remote-control.md). Rejected with `version`, `login`, and `agent` (exit 2). |
| `--version` | | Print the build version and exit 0. |

Per-verb flags come after the positional arguments: `piper delete blog --yes`, never `piper delete --yes blog`. Go's flag parser stops at the first positional, so a flag placed before the name is taken as the name.

Exit codes are the same everywhere: 0 on success, 1 on error (piperd unreachable, the request rejected, a deploy that did not end `running`), 2 on usage (bad flags or arguments). Declining a confirmation prompt prints `aborted` and exits 0.

Every verb except `login`, `box`, and `agent` talks to piperd's [control API](api.md) at the address `piper login` saved, `http://127.0.0.1:8088` when nothing is saved. `PIPER_ADDR` and `PIPER_TOKEN` override the saved address and token; see [Environment variables](env.md#piper-cli).

## version

```text
piper version
```

Prints the build version, the same string as `--version`. Exit 0.

## login

```text
piper login [--relay <url>] [--web] [--org <name>] [--no-enroll] [--re-enroll] [--relogin] [--data-dir DIR]
piper login --token <token> [--addr <url>]
piper login --token <token>  (create one with `piperd token create`, prefixed with `sudo` on a systemd install)
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `--token <token>` | | API token from `piperd token create`. Present: LAN login. Absent: relay login. |
| `--addr <url>` | the saved address, else `http://127.0.0.1:8088` | piperd address for a LAN login. |
| `--relay <url>` | `https://api.public.getpiper.dev` | Relay control API base URL for a relay login. |
| `--web` | off | Log in through the relay's GitHub App in the browser (one trip) instead of the device flow. |
| `--org <name>` | | Enroll this box for a GitHub org you own. |
| `--no-enroll` | off | Stop after identity; do not claim this box. |
| `--re-enroll` | off | Claim this box fresh even if already enrolled (after `piper box rm`, or when switching accounts or relays). |
| `--relogin` | off | Authenticate again even if the saved credential still works (switching GitHub accounts). |
| `--data-dir DIR` | `~/.piper/piperd` | piperd data directory, used to find the enrollment socket. |

Two commands share the verb. A **LAN login** (`--token`) verifies the token against the box's `GET /v1/apps` and saves the address and token as the current box; see [LAN control](../guides/lan-control.md). A **relay login** (no `--token`) runs the GitHub device flow against the relay (or the browser flow with `--web`), saves the account credential, then claims this box through piperd's enrollment socket and waits for the tunnel to connect; see [Join the public relay](../guides/relay-login.md).

Exit 1 when the token is rejected, piperd is unreachable, or the claim fails outright. Once piperd has persisted the enrollment, a tunnel that has not yet connected is advisory: the command says what is still pending and exits 0.

## agent

```text
piper agent <up|down|status>
```

No flags. Starts, stops, or reports the piperd service on this machine: `brew services` on macOS, the `piperd` systemd unit on Linux. `status` prints whether the daemon is running, the control API address, the build the running daemon reports, and the listen addresses and data directory it loaded. Exit 0 whether or not piperd is installed (`status` says `not installed`), 1 when `brew services` or `systemctl` fails, 2 on any other OS.

## create

```text
piper create <name> [--port N]
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `--port N` | `8080` | Container port the app listens on. |

Registers an app. The name is a DNS label: lowercase letters, digits, and hyphens, 1 to 63 characters, no leading or trailing hyphen, and not `hooks`. Exit 1 if the name is invalid or already exists.

## deploy

```text
piper deploy <name> [--path DIR] [--timeout DUR]
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `--path DIR` | `.` | Source directory containing a Dockerfile, sent to piperd as a tarball. |
| `--timeout DUR` | `15m` | Give up following the deploy after this long; `0` waits forever. |

Builds and runs one deployment, streaming build logs to stderr until it ends. Without an explicit `--path`, an app linked to a repository with `piper app link` deploys from that repository's tracked branch instead of the local directory. Prints the app URL on success. Exit 1 when the app does not exist, the final status is not `running`, or the timeout passes (the build may still be running on the box).

## list

```text
piper list
```

One line per app: name, port, and URL when it has ever been deployed. Exit 0.

## status

```text
piper status
```

Like `list` with each app's status (`building`, `running`, `failed`, `stopped`, or `-` when never deployed), preceded by the running daemon's version. With `--remote`, first reports whether the box's tunnel is connected and stops there when it is offline. Exit 0.

## stop

```text
piper stop <name>
```

Stops the app's production container, drops its routes, and marks the deployment `stopped`; the app and its history remain, and PR previews are untouched. A no-op when nothing is running. Exit 1 when the app does not exist.

## start

```text
piper start <name>
```

Runs the latest deployment's image again and restores its routes. A no-op unless the latest deployment is `stopped`. Exit 1 when the app does not exist.

## delete

```text
piper delete <name> [--yes]
piper delete <name> [--yes]  (the app name must come before flags)
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `--yes` | off | Skip the confirmation prompt. |

Removes the app, its deployments, and its per-app domains after a `y`/`yes` at the prompt. Exit 2 when the first argument starts with `-` (the flag was given before the name).

## app

### app link

```text
piper app link <name> --repo owner/name [--branch main] [--root-dir apps/web]
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `--repo owner/name` | required | GitHub repository, `owner/name`. |
| `--branch` | `main` | Tracked branch. |
| `--root-dir` | repo root | Build subpath inside the repository, for monorepos. |

Links the app to a repository so pushes to the branch deploy it and `piper deploy <name>` without `--path` builds from the repository; see [Git deploys](../guides/git-deploys.md). Exit 2 when `--repo` is missing.

## env

```text
piper env <app> <set KEY=VALUE [KEY2=VALUE2 ...] | ls [--show] | rm KEY>
```

Per-app environment variables. Changes are saved at once and applied on the app's next deploy or restart, never by restarting the running container.

### env set

```text
piper env <app> set KEY=VALUE [KEY2=VALUE2 ...]
```

No flags. Saves every pair; every argument is parsed before anything is sent, so one malformed argument (exit 2) leaves the app untouched. Keys are `[A-Za-z_][A-Za-z0-9_]*`; `PORT` is reserved and rejected by piperd.

### env ls

```text
piper env <app> ls [--show]
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `--show` | off | Print values instead of masking them as `******`. |

Lists the app's variables, sorted, with how long ago each was last set.

### env rm

```text
piper env <app> rm KEY
```

No flags. Removes one variable.

## domains

```text
piper domains <add <domain> --app <name> | list [--app <name>] | remove <domain> [--app <name>]>
```

Per-app custom domains; see [Custom domains](../guides/custom-domains.md) and [Direct serve](../guides/direct-serve.md). Every subcommand exits 1 on a LAN-only box, where piperd answers 409.

### domains add

```text
piper domains add <domain> --app <name>
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `--app <name>` | required | App to attach the domain to. |

Attaches the domain and prints the DNS record to create. Issuance starts once DNS resolves to the box or relay.

### domains list

```text
piper domains list [--app <name>]
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `--app <name>` | every app | Only this app's domains. |

One line per domain: app, status (`pending`, `issuing`, `active`, `failed`), certificate expiry, and whether DNS resolves correctly.

### domains remove

```text
piper domains remove <domain> [--app <name>]
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `--app <name>` | looked up | The app the domain is attached to; skips the lookup. |

Detaches the domain and releases its certificate. Exit 1 when the domain is not attached to any app.

## github

```text
piper github setup [--org <name>] | piper github repos | piper github reset [--yes]
```

The box's own GitHub App, for boxes that do not use a relay-brokered App; see [Git deploys](../guides/git-deploys.md).

### github setup

```text
piper github setup [--org <name>]
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `--org <name>` | your user account | Create the App under this GitHub organization. |

Opens the browser on GitHub's App-manifest page, catches the redirect, and stores the resulting App credentials on the box. Prints the App's install URL. Exit 1 after five minutes without approval.

### github repos

```text
piper github repos
```

No flags. Lists the account's relay-brokered GitHub App installations and the repositories they cover. Exit 1 without a relay login.

### github reset

```text
piper github reset [--yes]
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `--yes` | off | Skip the confirmation prompt. |

Deletes the box's own GitHub App credentials (the private key is not recoverable) and reports which webhook provider piperd will use after a restart.

## box

```text
piper box ls | piper box rm <base-domain> [--yes]
```

Boxes claimed on the relay account; both subcommands need a relay login and exit 1 without one. `--remote` does not apply.

### box ls

```text
piper box ls
```

No flags. One line per box: base domain, owner, and `connected` or `offline`.

### box rm

```text
piper box rm <base-domain> [--yes]
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `--yes` | off | Skip the confirmation prompt. |

Retires a box and frees its agent slot; the box must run `piper login --re-enroll` to come back. Exit 1 when the box is still connected, is not on this account, or belongs to an org you do not own.
````

- [ ] **Step 6: Run the drift test to verify it passes**

Run: `go test ./test/docs/ -run TestCLIReferenceCoversEveryUsageLine -v`
Expected: PASS. If it reports a missing synopsis, the page's line differs from the literal by a character; fix the page, never the test.

- [ ] **Step 7: Add the page to the manifest**

Edit `docs/manifest.json` so the `sections` array has a second element after `Guides`:

```json
    { "title": "Reference", "pages": [
      { "slug": "cli", "file": "reference/cli.md", "title": "CLI" }
    ]}
```

Keep the existing formatting style (one page per line). The file must still parse: `python3 -c 'import json; json.load(open("docs/manifest.json"))'` prints nothing.

- [ ] **Step 8: Run the whole docs package**

Run: `go test ./test/docs/ -v`
Expected: PASS for all four tests. The three manifest tests now also check `reference/cli.md` for its H1, lead, and links.

- [ ] **Step 9: Mutation check**

Append to `cmd/piper/main.go`:

```go

var _ = "usage: piper frob <thing>"
```

Run: `go test ./test/docs/ -run TestCLIReferenceCoversEveryUsageLine`
Expected: FAIL with `reference/cli.md lacks the synopsis "piper frob <thing>"`.

Revert: `git checkout -- cmd/piper/main.go`, then `git status --short` shows no change under `cmd/`.

- [ ] **Step 10: Commit**

```bash
gofmt -l test/docs/ && git add test/docs/literals_test.go test/docs/cli_drift_test.go docs/reference/cli.md docs/manifest.json
git commit -m "docs: CLI reference page pinned by a usage-line drift test

Part of the docs organization spec (PR 2). test/docs gains the go/parser
literal helper the three drift tests share; cli.md lists every verb with
the exact usage line the binary prints.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

(`gofmt -l` must print nothing.)

---

### Task 2: env drift test and `reference/env.md`

**Files:**
- Create: `test/docs/env_drift_test.go`
- Create: `docs/reference/env.md`
- Modify: `docs/manifest.json`
- Read (source of truth): `internal/config/config.go` (`Load`, `NoBrowser`, `LoadClient`, `DefaultDataDir`), `cmd/piperd/main.go` (grep `PIPER_`), `cmd/piper/main.go:189`, `cmd/piper-relay/main.go:43-300` and `:379`, `cmd/piper-edge/main.go:30-90`, `packaging/systemd/piperd.env.example`, `docs/self-host/relay.md` § Configure and § Ops surface

**Interfaces:**
- Consumes: `stringLiterals`, `readDoc` from Task 1.
- Produces: nothing later tasks use.

- [ ] **Step 1: Write the failing env drift test**

Create `test/docs/env_drift_test.go`:

```go
package docs_test

import (
	"regexp"
	"strings"
	"testing"
)

var envNameRE = regexp.MustCompile(`^PIPER_[A-Z_]+$`)

// envDirs are the packages that read PIPER_* variables: piperd's config
// loader and the four binaries. Test helper packages are not binaries and
// stay out.
var envDirs = []string{"internal/config", "cmd/piperd", "cmd/piper", "cmd/piper-relay", "cmd/piper-edge"}

// Every PIPER_* name a binary reads appears backticked in reference/env.md.
// Defaults and meanings are not checked; they stay human-reviewed.
func TestEnvReferenceCoversEveryVariable(t *testing.T) {
	page := readDoc(t, "reference/env.md")
	total := 0
	for _, dir := range envDirs {
		names := stringLiterals(t, dir, envNameRE.MatchString)
		total += len(names)
		for _, n := range names {
			if !strings.Contains(page, "`"+n+"`") {
				t.Errorf("reference/env.md lacks `%s` (read in %s)", n, dir)
			}
		}
	}
	if total == 0 {
		t.Fatal("no PIPER_* literals found; the predicate or paths are wrong")
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./test/docs/ -run TestEnvReferenceCoversEveryVariable`
Expected: FAIL — `open ../../docs/reference/env.md: no such file or directory`.

- [ ] **Step 3: Write `docs/reference/env.md`**

The page below is complete. Every default was read from the `env("NAME", default)` call or `os.Getenv` comparison in the file the section names; confirm each row against that file before committing, and add a row for any name the drift test reports missing (the test is the authority on the name list, this table on the rest).

````markdown
# Environment variables

Every `PIPER_*` variable each binary reads, with its default and what it does. `test/docs` fails when a binary reads a name this page does not list.

Unset means the default applies. Booleans are enabled by the exact value `1` unless a row says otherwise.

## piperd

Read by `internal/config` and `cmd/piperd`. The systemd unit sets `PIPER_DATA_DIR` to `/var/lib/piper` and reads the rest from `/etc/piper/piperd.env`; [`packaging/systemd/piperd.env.example`](../../packaging/systemd/piperd.env.example) is the curated subset most installs touch. Values marked *relay.json* also come from the enrollment file `piper login` writes; the variable, when set, overrides it and marks the box operator-managed, so `piper login` refuses to enroll it.

| Name | Default | Meaning |
| --- | --- | --- |
| `PIPER_DATA_DIR` | `~/.piper/piperd` (`/var/lib/piper` under systemd) | Directory for the SQLite database, `relay.json`, and certificates. |
| `PIPER_API_ADDR` | `127.0.0.1:8088` | Control API listen address. A loopback bind serves without a token; any other bind requires a bearer token (see [Control API](api.md#authentication)). `0.0.0.0:8088` opens it to the LAN. |
| `PIPER_WEBHOOK_ADDR` | `127.0.0.1:8089` | Loopback listener for GitHub webhooks arriving through the relay. |
| `PIPER_BASE_DOMAIN` | `piper.localhost`, or the enrolled relay's (*relay.json*) | Apps are served at `<name>.<base>`. Setting it makes the box-wide domain env-managed: `PUT`/`DELETE /v1/domain` answer 409. |
| `PIPER_CADDY_ADMIN` | `http://127.0.0.1:2019` | Admin API of the Caddy piperd manages. |
| `PIPER_HTTP_ADDR` | `:80` | Caddy's HTTP listen address. |
| `PIPER_HTTPS_ADDR` | `:443` | Caddy's HTTPS listen address. |
| `PIPER_RELAY_ADDR` | unset (*relay.json*) | Relay tunnel endpoint, `host:port`. Unset and no `relay.json`: the box is LAN-only. |
| `PIPER_RELAY_TOKEN` | unset (*relay.json*) | Enrollment token presented to the relay. |
| `PIPER_RELAY_TERMINATED` | unset (*relay.json*) | `1`: the relay terminates TLS for the shared domain; the box serves `:80` and holds no certificate. |
| `PIPER_WEBHOOK_SECRET` | unset (*relay.json*) | HMAC key the relay signs brokered GitHub deliveries with. |
| `PIPER_GITHUB_BROKERED` | unset (*relay.json*) | `1`: the relay holds the GitHub App, so this box needs no App credentials of its own. |
| `PIPER_GITHUB_API_BASE` | `https://api.github.com` | GitHub API base URL override, for tests against a fake GitHub. |
| `PIPER_ACME_EMAIL` | unset | ACME account email for public certificates. |
| `PIPER_ACME_CA` | Let's Encrypt production | ACME directory URL; set the staging URL while testing. |
| `PIPER_DNS_PROVIDER` | unset | DNS-01 provider name for lego, for example `cloudflare`. |
| `PIPER_TLS_CERT_FILE` | unset | Static certificate path; with the key, skips ACME (tests, bring-your-own cert). |
| `PIPER_TLS_KEY_FILE` | unset | Static key path, paired with `PIPER_TLS_CERT_FILE`. |
| `PIPER_PUBLIC_IP` | learned from the relay | Public IP the direct-serve DNS guidance names; needed on a never-enrolled box or behind split-horizon NAT. Ignored with a log line when not an IP. |
| `PIPER_SERVE` | `relay` | `direct` serves `PIPER_BASE_DOMAIN` from this box's own `:443` instead of through the relay; see [Direct serve](../guides/direct-serve.md). Any other value is ignored with a log line. |
| `PIPER_SKIP_CADDY` | unset | Any value: do not start or manage a Caddy, because one is already running (the e2e suite). |
| `PIPER_TEST_ISSUER` | unset | **Test only.** `selfsigned` replaces ACME with a self-signed issuer so the e2e suite can serve TLS without real DNS. Never set it on a real box. |

## piper-relay

Read by `cmd/piper-relay`. See [Run your own relay](../self-host/relay.md#configure) for a working configuration.

| Name | Default | Meaning |
| --- | --- | --- |
| `PIPER_RELAY_DB_URL` | required | Postgres DSN, `postgres://user:password@host/dbname`. The relay creates its tables on first start. |
| `PIPER_RELAY_APEX` | `public.getpiper.dev` | The shared domain boxes get subdomains of. |
| `PIPER_RELAY_TLS_ADDR` | `:443` | Public TLS listener (SNI passthrough and shared-domain termination). |
| `PIPER_RELAY_HTTP_ADDR` | `:80` | Public HTTP listener. |
| `PIPER_RELAY_TUNNEL_ADDR` | `:7000` | Listener agents dial for their tunnels. |
| `PIPER_RELAY_API_ADDR` | `:8080` | Account and control API listener (`api.<apex>`). |
| `PIPER_RELAY_TUNNEL_PUBLIC` | unset | Tunnel endpoint handed to boxes at enrollment (`tunnel_endpoint` in the enroll response), for when the dialable address differs from the bind. |
| `PIPER_RELAY_ADVERTISE_HOST` | first non-loopback IPv4 | Host this instance registers in the pool for the edge and other relays to reach it. |
| `PIPER_RELAY_ZONE` | unset | Failure zone label; an agent's two tunnel sessions prefer relays in different zones. |
| `PIPER_RELAY_MAX_AGENTS` | `3` | Boxes per account. |
| `PIPER_RELAY_MAX_APPS` | `10` | Shared-domain app hostnames per account. |
| `PIPER_RELAY_MAX_DOMAINS` | `5` | Custom domains per account. |
| `PIPER_RELAY_TLS_CERT` | unset | Wildcard certificate for shared-domain termination; unset with the key means passthrough only. Re-read when the file changes. |
| `PIPER_RELAY_TLS_KEY` | unset | Key paired with `PIPER_RELAY_TLS_CERT`. |
| `PIPER_RELAY_PROXY_PROTOCOL` | unset | `1`: `:443`, `:80`, and `:7000` require a PROXY protocol v2 header. Set only when a trusted L4 proxy or `piper-edge` is the sole path to those ports. |
| `PIPER_RELAY_GITHUB_CLIENT_ID` | unset | GitHub OAuth client ID for self-service `piper login`. Unset disables self-service login; boxes can only be operator-enrolled. |
| `PIPER_RELAY_GITHUB_CLIENT_SECRET` | unset | Client secret paired with the ID; also required for browser login. |
| `PIPER_RELAY_WEB_REDIRECTS` | unset | Comma-separated `https://host/path-prefix` allowlist of browser-login redirect URIs (the dashboard). Empty disables browser login. |
| `PIPER_RELAY_GITHUB_APP_ID` | unset | Relay-held GitHub App ID; with the key, the relay brokers webhooks and tokens for every box. |
| `PIPER_RELAY_GITHUB_APP_KEY` | unset | Path to that App's private key PEM. |
| `PIPER_RELAY_GITHUB_APP_SLUG` | unset | That App's slug, so boxes can deep-link its install page. |
| `PIPER_RELAY_GITHUB_WEBHOOK_SECRET` | unset | That App's webhook secret. |
| `PIPER_RELAY_OPS_ADDR` | `127.0.0.1:9090` | Ops listener bind; binds when set or when either toggle below is on. Serves `/readyz` and `/livez` whenever bound. |
| `PIPER_RELAY_METRICS` | unset | `1`: serve Prometheus metrics on the ops listener. |
| `PIPER_RELAY_LOGS` | unset | `1`: serve the recent-log ring on the ops listener. |
| `PIPER_RELAY_FAKE_APPROVE` | unset | **Test only.** `1` auto-approves device login when no GitHub client ID is set. Never set it on a public relay. |

## piper-edge

Read by `cmd/piper-edge`. The edge shares the relays' database and fronts their public ports.

| Name | Default | Meaning |
| --- | --- | --- |
| `PIPER_EDGE_APEX` | required | The relays' apex, for example `public.getpiper.dev`. |
| `PIPER_EDGE_DB_URL` | required | The relays' Postgres DSN. |
| `PIPER_EDGE_TLS_ADDR` | `:443` | Public TLS listener. |
| `PIPER_EDGE_HTTP_ADDR` | `:80` | Public HTTP listener. |
| `PIPER_EDGE_TUNNEL_ADDR` | `:7000` | Tunnel listener agents dial. |
| `PIPER_EDGE_PROXY_PROTOCOL` | unset | `1`: the three public listeners require a PROXY protocol v2 header from a trusted balancer in front. |
| `PIPER_EDGE_OPS_ADDR` | `127.0.0.1:9090` | Ops listener bind, same rules as the relay's. |
| `PIPER_EDGE_METRICS` | unset | `1`: serve metrics on the ops listener. |
| `PIPER_EDGE_LOGS` | unset | `1`: serve the recent-log ring on the ops listener. |

## piper CLI

Read by `cmd/piper` and the TUI. The CLI also reads `PIPER_API_ADDR`, `PIPER_HTTP_ADDR`, `PIPER_HTTPS_ADDR`, and `PIPER_DATA_DIR` from the running daemon's environment for `piper agent status`; those are piperd's variables, listed above.

| Name | Default | Meaning |
| --- | --- | --- |
| `PIPER_ADDR` | the saved box's address, else `http://127.0.0.1:8088` | piperd control API to talk to; overrides the saved box. |
| `PIPER_TOKEN` | the saved box's token | Bearer token for that API; overrides the saved box. |
| `PIPER_REMOTE` | unset | Default for `--remote`: base domain of a relay-connected box to drive through the relay. |
| `PIPER_NO_BROWSER` | unset | `1`: never open a browser (`piper login --web`, `piper github setup`, the TUI); print the URL instead. |
````

- [ ] **Step 4: Run the drift test to verify it passes**

Run: `go test ./test/docs/ -run TestEnvReferenceCoversEveryVariable -v`
Expected: PASS. A reported missing name means a binary reads a variable this table lacks: add its row from the file the message names.

- [ ] **Step 5: Add the page to the manifest**

In `docs/manifest.json`, append to the `Reference` section's `pages` after the `cli` line:

```json
      { "slug": "env", "file": "reference/env.md", "title": "Environment variables" }
```

- [ ] **Step 6: Run the whole docs package**

Run: `go test ./test/docs/ -v`
Expected: PASS for all five tests (the manifest tests now check `env.md`'s H1, lead, and the `../../packaging/...` and `api.md#authentication` links; the anchor is not checked, only the file).

- [ ] **Step 7: Mutation check**

Append to `internal/config/config.go`:

```go

var _ = "PIPER_FROB"
```

Run: `go test ./test/docs/ -run TestEnvReferenceCoversEveryVariable`
Expected: FAIL with ``reference/env.md lacks `PIPER_FROB` (read in internal/config)``.

Revert: `git checkout -- internal/config/config.go`; `git status --short` shows nothing under `internal/`.

- [ ] **Step 8: Commit**

```bash
gofmt -l test/docs/ && git add test/docs/env_drift_test.go docs/reference/env.md docs/manifest.json
git commit -m "docs: environment reference page pinned by an env-var drift test

One table per binary (piperd, piper-relay, piper-edge, piper CLI); every
PIPER_* literal a binary reads must appear on the page.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 3: API drift test and `reference/api.md`

**Files:**
- Create: `test/docs/api_drift_test.go`
- Create: `docs/reference/api.md`
- Modify: `docs/manifest.json`
- Read (source of truth): `internal/api/api.go` (all of it: the `App` and `AgentInfo` types, every `mux.HandleFunc`, `RequireToken`), `internal/store/store.go:24-46` (`App`, `Deployment`), `internal/domain/domain.go:831-853` (`DNSRecord`, `Status`), `internal/domain/appdomain.go:391-402` (`AppDomainStatus`), `cmd/piperd/main.go:243-285` (loopback token rule)

**Interfaces:**
- Consumes: `stringLiterals`, `readDoc` from Task 1.
- Produces: nothing later tasks use.

- [ ] **Step 1: Write the failing API drift test**

Create `test/docs/api_drift_test.go`:

```go
package docs_test

import (
	"regexp"
	"strings"
	"testing"
)

// Route patterns as registered on the mux: "METHOD /v1/...".
var routeRE = regexp.MustCompile(`^(GET|POST|PUT|DELETE) /v1/`)

// Every route internal/api registers has its own "### METHOD /v1/path"
// heading in reference/api.md. Bodies and status lists are not checked; they
// stay human-reviewed.
func TestAPIReferenceCoversEveryRoute(t *testing.T) {
	routes := stringLiterals(t, "internal/api", routeRE.MatchString)
	if len(routes) == 0 {
		t.Fatal("no route literals found under internal/api; the predicate or path is wrong")
	}
	page := readDoc(t, "reference/api.md")
	for _, r := range routes {
		if !strings.Contains(page, "\n### "+r+"\n") {
			t.Errorf("reference/api.md lacks a heading `### %s`", r)
		}
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./test/docs/ -run TestAPIReferenceCoversEveryRoute`
Expected: FAIL — `open ../../docs/reference/api.md: no such file or directory`.

- [ ] **Step 3: Write `docs/reference/api.md`**

The page below is complete for the 25 routes `internal/api/api.go` registers today. Field names in the JSON shapes are exact: `store.App` and `store.Deployment` carry no `json` tags, so their fields serialize under their Go names (`Name`, `Port`, `CreatedAt`, …), while the domain types and inline request structs use the snake_case tags shown. Before committing, walk `api.go` top to bottom and confirm each heading, each status code, and each field against the handler.

````markdown
# Control API

piperd's HTTP control plane: every route the `piper` CLI, the TUI, and the dashboard use, with request and response shapes and the error statuses each handler returns. `test/docs` fails when a route is registered without a heading here.

All responses are JSON (`Content-Type: application/json`) except deployment logs, which are plain text. Errors are plain-text bodies with the status codes listed per route; a `500` carries only `internal server error`, the detail goes to piperd's log. Every route may also answer `500` on a store failure, so it is not repeated below.

## Authentication

piperd listens on `PIPER_API_ADDR` (default `127.0.0.1:8088`). A loopback bind serves without credentials: whoever can reach the socket owns the box. Any other bind (`0.0.0.0:8088` for LAN use) wraps every route in a bearer check, and a missing, malformed, or revoked token answers `401`. Create a token on the box with `piperd token create` (with `sudo` on a systemd install) and pass it as `Authorization: Bearer <token>`; `piper login --token` stores it for the CLI.

Through the relay the same routes are reachable at `https://api.<apex>/agents/<base-domain>/v1/...`, authenticated with the account credential from `piper login`; the relay swaps it for the box's own token and forwards the request over the tunnel.

## Types

**App** — the stored app plus the daemon's view of it:

```json
{
  "Name": "blog",
  "Port": 8080,
  "Repo": "octocat/blog",
  "Branch": "main",
  "RootDir": "",
  "Hostname": "blog.piper.localhost",
  "CreatedAt": "2026-09-11T10:00:00Z",
  "Status": "running",
  "Scheme": "http"
}
```

`Status` is exactly one of `building`, `running`, `failed`, `stopped`, or `""` when never deployed; a `failed` latest deployment still reports `running` while an older deployment is up. `Scheme` is `http` or `https`, whichever the public reaches this box's apps on. `Hostname` is `""` until the first deploy.

**Deployment:**

```json
{
  "ID": "d_01J...",
  "App": "blog",
  "PR": 0,
  "ImageID": "sha256:...",
  "ContainerID": "...",
  "HostPort": 32768,
  "Status": "running",
  "Hostname": "",
  "CreatedAt": "2026-09-11T10:00:00Z"
}
```

`PR` is non-zero and `Hostname` set for a pull-request preview; production's hostname lives on the app.

**AppDomainStatus** — one per-app custom domain:

```json
{
  "domain": "shop.example.com",
  "app": "blog",
  "status": "active",
  "error": "",
  "cert_not_after": "2026-12-10T10:00:00Z",
  "dns_records": [{ "type": "CNAME", "name": "shop.example.com", "value": "blog.abc123.public.getpiper.dev" }],
  "dns_ok": true,
  "note": ""
}
```

`status` is `pending`, `issuing`, `active`, or `failed`; `cert_not_after` and `note` are omitted when empty.

**Status** — the box-wide custom domain:

```json
{
  "domain": "example.com",
  "dns_provider": "cloudflare",
  "dns_token_set": true,
  "source": "api",
  "serve": "relay",
  "status": "active",
  "error": "",
  "cert_not_after": "2026-12-10T10:00:00Z",
  "dns_records": [{ "type": "A", "name": "example.com", "value": "203.0.113.7" }],
  "dns_ok": true
}
```

`source` is `api` or `env` (`PIPER_BASE_DOMAIN` set); `serve` is `relay` or `direct`; `status` is `""`, `issuing`, `active`, or `failed`. The DNS token itself is never returned.

## Daemon

### GET /v1/version

The running daemon's build and the listen configuration it loaded, so a client can tell a half-applied upgrade from a fix that did not work.

Response `200`: `{"version": "v0.23.1", "http_addr": ":80", "https_addr": ":443", "data_dir": "/var/lib/piper"}`.

## Apps

### POST /v1/apps

Create an app. Body: `{"name": "blog", "port": 8080}`; `port` defaults to 8080 when 0 or absent.

Response `201`: App. Errors: `400` invalid body, name not a DNS label (lowercase letters, digits, hyphens, 1 to 63 characters, no leading or trailing hyphen), or the reserved name `hooks`; `409` app exists.

### GET /v1/apps

Every app. Response `200`: `[App, ...]` (an empty array when there are none).

### GET /v1/apps/{name}

One app. Response `200`: App. Errors: `404` not found.

### DELETE /v1/apps/{name}

Delete the app, its deployments, and its per-app domains; on a box with a domain manager each domain is released first and a release failure aborts the delete, so it stays retryable. Response `204`. Errors: `404` unknown app.

### POST /v1/apps/{name}/stop

Stop the production container, drop its routes, and mark the deployment `stopped`; a no-op when nothing is running. Response `204`. Errors: `404` unknown app.

### POST /v1/apps/{name}/start

Run the latest deployment's image again and restore its routes; a no-op unless the latest deployment is `stopped`. Response `204`. Errors: `404` unknown app.

### POST /v1/apps/{name}/link

Link the app to a GitHub repository. Body: `{"repo": "owner/name", "branch": "main", "root_dir": "apps/web"}`; `root_dir` is optional and must be a relative path inside the repository. When a relay connection exists the binding is also registered there so brokered webhooks route to this box; a relay that is briefly unreachable does not fail the request.

Response `204`. Errors: `400` invalid body, `repo` not `owner/name`, or `root_dir` absolute or escaping the repository; `404` unknown app.

## Deployments

### POST /v1/apps/{name}/deploy

Deploy from a tarball. The body is a tar stream of the source directory containing a Dockerfile. The build runs after the response; poll the deployment's status and logs.

Response `202`: Deployment with `Status` `building`. Errors: `400` bad tar (including an entry that escapes the destination); `404` unknown app.

### POST /v1/apps/{name}/deploy-from-repo

Deploy a linked app from its repository's tracked branch. No body. The fetch happens before the response, the build after it.

Response `202`: Deployment. Errors: `404` unknown app; `409` app not linked, or no GitHub App configured (`run piper github setup first`); `502` the repository fetch failed.

### GET /v1/apps/{name}/deployments

Every deployment of the app, newest first. Response `200`: `[Deployment, ...]` (an empty array when there are none). Errors: `404` unknown app.

### GET /v1/apps/{name}/deployments/{id}/logs

The deployment's build and run log. Response `200`: `text/plain; charset=utf-8`. Errors: `404` unknown deployment.

## GitHub App

The box's own GitHub App, for boxes not using a relay-brokered one.

### POST /v1/github/manifest

Build the GitHub App manifest `piper github setup` posts to GitHub. Body: `{"redirect_url": "http://127.0.0.1:PORT/cb"}`.

Response `200`: `{"manifest": "<JSON string>"}`. Errors: `400` invalid body or empty `redirect_url`.

### POST /v1/github/exchange

Exchange the code GitHub redirected back with for App credentials, store them, and start serving webhooks without a restart. Body: `{"code": "..."}`.

Response `200`: `{"slug": "piper-abc123"}`. Errors: `400` invalid body; `502` GitHub rejected the exchange.

### GET /v1/github

Whether a GitHub App is configured on this box. Response `200`: `{"configured": false}` or `{"configured": true, "app_id": 12345, "slug": "piper-abc123"}`.

### DELETE /v1/github/app

Drop the stored App. The running webhook listener keeps the old credentials until piperd restarts; the response names the provider a restart will pick.

Response `200`: `{"provider": "brokered" | "none" | "unknown"}` (or another provider name).

## Box-wide domain

The box's own domain, `PIPER_BASE_DOMAIN` or one set here. All three routes answer `409` on a LAN-only box: `domain config requires a relay connection, or direct serve (PIPER_BASE_DOMAIN with PIPER_SERVE=direct)`.

### GET /v1/domain

Response `200`: Status.

### PUT /v1/domain

Set or replace the domain. Body: `{"domain": "example.com", "dns_provider": "cloudflare", "dns_token": "...", "serve": "relay"}`; `serve` is `relay` or `direct`.

Response `200`: Status. Errors: `400` invalid body, `invalid domain`, `unsupported dns provider`, `dns_token required`, or invalid `serve`; `409` the domain is env-managed (`PIPER_BASE_DOMAIN` is set) or the box is LAN-only.

### DELETE /v1/domain

Remove the API-managed domain. Response `204`. Errors: `409` env-managed or LAN-only.

## Per-app domains

Custom domains attached to one app. All three routes answer `409` on a LAN-only box, as above.

### GET /v1/apps/{name}/domains

Response `200`: `[AppDomainStatus, ...]` (an empty array when there are none). Errors: `404` unknown app.

### POST /v1/apps/{name}/domains

Attach a domain. Body: `{"domain": "shop.example.com"}`. The domain is stored lowercase.

Response `201`: AppDomainStatus, whose `dns_records` say what to create. Errors: `400` invalid body or invalid domain; `404` unknown app; `409` the domain is the box-wide domain, is already attached to an app, or, on a direct-served box, there is no box-wide domain with a DNS token to issue its certificate with.

### DELETE /v1/apps/{name}/domains/{domain}

Detach the domain and release its certificate. Response `204`. Errors: `404` unknown app, or the domain is not attached to this app.

## App environment

Per-app variables applied on the next deploy or restart.

### GET /v1/apps/{name}/env

Response `200`: `{"env": {"KEY": "value"}, "updated_at": {"KEY": "2026-09-11T10:00:00.123456789Z"}}`. Errors: `404` unknown app.

### POST /v1/apps/{name}/env

Set one variable. Body: `{"key": "DATABASE_URL", "value": "..."}`. Keys match `[A-Za-z_][A-Za-z0-9_]*`.

Response `204`. Errors: `400` invalid body, invalid key, or the reserved key `PORT`; `404` unknown app.

### DELETE /v1/apps/{name}/env/{key}

Remove one variable. Response `204` (also when the key was not set). Errors: `404` unknown app.
````

- [ ] **Step 4: Run the drift test to verify it passes**

Run: `go test ./test/docs/ -run TestAPIReferenceCoversEveryRoute -v`
Expected: PASS. A reported missing heading means `api.go` registers a route this page lacks, or a heading has a typo; the heading must match the literal exactly, including `{name}`-style placeholders.

- [ ] **Step 5: Add the page to the manifest**

In `docs/manifest.json`, append to the `Reference` section's `pages` after the `env` line:

```json
      { "slug": "api", "file": "reference/api.md", "title": "Control API" }
```

The finished section reads:

```json
    { "title": "Reference", "pages": [
      { "slug": "cli", "file": "reference/cli.md", "title": "CLI" },
      { "slug": "env", "file": "reference/env.md", "title": "Environment variables" },
      { "slug": "api", "file": "reference/api.md", "title": "Control API" }
    ]}
```

- [ ] **Step 6: Run the whole docs package**

Run: `go test ./test/docs/ -v`
Expected: PASS for all six tests.

- [ ] **Step 7: Mutation check**

Append to `internal/api/api.go`:

```go

var _ = "GET /v1/frob"
```

Run: `go test ./test/docs/ -run TestAPIReferenceCoversEveryRoute`
Expected: FAIL with ``reference/api.md lacks a heading `### GET /v1/frob` ``.

Revert: `git checkout -- internal/api/api.go`; `git status --short` shows nothing under `internal/`.

- [ ] **Step 8: Commit**

```bash
gofmt -l test/docs/ && git add test/docs/api_drift_test.go docs/reference/api.md docs/manifest.json
git commit -m "docs: control API reference page pinned by a route drift test

One ### heading per route registered in internal/api, with auth, shapes,
and error statuses read from the handlers.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 4: publish the section in the maps and verify the branch

**Files:**
- Modify: `docs/README.md` (the `reference/` row)
- Modify: `README.md:93-99` (the Docs table)
- Modify: `PROGRESS.md:5` and `PROGRESS.md:20`

**Interfaces:**
- Consumes: the three pages and the manifest section from Tasks 1 to 3.
- Produces: nothing.

- [ ] **Step 1: Mark `reference/` as published in the docs map**

In `docs/README.md`, the table row for `reference/` ends `| yes (PR 2) |`. Change it to `| yes |`. Nothing else on the page changes.

- [ ] **Step 2: Add a Reference row to the repo README's docs table**

In `README.md`, the table under `| Doc | Covers |` has rows Guides, Self-host, Docs map, PROGRESS.md, Design. Insert after the Guides row:

```markdown
| [Reference](docs/reference/cli.md) | every CLI verb, env var, and control-API route, pinned to code by `test/docs` |
```

- [ ] **Step 3: Update PROGRESS.md**

Line 20 (the Foundation bullet) ends `; reference pages and the dashboard sync follow`. Change that tail to `; the dashboard sync follows`.

Line 5 (`_Last updated:`) begins `_Last updated: 2026-09-11 — docs organized by audience with a manifest and contract test ([spec](...)).`. Change the clause to `docs organized by audience with a manifest, contract test, and code-pinned reference pages ([spec](...))`, keeping the link and everything after it unchanged.

- [ ] **Step 4: Check no in-repo link still says the reference is pending**

Run:

```bash
grep -rn 'PR 2' README.md docs/README.md PROGRESS.md CLAUDE.md
```

Expected: no output.

- [ ] **Step 5: Run the full gate**

Run: `make verify`
Expected: exit status 0 (check with `echo $?` immediately after; do not grep the output).

- [ ] **Step 6: Commit**

```bash
git add docs/README.md README.md PROGRESS.md
git commit -m "docs: reference section is published; maps and progress updated

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

## After the tasks (controller, not implementers)

1. Final whole-branch review, then push and open the PR into `main`:
   - Title: `[docs] reference pages (CLI, env, control API) with drift tests`
   - Body: what the three pages and three tests are, `Part of` the docs organization spec (link the spec file), the five mutation checks were run, and the trailer line.
2. Squash-merge. Then file the follow-ups the spec and PR 1 left:
   - `[docs] generate reference pages from code` — spec § Step two: a command table with help text in `cmd/piper`, doc comments on `internal/config` fields, a route table with a one-line purpose per endpoint; the generator overwrites the three pages and the drift tests become "generated output is committed and current". Labels: `documentation`, `enhancement`, `P3`, `size/L`, `cli`, `agent`.
   - `[cli] piper deploy timeout hint names a command that does not exist` — `cmd/piper/main.go` prints ``check `piper app <name>` `` but the only `app` subcommand is `link`; point it at `piper status` or `GET /v1/apps/{name}/deployments`. Labels: `bug`, `P3`, `size/XS`, `cli`.
   - `[cli] piper domains add says "points at the relay" on a direct box` — `cmd/piper/domains.go` prints `issuance starts once DNS points at the relay`; on a direct-served box DNS points at the box. Labels: `bug`, `P3`, `size/XS`, `cli`.
3. PR 3 (dashboard: manifest-driven sync, sections, `llms.txt`, raw routes, landing links) gets its own plan in the dashboard repo.

## Self-review

- **Spec coverage.** § Reference pages: cli.md (Task 1), env.md (Task 2), api.md (Task 3), each in the mandated layout. § `test/docs`: one shared helper (Task 1), CLI drift (Task 1), env drift (Task 2), API drift (Task 3), each mutation-checked in its task; the manifest contract test already exists and covers the new pages through the manifest edits. § Rollout item 2: separate PR, step-two issue filed on merge (After the tasks). Success criterion 4 (each drift test fails on an undocumented verb, env var, or route) is the mutation step in each task. The one deliberate deviation, scanning `cmd/piperd` for env vars, is stated in Global Constraints with its reason.
- **Placeholders.** None: every page is written out in full, every test is complete code, every command has its expected result.
- **Consistency.** `stringLiterals`, `readDoc`, `repoRoot`, and `docsDir` are named identically in Tasks 1, 2, and 3. Manifest slugs `cli`, `env`, `api` match the `env.md#piper-cli`, `api.md#authentication` anchors and the README links. The `Reference` section title matches the docs map table. Heading `### METHOD /v1/path` matches the drift test's `"\n### " + route + "\n"` check, which requires each heading on its own line with nothing trailing.
