# Homebrew Tap Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `brew install piperbox/tap/piper` installs both binaries and `brew services start piper` runs a durable piperd on macOS, with the formula published automatically by goreleaser on every stable release.

**Architecture:** goreleaser gains a combined archive (`piper` + `piperd` in one tarball per platform — goreleaser's brew pipe errors on two archives per OS/arch, so one formula requires one archive) and a `brews:` publisher that renders the formula — install block, `service do` block, caveats, test — and pushes it to a new `piperbox/homebrew-tap` repo on stable tags. The tap repo carries Homebrew's own `test-bot` CI. The service runs piperd with data under brew's `var/"piper"`; ports need no pinning (macOS ≥10.14 allows unprivileged wildcard binds on :80/:443).

**Tech Stack:** goreleaser v2 `brews:` (deprecated-but-functional — casks cannot declare `brew services` blocks, so this is deliberate), Homebrew formula + service DSL, `brew test-bot` CI, GitHub fine-grained PAT.

This is **plan 2 of 4** of [the onboarding & packaging spec](../specs/2026-07-30-onboarding-packaging-design.md) (deliverable 2, issue #445). Plan 1 (deb + apt, #444) is live as of v0.13.0. All facts below about goreleaser/brew behavior were verified against current docs/source on 2026-07-30.

## Global Constraints

- One formula, named **`piper`**, installing **both** binaries; tap repo **`piperbox/homebrew-tap`**, formula at **`Formula/piper.rb`**.
- Combined archive id **`piper-bundle`**, name template `piper-bundle_{{ .Version }}_{{ .Os }}_{{ .Arch }}{{ if .Arm }}v{{ .Arm }}{{ end }}` — adds 5 release assets (2 darwin + 3 linux), total **30** from the next release.
- `brews.skip_upload: auto` — RC tags (prerelease) never touch the tap.
- Service block: `run [opt_bin/"piperd"]`, `keep_alive true`, logs under `var/"log/"`, `XDG_DATA_HOME`/`XDG_CONFIG_HOME` pinned to `var/"piper"`. No port env pinning.
- `brew upgrade` does **not** auto-restart services (verified) — caveats must tell users `brew services restart piper`.
- Token: goreleaser reads **`HOMEBREW_TAP_GITHUB_TOKEN`** (a second secret carrying the same fine-grained PAT as `APT_REPO_TOKEN`, after the user extends that PAT's repo access to `piperbox/homebrew-tap`). The apt path and its secret are not touched.
- Shell: POSIX sh, `#!/bin/sh`, `set -eu`, tabs. `make verify` before every commit in the piper repo. Conventional commits, `Part of #445`, trailer `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`.
- The formula will contain an `on_linux` block (goreleaser has no OS filter for archives; harmless, and Linuxbrew users get a working install). Do not fight it.

---

### Task 1: Combined archive + brews publisher + snapshot verification

**Files:**
- Modify: `.goreleaser.yaml` (add `piper-bundle` archive; add `brews:`)
- Modify: `.github/workflows/release.yml` (pass the tap token env)
- Create: `test/packaging/verify_brew.sh`
- Modify: `.github/workflows/ci.yml` (run verify_brew.sh next to verify_deb.sh)
- Modify: `.claude/skills/release/SKILL.md` (asset count 25 → 30; tap-push check)

**Interfaces:**
- Consumes: existing build ids `piper`, `piperd`; the CI `packaging` paths-filter and snapshot step from plan 1 (already in ci.yml).
- Produces: release assets `piper-bundle_<version>_<os>_<arch>.tar.gz` (5 per release); a formula pushed to `piperbox/homebrew-tap` `Formula/piper.rb` on stable tags (consumed by Task 2's CI and Task 4's smoke); release.yml exposes `HOMEBREW_TAP_GITHUB_TOKEN` to the goreleaser step.

- [ ] **Step 1: Write the failing verification script**

`test/packaging/verify_brew.sh`:

```sh
#!/bin/sh
# Asserts a goreleaser snapshot produced the combined brew archives. Run after:
#   goreleaser release --snapshot --clean
# The generated formula itself is validated at release time by the tap's CI
# (test-bot audits + installs it) — snapshot mode skips the publish pipe that
# renders it, so only the tarballs are checkable here.
set -eu
dist="${1:-dist}"
fail() { echo "verify_brew: $*" >&2; exit 1; }

for combo in darwin_amd64 darwin_arm64 linux_amd64 linux_arm64 linux_armv7; do
	set -- "$dist"/piper-bundle_*_"$combo".tar.gz
	[ -e "$1" ] || fail "missing piper-bundle for $combo"
done

set -- "$dist"/piper-bundle_*_darwin_arm64.tar.gz; bundle="$1"
for bin in piper piperd; do
	tar tzf "$bundle" | grep -qx "$bin" || fail "$bin missing from $bundle"
done
echo "verify_brew: ok"
```

`chmod +x test/packaging/verify_brew.sh`

- [ ] **Step 2: Run to verify failure**

```bash
goreleaser release --snapshot --clean
./test/packaging/verify_brew.sh
```

Expected: FAIL with `missing piper-bundle for darwin_amd64` (no combined archive yet).

- [ ] **Step 3: Add the archive and brews config**

In `.goreleaser.yaml`, append to the `archives:` list:

```yaml
  - id: piper-bundle
    ids: [piper, piperd]
    name_template: "piper-bundle_{{ .Version }}_{{ .Os }}_{{ .Arch }}{{ if .Arm }}v{{ .Arm }}{{ end }}"
```

and add a top-level `brews:` section (after `nfpms:`):

```yaml
# `brews` is deprecated in favor of homebrew_casks, but casks cannot declare
# `brew services` blocks — a formula with a `service do` block is the whole
# point, so the deprecation warning is accepted deliberately (spec 2026-07-30).
brews:
  - name: piper
    ids: [piper-bundle]
    repository:
      owner: piperbox
      name: homebrew-tap
      branch: main
      token: "{{ .Env.HOMEBREW_TAP_GITHUB_TOKEN }}"
    directory: Formula
    homepage: https://github.com/piperbox/piper
    description: "Git push -> live HTTPS URL on hardware you own"
    license: Apache-2.0
    # auto: prerelease (RC) tags never touch the tap.
    skip_upload: auto
    install: |
      bin.install "piper"
      bin.install "piperd"
    caveats: |
      Run the Piper agent now and at every login:
        brew services start piper
      Boot-durable instead (headless Macs, runs as root):
        sudo brew services start piper
      brew upgrade does NOT restart a running agent — after upgrading run:
        brew services restart piper
    service: |
      run [opt_bin/"piperd"]
      keep_alive true
      log_path var/"log/piperd.log"
      error_log_path var/"log/piperd.err.log"
      environment_variables XDG_DATA_HOME: var/"piper", XDG_CONFIG_HOME: var/"piper"
    test: |
      assert_match version.to_s, shell_output("#{bin}/piper --version")
```

- [ ] **Step 4: Run to verify pass**

```bash
goreleaser check
goreleaser release --snapshot --clean
./test/packaging/verify_deb.sh
./test/packaging/verify_brew.sh
```

Expected: `goreleaser check` warns about the `brews` deprecation (accepted) but exits 0; both verify scripts print ok. If `goreleaser check` treats the deprecation as an error, that would be a goreleaser behavior change — stop and report BLOCKED rather than switching to casks.

- [ ] **Step 5: Wire CI and the release workflow**

In `.github/workflows/ci.yml`, directly after the `Verify deb contents` step:

```yaml
      - name: Verify brew archives
        if: steps.changes.outputs.packaging == 'true'
        run: ./test/packaging/verify_brew.sh
```

In `.github/workflows/release.yml`, add to the goreleaser step's `env:` block (alongside `GITHUB_TOKEN` and `GORELEASER_CURRENT_TAG`):

```yaml
          HOMEBREW_TAP_GITHUB_TOKEN: ${{ secrets.HOMEBREW_TAP_GITHUB_TOKEN }}
```

- [ ] **Step 6: Update the release skill's expectations**

In `.claude/skills/release/SKILL.md`:

1. Change `**Expect 25 assets.**` to `**Expect 30 assets.**` and extend that sentence's list with the five `piper-bundle` tarballs.
2. Extend the asset-count regression note with: `v0.14.0 added the five piper-bundle tarballs (25 → 30).`
3. After the apt-repo smoke section, add a new `### Homebrew tap (stable tags only)` section whose body says: "goreleaser pushes the updated formula to piperbox/homebrew-tap. Confirm it landed and CI passed:" followed by one `sh` fenced block containing exactly these two commands:

```sh
gh api repos/piperbox/homebrew-tap/commits/main -q '.commit.message'   # names the new version
gh run list -R piperbox/homebrew-tap -L 1
```

- [ ] **Step 7: `make verify`, commit, push**

```bash
git add .goreleaser.yaml .github/workflows/ci.yml .github/workflows/release.yml test/packaging/verify_brew.sh .claude/skills/release/SKILL.md
git commit -m "feat: brew formula via goreleaser — piper-bundle archive + brews publisher

Part of #445

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
git push -u origin HEAD
```

---

### Task 2: Tap repo scaffold (README + test-bot CI)

Built in a **local scratch clone** at `~/scratch/piperbox-homebrew-tap` (`mkdir -p`, `git init -b main`); pushed in Task 3. The formula itself is NOT hand-written — goreleaser pushes it on the first stable release after Task 1 merges.

**Files (in the tap repo):**
- Create: `README.md`, `.github/workflows/tests.yml`

**Interfaces:**
- Consumes: nothing from other tasks.
- Produces: the repo content Task 3 pushes; CI that runs on goreleaser's formula pushes (Task 4 checks it).

- [ ] **Step 1: Write the CI workflow**

`.github/workflows/tests.yml` (Homebrew's own `brew tap-new` template, matrix trimmed — `setup-homebrew` taps the checkout automatically; `--only-formulae` does a real install + `test do` run on PRs and pushes):

```yaml
name: tests

on:
  pull_request:
  push:
    branches: [main]

jobs:
  test-bot:
    strategy:
      matrix:
        os: [macos-latest]
    runs-on: ${{ matrix.os }}
    steps:
      - name: Set up Homebrew
        id: set-up-homebrew
        uses: Homebrew/actions/setup-homebrew@master
        with:
          token: ${{ secrets.GITHUB_TOKEN }}

      - run: brew test-bot --only-cleanup-before
      - run: brew test-bot --only-setup
      - run: brew test-bot --only-tap-syntax
      - run: brew test-bot --only-formulae
```

- [ ] **Step 2: Write the README**

`README.md`:

```markdown
# piperbox/homebrew-tap

Homebrew tap for [Piper](https://github.com/piperbox/piper). The formula is
generated and pushed by goreleaser on every stable Piper release — do not
edit `Formula/piper.rb` by hand.

## Use it

    brew install piperbox/tap/piper
    brew services start piper        # runs piperd now and at every login
    sudo brew services start piper   # boot-durable instead (headless Macs)

After `brew upgrade`: `brew services restart piper` (upgrade does not
restart a running agent).
```

- [ ] **Step 3: Sanity-check the workflow YAML and commit**

```bash
python3 -c "import yaml,sys; yaml.safe_load(open('.github/workflows/tests.yml'))" && echo yaml-ok
git add -A
git commit -m "chore: tap scaffold — README + test-bot CI

Part of piperbox/piper#445

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

(If PyYAML is missing locally, `python3 -m pip install --user pyyaml` or fall back to `ruby -ryaml -e "YAML.load_file('.github/workflows/tests.yml')"` — macOS ships ruby.)

---

### Task 3: Infra (tap repo live + token)

**Files:** none (GitHub/secret state). **[USER]** marks Faruk-only steps.

**Interfaces:**
- Consumes: the scratch repo from Task 2.
- Produces: live `piperbox/homebrew-tap`; secret `HOMEBREW_TAP_GITHUB_TOKEN` in piperbox/piper that release.yml (Task 1) reads.

- [ ] **Step 1: Create and push the repo**

```bash
gh repo create piperbox/homebrew-tap --public --description "Homebrew tap for Piper — brew install piperbox/tap/piper" --disable-wiki
cd ~/scratch/piperbox-homebrew-tap && git remote add origin git@github.com:piperbox/homebrew-tap.git && git push -u origin main
```

- [ ] **Step 2: [USER] Extend the existing PAT** — edit the fine-grained PAT behind `APT_REPO_TOKEN` (github.com/settings/personal-access-tokens → the token → Repository access) to add `piperbox/homebrew-tap`. Permission stays Contents read/write. No new PAT.

- [ ] **Step 3: [USER] Add the secret** — same PAT value under the name goreleaser reads:

```bash
gh secret set HOMEBREW_TAP_GITHUB_TOKEN -R piperbox/piper
```

(paste the token at the prompt).

---

### Task 4: PROGRESS + PR, then release-riding verification

**Files:**
- Modify: `PROGRESS.md`

**Interfaces:**
- Consumes: everything above; the release skill (updated in Task 1).
- Produces: PR for #445; the first formula publish rides the next stable release.

- [ ] **Step 1: Update PROGRESS.md** — under the apt-repo line, add: `- 🟡 Homebrew tap: piperbox/homebrew-tap wired (formula via goreleaser, brew services block); first publish rides the next stable release — [#445](https://github.com/piperbox/piper/issues/445)`

- [ ] **Step 2: `make verify`, commit, open the PR**

```bash
git add PROGRESS.md
git commit -m "docs: track Homebrew tap in PROGRESS

Part of #445

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
git push
gh pr create --base main --title "Homebrew tap: piper formula with brew services, published by goreleaser" \
  --body "Deliverable 2 of the onboarding & packaging spec (docs/superpowers/specs/2026-07-30-onboarding-packaging-design.md): combined piper-bundle archive, brews publisher with service/caveats/test blocks, tap repo with test-bot CI, CI snapshot verification. First formula publish rides the next stable release. Part of #445.

🤖 Generated with [Claude Code](https://claude.com/claude-code)"
```

- [ ] **Step 3: After squash-merge — cut the next stable release** per the `release` skill (v0.14.0; minor: new brew channel). Expect **30 assets**. The release pushes `Formula/piper.rb` to the tap; confirm per the skill's new tap check (commit message names the version, tap CI run green — test-bot actually installs the formula and runs its `test do` block on a mac runner).

- [ ] **Step 4: Local smoke (non-invasive).** This Mac runs its own piperd LaunchAgent, so do NOT `brew services start` here (port fights). Instead:

```bash
brew install piperbox/tap/piper
piper --version && piperd --version   # both print the new release; check `which piper` — brew's copy may be shadowed by ~/.local/bin
brew audit --strict piperbox/tap/piper
brew uninstall piper   # leave the machine as it was
```

Expected: install + versions + audit clean. Full `brew services start` verification belongs to a Mac without a hand-rolled agent (or after plan 3 migrates this one).

- [ ] **Step 5: Close out** — comment on #445 with the release tag + tap CI run link, close it, and flip the PROGRESS 🟡 to ✅ in the plan-3 PR (or a one-line docs PR if plan 3 is far off).
