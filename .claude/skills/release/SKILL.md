---
name: release
description: Cut a Piper release — choosing the version, the green-tip gate, tagging, verifying goreleaser's output, smoke-testing the published installer, and publishing notes. Use when asked to cut, tag, or publish a release, or to promote an RC to final.
---

# Cutting a Piper release

Releases are semver git tags. Pushing a `v*` tag triggers `.github/workflows/release.yml`, which runs goreleaser and publishes a GitHub Release. There is nothing else to run — but the tag is irreversible in practice, so the gates below come first.

Pre-1.0 (`0.x`), breaking changes may land in minor bumps. See CLAUDE.md's compatibility policy.

## 1. Preconditions

- On `main`, clean tree, synced with origin.
- `make verify` clean locally.

## 2. The gate: green on the *exact* tip

**The release workflow does not re-run e2e.** Whatever you tag is what ships, so verify CI + e2e + Code Quality all succeeded on the exact SHA you are about to tag:

```sh
TIP=$(git rev-parse main)
gh run list --branch main --limit 25 --json headSha,name,conclusion \
  --jq ".[] | select(.headSha==\"$TIP\") | \"\(.name)\t\(.conclusion)\""
```

Do not use `gh run list --commit <sha>` — it has returned empty unreliably. Filter on `headSha` as above.

**If a push-event run is missing entirely** (GitHub has occasionally fired no push workflows for a squash-merge), tree-equality against a green PR run is an acceptable substitute: confirm `git rev-parse <release-commit>^{tree}` equals the tree of the PR head that e2e tested.

## 3. Choose the version

Ask the user rather than assuming — but bring a recommendation.

**Minor bump when the release breaks anything a user must act on.** Precedents:

- `v0.5.0` — breaking API response shape
- `v0.6.0` — new columns on existing tables, so an in-place DB needs replacing
- `v0.8.0` — macOS launchd label changed, requiring a manual `launchctl bootout`

**Patch** only when nothing requires user action. A patch that silently breaks an upgrade path is worse than a minor: the version number is the only signal most users read.

### Schema changes are subtler than they look

Both the agent and relay stores apply `schema.sql` with `CREATE TABLE IF NOT EXISTS` and there are no migrations anywhere.

- **New table** → materializes on an existing DB. Additive, no re-enroll.
- **New column on an existing table** → does **not** materialize. Needs a fresh DB and re-enrollment.

Check `git diff <last-release-tag>..main -- '*/schema.sql'` and classify before writing upgrade notes.

### RC or straight to final?

Use an RC when the release carries risk the tests can't cover — infrastructure changes, publishing-pipeline changes, or anything needing real-hardware validation. Otherwise tag final directly off green `main`.

If the RC ends up sitting exactly on the tip of `main` with zero commits since, promotion is a clean **same-commit re-tag** — no rebuild, no divergence.

## 4. Tag and push

```sh
git tag -a v0.8.0 -m "piper v0.8.0" <sha>
git push origin v0.8.0
```

The workflow takes ~4 minutes. goreleaser auto-marks `rc`/`beta`/`alpha` tags as pre-releases.

## 5. Verify what actually published

**Expect 18 assets.** Three binaries (`piper`, `piperd`, `piper-relay`) × 5 platforms (linux amd64/arm64/armv7, darwin amd64/arm64) = 15, plus `install.sh`, `piper-relay.service`, and `checksums.txt`.

```sh
gh release view <tag> --json assets --jq '.assets | length'
```

Releases before `v0.7.0` had 22 — the four `piperd` unit/env files are no longer published because the CLI embeds them. If you see 22, something regressed.

### Smoke-test the published installer

A green workflow only proves goreleaser ran. It does not prove the installer works. Run the *published* script against a throwaway prefix:

```sh
SB=$(mktemp -d)
curl -fsSL https://raw.githubusercontent.com/piperbox/piper/main/install.sh \
  | PIPER_PREFIX="$SB" PIPER_CLI_ONLY=1 sh
"$SB/piper" --version
```

Add `PIPER_RC=1` for a prerelease — without it the installer resolves `releases/latest`, which skips prereleases by design.

This matters most when anything about repo identity, hosting, or the installer itself changed.

### apt repo smoke (stable tags only)

A stable tag (no `-` suffix) dispatches a publish to `piperbox/apt`. Confirm the run went green:

```sh
gh run list -R piperbox/apt -w Publish -L 1
```

Then smoke-test the published repo end-to-end:

```sh
docker run --rm debian:stable sh -c 'apt-get update -qq && apt-get install -y -qq curl ca-certificates >/dev/null && mkdir -p /etc/apt/keyrings && curl -fsSL https://apt.piperbox.dev/piperbox.gpg -o /etc/apt/keyrings/piperbox.gpg && curl -fsSL https://apt.piperbox.dev/piperbox.sources -o /etc/apt/sources.list.d/piperbox.sources && apt-get update -qq && apt-get install -y piperd piper && piper --version && piperd --version'
```

Expected: both versions print the tag just cut. If it times out on DNS/cert propagation (first publish only), re-run once before debugging.

## 6. Publish the notes

goreleaser's auto-body is a bare commit list. Always hand-write them.

```sh
gh release edit <tag> \
  --title "v0.8.0 — short description of the headline change" \
  --notes-file notes.md \
  --prerelease=false --latest --verify-tag
```

Use `--prerelease` (not `--prerelease=false --latest`) for an RC. **An RC must never take the `latest` pointer.**

Notes should cover, when applicable:

- Any **manual upgrade step**, stated up front — not buried under a feature list
- Whether a **fresh DB / re-enrollment** is needed (see the schema rule above)
- **Relay-before-agents ordering**, whenever the agent↔relay wire protocol or a broker path changed. Deploying the hosted relay is a separate operator task, outside this skill.
- Known issues shipped open, with issue links — this project has a precedent of disclosing them rather than staying silent

Verify the pointers landed:

```sh
gh release list --limit 5 --json tagName,isLatest,isPrerelease
```

`gh release view --json isLatest` is **not** valid — only the list form exposes it.

## 7. Afterwards

Update `PROGRESS.md` if the release completes anything tracked there, and leave issues to be closed by their PRs rather than by hand.
