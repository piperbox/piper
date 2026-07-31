# TUI Phase 6 — wizards (login, GitHub setup, link repo)

**Date:** 2026-07-13
**Status:** approved design, pre-implementation
**Surface:** `[cli]` — the `piper` binary only; one surgical `internal/client` addition (a `StatusError.Unauthorized()` helper). No `piperd` or `piper-relay` changes.

Phase 6 — the final phase — of the Interactive TUI epic
[#183](https://github.com/piperbox/piper/issues/183). Phases 1–5 built the
multi-box config, the read-only dashboard, drill-down, actions, and the boxes
view. This phase adds the three flows the TUI still shells out to the CLI for:
**login**, **GitHub App setup**, and **link repo**. Parent design:
`docs/superpowers/specs/2026-07-12-piper-tui-design.md` (View map →
**Login/connect wizard**, **GitHub setup wizard**, **Link repo**).

## Problem

The TUI can monitor, deploy, and manage apps, but only against a box that was
*already* authenticated and *already* had a GitHub App configured — both done
from the CLI (`piper login`, `piper github setup`, `piper app link`). Three gaps
remain, all named as placeholders in the parent design's view map
(`2026-07-12-piper-tui-design.md:73`, `:84–87`, `:91`):

- **Login** (`L` from apps home) — no way to authenticate a box from inside the
  TUI; a token-less box just banners `401` on every poll.
- **GitHub setup** (`g` from apps home) — no way to run the GitHub App manifest
  flow (`piper github setup`).
- **Link repo** (`l` from app detail) — no way to attach a repo to an app
  (`piper app link`).

## Proposal

Three leaf views plus small root wiring, following the established
one-file-per-view layout and the `API`-interface seam. Each view maps onto an
existing `*client.Client` method the CLI subcommands already use.

### 1. API seam — three new methods

The TUI talks only to the `tui.API` interface (`tui.go`), never
`internal/client`. Phase 6 widens that interface with the three methods these
wizards call; `*client.Client` already implements all three (used by the
`login`/`github`/`app link` subcommands), and the test `fakeAPI` gains them:

```go
Manifest(redirectURL string) (string, error)   // GitHub App manifest for the flow
ExchangeGitHub(code string) error               // trade the ?code= for App creds
LinkApp(name, repo, branch string) error        // attach a repo to an app
```

Login needs no new API method — it verifies with the existing `ListApps` probe.

### 2. `login.go` — login wizard (`L` from apps home)

> **Interim scope — LAN token only.** The parent design's login/connect wizard
> eventually spans two target types: **LAN** (paste a token from
> `piperd token create`) and **relay** (GitHub device-flow → account credential,
> today's `piper login`/`connect`). Phase 6 ships **LAN only**; relay login is a
> later phase. The view is written with a **target-type seam** (§below) so relay
> slots in as a second branch without re-architecting the view or the root.
>
> **Update:** `piper connect` no longer exists — the one-command login merge
> ([`2026-07-31-one-command-login-design.md`](2026-07-31-one-command-login-design.md))
> folds box claiming into `piper login`. The future relay branch here targets
> that merged verb, not a separate connect step.

A single leaf view pushed by the root on `L` (a global key, wired in the existing
`!m.topCapturesText()` block like `?`/`t`, with the same already-on-top guard).
`capturesText()` is true. It reuses the `formView`/`bubbles/textinput` pattern
with one masked **token** field (`textinput.EchoPassword`).

On `↵` it **verifies before it saves**, exactly mirroring the boxes form's
pre-save probe (phase 5) and the CLI `login` (`cmd/piper/main.go:118`):

1. Reject an empty token inline (`⚠ token required`).
2. Build a throwaway client for the **current box's addr** + the entered token
   (via the injected `Dialer`, using a synthetic `config.Box{Addr: current.addr,
   Token: token}`) and call `ListApps`. On error, banner it in the form
   (`⚠ token rejected: …` for a 401, `⚠ can't reach box: …` otherwise) and do
   **not** write — the field stays focused.
3. On success, persist the token to the **current box** and re-dial the live
   session (same mechanism as a phase-5 edit of the current box): emit a
   `loginSavedMsg{token}`; the root writes the token onto the current `Box` via
   the existing `saveBox(box, replacing=current.name)` helper and re-dials, then
   pops home.

**Target-type seam.** The view holds a `target` field (an enum with a single
value `targetLAN` today). `newLoginView` takes the current box so a future relay
branch can read/populate relay fields. The parent design's "target type" chooser
is a no-op for one option now; adding relay later means a second enum value + a
device-flow branch in `Update`, not a rewrite. This is called out in a code
comment so the interim nature is explicit at the seam.

**Footer:** `↵ verify & save · esc cancel · ? help`.

### 3. Unauthenticated hint on apps home

The parent design (`:74`): "If unauthenticated, a hint bar points at `L`." When
the current box has no valid token, every poll lands as an `errMsg` carrying a
401. The apps view renders a hint bar — `not logged in — press L to log in` —
instead of the raw error banner when the last error is an **unauthorized** one.

Detecting 401 without importing `internal/client` (the seam phase 5 preserved):
add a one-line `func (e *StatusError) Unauthorized() bool { return e.Code ==
http.StatusUnauthorized }` to `internal/client`, and classify in `tui` via a
local anonymous interface — no client import:

```go
func isUnauthorized(err error) bool {
    var u interface{ Unauthorized() bool }
    return errors.As(err, &u) && u.Unauthorized()
}
```

### 4. `github.go` — GitHub setup wizard (`g` from apps home)

A leaf view pushed by the root on `g` (global key, same wiring as `L`). One
**org** text field (blank = personal account). `↵` starts the manifest flow,
which mirrors `cmd/piper`'s `githubSetup` (`main.go:477`) but bridged into Bubble
Tea as a `tea.Cmd`:

1. `net.Listen("127.0.0.1:0")` a **callback** server (catches GitHub's
   `?code=`) and a **form** server (serves the auto-submitting manifest form).
2. `Manifest(redirect)` from the API, build the form page, open the browser at
   the form URL via an injected `openBrowser` seam.
3. Block on the callback channel (5-minute deadline, like the CLI), then
   `ExchangeGitHub(code)`.

While the command runs the view shows a spinner + the form URL
(`waiting for GitHub App approval… <url>`). The command lands as
`githubDoneMsg{err}`: success banners `GitHub App configured` briefly and pops;
error banners inline and stays. `esc` cancels — closes the servers (context
cancel) and pops; a late callback is dropped.

**Seams for tests** (no real browser, no real GitHub, no bound sockets in unit
tests): the browser-open is an injected `openBrowser func(string) error` field
(defaulting to the real one, like `cmd/piper`'s `openBrowserFn`), and the whole
flow is one `tea.Cmd` whose collaborators are injected. Unit tests drive the
state machine by feeding `githubDoneMsg{…}` directly and asserting the
spinner/banner/pop transitions and that a blank vs. named org selects the right
`settings/apps/new` URL; the socket-level plumbing is covered by the existing CLI
`githubSetup` tests and the e2e path, not re-exercised here.

Because `github.go` needs `net`/`http`/`html` plumbing that the pure state-machine
views avoid, the flow-runner is a small helper (`runManifestFlow`) kept separate
from `Update`, so `Update` stays a pure `(msg) → (model, cmd)` machine that tests
feed messages to.

### 5. `linkform.go` — link repo (`l` from app detail, depth 2)

A leaf view pushed by **app detail** on `l` (app pre-selected — parent design
puts Link repo at depth 2 under App detail). `capturesText()` true. Two fields:
**repo** (`owner/name`) and **branch** (default `main`). On `↵`:

1. Reject an empty repo inline.
2. Emit a `linkAppMsg{name, repo, branch}`; the root owns the client, runs
   `LinkApp` off the UI thread, and reports via the existing `actionResultMsg`
   (phase 4). Success pops back to app detail and refreshes (so the header's repo
   line updates); error banners in the form.

App detail's footer gains `l link`:
`d deploy · s stop · x delete · l link · ↵ logs · r refresh · esc back · ? help`.

### 6. Help overlay

The `?` overlay (phase-4 discoverability) gains rows for the three new keys:
`L login` and `g github` under Global/Apps, `l link` under App detail.

## Architecture / layering

Pure `internal/tui` (three new leaf-view files + root wiring in `app.go`, message
types in `tui.go`, `fakeAPI` methods in tests) plus **one surgical
`internal/client` addition**: `StatusError.Unauthorized()`. The parent design
sanctions `tui` importing `internal/client`, but phases 1–5 kept `tui` free of it
via the `API`/`Dialer` seams; phase 6 preserves that — the new `Unauthorized()`
is consumed through a local anonymous interface, not a client import. No
`piperd`/`piper-relay` change, no API *wire* change (the three methods already
exist on the client). `github.go` is the one view that reaches for `net`/`http`;
its socket plumbing lives in a helper beside — not inside — the `Update` machine.
Nothing imports up.

## Testing (TDD, failing-test-first)

Views are pure `Update(msg) → (model, cmd)` state machines; tests are
table-driven Go with a fake `API` behind the seam and the phase-5 `fakeDialer`.
Reuse the existing helpers (`keyRunes`, `keyEnter`, `keyBackspace`, `pump`,
`fakeAPI`, `fakeDialer`, `seedConfig`); do not redefine them.

1. **Login verifies & saves** — a valid token whose `ListApps` probe succeeds
   emits `loginSavedMsg`; at the root it writes the token to the current box
   (assert the on-disk `ClientFile` under a temp `HOME`) and re-dials.
2. **Login — bad token** — a probe returning a 401 banners `token rejected` in
   the form and does **not** write the config file; the field stays focused.
3. **Login — empty token** — inline `token required`; no probe, no write.
4. **Unauthorized hint** — an apps view whose last poll `errMsg` wraps a
   401-classified error renders the `press L` hint bar (not the raw banner); a
   non-401 error still banners normally. Covers `isUnauthorized` + the
   `StatusError.Unauthorized()` helper.
5. **`L` opens login** — `L` from the apps view pushes the login view; a text
   field on top swallows a literal `L` (the `topCapturesText` guard).
6. **GitHub org URL** — the flow targets `github.com/settings/apps/new` for a
   blank org and `.../organizations/<org>/settings/apps/new` for a named one.
7. **GitHub done** — `githubDoneMsg{nil}` banners success and pops; `githubDoneMsg{err}`
   banners the error and stays; `esc` mid-flow pops.
8. **`g` opens github** — `g` from apps pushes the github view (with the
   `topCapturesText` guard).
9. **Link repo** — a valid submit emits `linkAppMsg{name, repo, branch}`; the
   root's `LinkApp` success pops to app detail and refreshes; an empty repo is
   rejected inline; a `LinkApp` error banners in the form.
10. **`l` opens link / help** — `l` from app detail pushes the link form; the `?`
    overlay contains the `L`/`g`/`l` rows.

## Out of scope / future

- **Relay login** — the login wizard is **LAN token only** for now (see §2). The
  relay target (GitHub device-flow → account credential, and `connect`
  enrollment) stays in the CLI (`piper login`/`connect`) and lands in a later
  phase through the target-type seam. This is a **temporary** split, flagged in
  the spec and in code.
- **GitHub App install / repo discovery** — the wizard configures the App; the
  user still installs it on the repo and links via the link form (same as the
  CLI's closing hint). No repo picker.
- **Relay-box switching** — still deferred (a saved relay `Box` needs the
  `--remote`/agent mapping the future relay-login work defines).
