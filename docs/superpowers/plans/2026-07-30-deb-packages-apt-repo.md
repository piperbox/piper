# deb Packages + apt Repository Pipeline Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `sudo apt install piperd piper` works from a self-hosted, signed apt repository at apt.piperbox.dev, published automatically on every stable release, with the deb's postinst owning service enable/start/restart.

**Architecture:** goreleaser's built-in nfpm builds two `.deb`s (`piperd` with systemd unit + env conffile + maintainer scripts; `piper` CLI-only) and attaches them to the GitHub Release. The release workflow fires a `repository_dispatch` at a new `piperbox/apt` repo, whose workflow mirrors the live pool from apt.piperbox.dev, adds the new debs, rebuilds + signs the metadata with `dpkg-scanpackages`/`apt-ftparchive`/GPG, deploys to GitHub Pages, and self-verifies with a real `apt install` in a Debian container.

**Tech Stack:** goreleaser v2 (nfpm), POSIX sh, GitHub Actions (`upload-pages-artifact`/`deploy-pages`), `dpkg-scanpackages` + `apt-ftparchive` + gnupg, GitHub Pages + CNAME.

This is **plan 1 of 4** of [the onboarding & packaging spec](../specs/2026-07-30-onboarding-packaging-design.md). Read the spec first. Plans 2–4 (brew tap, CLI one-tier rewrite, installer dispatch + docs) are written after their predecessors land.

## Global Constraints

- `CGO_ENABLED=0` everywhere; module path `github.com/piperbox/piper`; Go 1.26.
- Shell is POSIX sh (`#!/bin/sh`, `set -eu`, tabs for indentation — match `install.sh`).
- deb arches: **amd64, arm64, armhf**. The `armhf` naming comes from nfpm's `{{ .ConventionalFileName }}` template, which MUST be set explicitly on every nfpms entry — goreleaser's *default* deb naming is `piperd_<ver>_linux_armv7.deb`, which would break the verify script, the prune regex, and `dpkg-scanpackages --arch`.
- Binaries in **`/usr/bin`**; unit at **`/usr/lib/systemd/system/piperd.service`** with `ExecStart=/usr/bin/piperd`; env conffile **`/etc/piper/piperd.env`** mode **0600**.
- apt repo: suite **stable**, component **main**; **stable tags only** (a tag containing `-` is an RC and never publishes to apt); keep the newest **3** versions per package/arch; signing key is **RSA 4096, no expiry**; users configure via a **deb822 `.sources`** file with `Signed-By`.
- Maintainer string: `piperbox maintainers <noreply@piperbox.dev>`; license `Apache-2.0`.
- Commits: conventional style, one per plan step-group, each ending with `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`, referencing the Task-0 issue (`Part of #N`).
- Run `make verify` before every commit that touches Go or `.goreleaser.yaml`.

---

### Task 0: File the four delivery issues

**Files:** none (GitHub only).

**Interfaces:**
- Produces: four issue numbers; this plan's commits reference the first (`Part of #<deb-apt issue>`). Record all four numbers in the PR body draft.

- [ ] **Step 1: File the issues** (labels per CLAUDE.md: type + priority + size + binary):

```bash
gh issue create --title "[repo] deb packages + self-hosted apt repo at apt.piperbox.dev" \
  --label enhancement,P1,size/L,agent,cli \
  --body "nfpm debs (piperd with unit+conffile+postinst enable/restart, piper CLI), dedicated piperbox/apt Pages repo, dispatch-triggered signed publish, self-verifying. Deliverable 1 of docs/superpowers/specs/2026-07-30-onboarding-packaging-design.md."
gh issue create --title "[repo] Homebrew tap: piperbox/homebrew-tap formula with brew services" \
  --label enhancement,P1,size/M,agent,cli \
  --body "Formula installs piper+piperd, service block runs piperd (brew services). Deliverable 2 of docs/superpowers/specs/2026-07-30-onboarding-packaging-design.md."
gh issue create --title "[cli] one-tier lifecycle: delete daemonize/rootless/LaunchAgent, agent wraps system service + brew" \
  --label enhancement,P1,size/L,cli \
  --body "Deliverable 3 of docs/superpowers/specs/2026-07-30-onboarding-packaging-design.md. Lands only after deliverables 1+2."
gh issue create --title "[repo] installer platform dispatch (apt/brew bootstrap) + docs overhaul" \
  --label enhancement,P1,size/M,cli \
  --body "install.sh detects deb-family/brew and hands off to the native channel; diet fallback elsewhere; README/getting-started/manual-setup rewrite. Deliverable 4 of docs/superpowers/specs/2026-07-30-onboarding-packaging-design.md."
```

- [ ] **Step 2: Note the first issue's number** — every commit below says `Part of #<that number>`.

---

### Task 1: deb packaging assets + Go contract tests

**Files:**
- Create: `packaging/deb/piperd.service`, `packaging/deb/postinstall.sh`, `packaging/deb/preremove.sh`
- Test: `packaging/deb/deb_test.go`

**Interfaces:**
- Consumes: `packaging/systemd/piperd.service` (the existing `/usr/local/bin` unit — stays untouched for the diet/manual path).
- Produces: the three asset paths above, referenced verbatim by Task 2's nfpm config.

- [ ] **Step 1: Write the failing contract tests**

`packaging/deb/deb_test.go` (mirrors the style of `packaging/systemd/piperd_test.go`):

```go
package deb

import (
	"os"
	"strings"
	"testing"
)

// TestDebUnitContract pins the deb variant of the unit: identical hardening to
// the manual-install unit, but ExecStart points at the deb's /usr/bin.
func TestDebUnitContract(t *testing.T) {
	b, err := os.ReadFile("piperd.service")
	if err != nil {
		t.Fatal(err)
	}
	unit := string(b)
	if !strings.Contains(unit, "ExecStart=/usr/bin/piperd") {
		t.Error("deb unit must ExecStart=/usr/bin/piperd")
	}
	if strings.Contains(unit, "/usr/local/bin") {
		t.Error("deb unit must not reference /usr/local/bin")
	}
}

// TestDebUnitOnlyDiffersInExecStart guards the two units against drifting
// apart: any hardening change must land in both.
func TestDebUnitOnlyDiffersInExecStart(t *testing.T) {
	deb, err := os.ReadFile("piperd.service")
	if err != nil {
		t.Fatal(err)
	}
	manual, err := os.ReadFile("../systemd/piperd.service")
	if err != nil {
		t.Fatal(err)
	}
	norm := func(b []byte) string {
		return strings.ReplaceAll(string(b), "ExecStart=/usr/bin/piperd", "ExecStart=/usr/local/bin/piperd")
	}
	if norm(deb) != string(manual) {
		t.Error("deb unit and packaging/systemd/piperd.service differ beyond ExecStart — sync them")
	}
}

// TestMaintainerScripts pins the dpkg maintainer-script contract: fresh
// install enables+starts, upgrade restarts only a running service, removal
// disables — all skipped outside systemd (containers/chroots).
func TestMaintainerScripts(t *testing.T) {
	post, err := os.ReadFile("postinstall.sh")
	if err != nil {
		t.Fatal(err)
	}
	pre, err := os.ReadFile("preremove.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`[ "$1" = configure ]`,
		"[ -d /run/systemd/system ]",
		"systemctl daemon-reload",
		"systemctl enable --now piperd",
		"systemctl restart piperd",
	} {
		if !strings.Contains(string(post), want) {
			t.Errorf("postinstall.sh missing %q", want)
		}
	}
	for _, want := range []string{
		`[ "$1" = remove ]`,
		"[ -d /run/systemd/system ]",
		"systemctl disable --now piperd",
	} {
		if !strings.Contains(string(pre), want) {
			t.Errorf("preremove.sh missing %q", want)
		}
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./packaging/deb/`
Expected: FAIL — `no such file or directory` for `piperd.service`.

- [ ] **Step 3: Write the assets**

`packaging/deb/piperd.service` — copy `packaging/systemd/piperd.service` byte-for-byte, then change only the ExecStart line to:

```
ExecStart=/usr/bin/piperd
```

`packaging/deb/postinstall.sh`:

```sh
#!/bin/sh
# dpkg runs this as `postinst configure <old-version>`; $2 is empty on a fresh
# install. Outside systemd (containers, chroots) the files land but nothing is
# started, per Debian convention.
set -eu
[ "$1" = configure ] || exit 0
[ -d /run/systemd/system ] || exit 0
systemctl daemon-reload
if [ -z "${2:-}" ]; then
	systemctl enable --now piperd
elif systemctl is-active --quiet piperd; then
	# Upgrade of a running service: restart so the new binary actually serves
	# (#375). A deliberately stopped piperd stays stopped.
	systemctl restart piperd
fi
```

`packaging/deb/preremove.sh`:

```sh
#!/bin/sh
# dpkg runs this as `prerm remove|upgrade ...`; tear down only on real removal.
set -eu
[ "$1" = remove ] || exit 0
[ -d /run/systemd/system ] || exit 0
systemctl disable --now piperd || true
```

`chmod +x packaging/deb/postinstall.sh packaging/deb/preremove.sh`

- [ ] **Step 4: Run tests to verify pass**

Run: `go test ./packaging/deb/`
Expected: PASS (3 tests).

- [ ] **Step 5: `make verify`, then commit**

```bash
git add packaging/deb/
git commit -m "feat: deb packaging assets — /usr/bin unit variant + maintainer scripts

Part of #<deb-apt issue>

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 2: nfpm config in goreleaser + snapshot verification + CI wiring

**Files:**
- Modify: `.goreleaser.yaml` (add `nfpms:`; extend `release.extra_files`)
- Create: `test/packaging/verify_deb.sh`
- Modify: `.github/workflows/ci.yml` (verify job: packaging filter + two gated steps)

**Interfaces:**
- Consumes: `packaging/deb/piperd.service`, `packaging/deb/postinstall.sh`, `packaging/deb/preremove.sh` (Task 1); existing build ids `piperd`, `piper`.
- Produces: release assets named `piperd_<version>_<arch>.deb` / `piper_<version>_<arch>.deb` for amd64/arm64/armhf (via the explicit `file_name_template: "{{ .ConventionalFileName }}"` — NOT goreleaser's default naming), plus `piperd.service` as a release extra file. Task 3's `gh release download -p '*.deb'`, the mkrepo prune regex, and `dpkg-scanpackages --arch` all rely on exactly this naming.

- [ ] **Step 1: Write the failing verification script**

`test/packaging/verify_deb.sh`:

```sh
#!/bin/sh
# Asserts a goreleaser snapshot produced well-formed debs. Run after:
#   goreleaser release --snapshot --clean
# Skips cleanly when dpkg-deb is unavailable (macOS without `brew install dpkg`),
# matching the repo's Docker-dependent-test convention.
set -eu
command -v dpkg-deb >/dev/null 2>&1 || { echo "verify_deb: skip (no dpkg-deb)"; exit 0; }
dist="${1:-dist}"
fail() { echo "verify_deb: $*" >&2; exit 1; }

for pkg in piperd piper; do
	for arch in amd64 arm64 armhf; do
		set -- "$dist"/${pkg}_*_"$arch".deb
		[ -e "$1" ] || fail "missing ${pkg} ${arch} deb"
	done
done

set -- "$dist"/piperd_*_amd64.deb; deb="$1"
dpkg-deb -c "$deb" | grep -q '\./usr/bin/piperd$' || fail "piperd binary not in /usr/bin"
dpkg-deb -c "$deb" | grep -q '\./usr/lib/systemd/system/piperd.service$' || fail "unit missing"
dpkg-deb -c "$deb" | grep -q '\./etc/piper/piperd.env$' || fail "env file missing"

ctrl="$(mktemp -d)"
dpkg-deb -e "$deb" "$ctrl/DEBIAN"
grep -q '^/etc/piper/piperd.env$' "$ctrl/DEBIAN/conffiles" || fail "piperd.env is not a conffile"
grep -q 'systemctl enable --now piperd' "$ctrl/DEBIAN/postinst" || fail "postinst does not enable --now"
grep -q 'systemctl restart piperd' "$ctrl/DEBIAN/postinst" || fail "postinst does not restart on upgrade"
grep -q 'systemctl disable --now piperd' "$ctrl/DEBIAN/prerm" || fail "prerm does not disable --now"
rm -rf "$ctrl"

set -- "$dist"/piper_*_amd64.deb; cli="$1"
dpkg-deb -c "$cli" | grep -q '\./usr/bin/piper$' || fail "piper binary not in /usr/bin"
if dpkg-deb -c "$cli" | grep -q 'systemd'; then fail "piper (CLI) deb must not ship a unit"; fi

echo "verify_deb: ok"
```

`chmod +x test/packaging/verify_deb.sh`

- [ ] **Step 2: Run to verify failure**

```bash
goreleaser release --snapshot --clean
./test/packaging/verify_deb.sh
```

Expected: FAIL with `missing piperd amd64 deb` (no `nfpms:` yet). If `dpkg-deb` is absent locally, `brew install dpkg` first — the skip path must not mask this step.

- [ ] **Step 3: Add the nfpm config**

In `.goreleaser.yaml`, after the `archives:` block:

```yaml
nfpms:
  - id: piperd
    ids: [piperd]
    package_name: piperd
    # ConventionalFileName is what maps GOARM=7 → armhf and drops the _linux_
    # infix; the default template would name this piperd_<ver>_linux_armv7.deb
    # and silently break the apt pipeline's filename parsing.
    file_name_template: "{{ .ConventionalFileName }}"
    formats: [deb]
    bindir: /usr/bin
    vendor: piperbox
    homepage: https://github.com/piperbox/piper
    maintainer: piperbox maintainers <noreply@piperbox.dev>
    description: Piper agent — git push to a live HTTPS URL on your own box.
    license: Apache-2.0
    section: net
    contents:
      - src: packaging/deb/piperd.service
        dst: /usr/lib/systemd/system/piperd.service
      - src: packaging/systemd/piperd.env.example
        dst: /etc/piper/piperd.env
        type: config
        file_info:
          mode: 0600
    scripts:
      postinstall: packaging/deb/postinstall.sh
      preremove: packaging/deb/preremove.sh
  - id: piper
    ids: [piper]
    package_name: piper
    file_name_template: "{{ .ConventionalFileName }}"
    formats: [deb]
    bindir: /usr/bin
    vendor: piperbox
    homepage: https://github.com/piperbox/piper
    maintainer: piperbox maintainers <noreply@piperbox.dev>
    description: Piper CLI — drive piperd locally, over the LAN, or through the relay.
    license: Apache-2.0
    section: net
```

And extend `release.extra_files` (the diet/manual path fetches the unit from the release, per spec):

```yaml
release:
  prerelease: auto
  extra_files:
    - glob: packaging/systemd/piper-relay.service
    - glob: packaging/systemd/piperd.service
    - glob: install.sh
```

- [ ] **Step 4: Run to verify pass**

```bash
goreleaser check
goreleaser release --snapshot --clean
./test/packaging/verify_deb.sh
```

Expected: `verify_deb: ok`.

- [ ] **Step 5: Wire CI**

In `.github/workflows/ci.yml`, add a `packaging` filter to the existing `dorny/paths-filter` step:

```yaml
            packaging:
              - '.goreleaser.yaml'
              - 'packaging/**'
              - 'test/packaging/**'
              - '.github/workflows/ci.yml'
```

and two steps after the existing `goreleaser check` step (dpkg-deb is present on ubuntu-latest):

```yaml
      - name: Build snapshot packages
        if: steps.changes.outputs.packaging == 'true'
        uses: goreleaser/goreleaser-action@v6
        with:
          distribution: goreleaser
          version: "~> v2"
          args: release --snapshot --clean

      - name: Verify deb contents
        if: steps.changes.outputs.packaging == 'true'
        run: ./test/packaging/verify_deb.sh
```

Note: the snapshot step needs Go — it must sit inside the `code`-gated region where `setup-go` already ran, or add `if: steps.changes.outputs.packaging == 'true'` to the `setup-go` step's condition (`if: steps.changes.outputs.code == 'true' || steps.changes.outputs.packaging == 'true'`). Do the latter.

- [ ] **Step 6: `make verify`, push the branch, confirm the CI job runs the new steps green, then commit is already pushed — commit message:**

```bash
git add .goreleaser.yaml test/packaging/verify_deb.sh .github/workflows/ci.yml
git commit -m "feat: build piperd + piper debs via nfpm, verified in CI

Part of #<deb-apt issue>

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
git push -u origin HEAD
```

---

### Task 3: piperbox/apt repo — mkrepo/mirror scripts + publish workflow + container test

This task builds the apt-repo publisher in a **local scratch clone** (the repo is created and pushed in Task 4). Work in `~/scratch/piperbox-apt` (any empty dir; `git init -b main` — Task 4 pushes `main`).

**Files (in the piperbox/apt repo):**
- Create: `scripts/mkrepo.sh`, `scripts/mirror-pool.sh`, `.github/workflows/publish.yml`, `README.md`, `test/test-mkrepo.sh`, `site-static/piperbox.sources`

**Interfaces:**
- Consumes: release assets `*_<arch>.deb` from `piperbox/piper` releases (Task 2 naming); env `APT_SIGNING_KEY` (Actions secret, armored private key, Task 4); dispatch `event_type: publish` with `client_payload.tag` (Task 5).
- Produces: the live site at `https://apt.piperbox.dev` — layout `pool/*.deb`, `dists/stable/main/binary-{amd64,arm64,armhf}/Packages{,.gz}`, `dists/stable/{Release,Release.gpg,InRelease}`, `piperbox.gpg` (binary keyring), `piperbox.asc` (armored), `piperbox.sources` (copy-pasteable deb822 stanza). Plan 4's installer consumes exactly these paths.

- [ ] **Step 1: Write the failing container test**

`test/test-mkrepo.sh` — the whole build-sign-install cycle against fixture debs, inside Docker (no host deps beyond Docker):

```sh
#!/bin/sh
# End-to-end test of mkrepo.sh: builds fixture debs (two versions, to prove
# pruning), a throwaway signing key, runs mkrepo.sh, then does a real
# `apt install` from the generated tree via a file:// source.
set -eu
cd "$(dirname "$0")/.."
exec docker run --rm -v "$PWD":/repo -w /repo debian:stable sh -eu -c '
	apt-get update -qq && apt-get install -y -qq apt-utils dpkg-dev gnupg >/dev/null

	# fixture debs: fake "piperd" 0.0.1 and 0.0.2. Arch = the container's own
	# (amd64 or arm64 depending on the host), so the final apt install works
	# on any dev machine; mkrepo.sh emits Packages for all arches regardless.
	arch=$(dpkg --print-architecture)
	mkdir -p /tmp/pool
	for v in 0.0.1 0.0.2; do
		d=/tmp/fake-$v
		mkdir -p $d/DEBIAN $d/usr/bin
		printf "Package: piperd\nVersion: $v\nArchitecture: $arch\nMaintainer: t <t@t>\nDescription: fixture\n" > $d/DEBIAN/control
		printf "#!/bin/sh\necho piperd $v\n" > $d/usr/bin/piperd && chmod +x $d/usr/bin/piperd
		dpkg-deb --build --root-owner-group $d /tmp/pool/piperd_${v}_${arch}.deb >/dev/null
	done

	# throwaway signing key
	gpg --batch --quiet --pinentry-mode loopback --passphrase "" \
		--quick-gen-key "test <t@t>" rsa2048 sign 0
	export GPG_KEY_ID=$(gpg --list-keys --with-colons | awk -F: "/^fpr/{print \$10; exit}")

	KEEP=1 sh scripts/mkrepo.sh /tmp/pool /tmp/site

	# pruning: only 0.0.2 survives
	[ ! -e /tmp/site/pool/piperd_0.0.1_${arch}.deb ] || { echo "FAIL: prune kept 0.0.1"; exit 1; }
	[ -e /tmp/site/pool/piperd_0.0.2_${arch}.deb ] || { echo "FAIL: 0.0.2 missing"; exit 1; }

	# signature verifies against the exported keyring
	gpg --no-default-keyring --keyring /tmp/site/piperbox.gpg --verify /tmp/site/dists/stable/InRelease 2>/dev/null \
		|| { echo "FAIL: InRelease signature"; exit 1; }

	# a real apt transaction against the tree
	install -m 0644 /tmp/site/piperbox.gpg /etc/apt/keyrings/test.gpg 2>/dev/null || {
		mkdir -p /etc/apt/keyrings && cp /tmp/site/piperbox.gpg /etc/apt/keyrings/test.gpg; }
	printf "Types: deb\nURIs: file:///tmp/site\nSuites: stable\nComponents: main\nSigned-By: /etc/apt/keyrings/test.gpg\n" \
		> /etc/apt/sources.list.d/test.sources
	apt-get update -qq
	apt-get install -y -qq piperd >/dev/null
	[ "$(piperd)" = "piperd 0.0.2" ] || { echo "FAIL: wrong piperd installed"; exit 1; }
	echo OK
'
```

`chmod +x test/test-mkrepo.sh`

- [ ] **Step 2: Run to verify failure**

Run: `./test/test-mkrepo.sh`
Expected: FAIL — `sh: scripts/mkrepo.sh: No such file or directory`.

- [ ] **Step 3: Write mkrepo.sh**

`scripts/mkrepo.sh`:

```sh
#!/bin/sh
# mkrepo.sh POOL SITE — build the signed apt tree from a directory of .debs.
# Pure: everything under SITE is regenerated from POOL each run.
# Env: GPG_KEY_ID (required) — signing key in the default keyring;
#      KEEP (default 3) — newest versions retained per package/arch.
set -eu
pool="$1"; site="$2"
keep="${KEEP:-3}"
: "${GPG_KEY_ID:?set GPG_KEY_ID}"

# Prune: filenames are <package>_<version>_<arch>.deb; sort -V per group.
for group in $(ls "$pool" | sed -E 's/^(.+)_[^_]+_([^_]+)\.deb$/\1_\2/' | sort -u); do
	pkg="${group%_*}"; arch="${group##*_}"
	ls "$pool/${pkg}"_*_"${arch}.deb" | sort -V | head -n -"$keep" 2>/dev/null | while read -r old; do
		rm -f "$old"
	done
done

rm -rf "$site"
mkdir -p "$site/pool"
cp "$pool"/*.deb "$site/pool/"

cd "$site"
for arch in amd64 arm64 armhf; do
	dir="dists/stable/main/binary-$arch"
	mkdir -p "$dir"
	# --multiversion: list every retained version, not just the newest —
	# mirror-pool.sh reconstructs the pool from Filename: entries, so an
	# unlisted deb would silently vanish on the next publish cycle.
	dpkg-scanpackages --multiversion --arch "$arch" pool > "$dir/Packages"
	gzip -9kf "$dir/Packages"
done

apt-ftparchive \
	-o APT::FTPArchive::Release::Origin=piperbox \
	-o APT::FTPArchive::Release::Label=piper \
	-o APT::FTPArchive::Release::Suite=stable \
	-o APT::FTPArchive::Release::Codename=stable \
	-o APT::FTPArchive::Release::Components=main \
	-o "APT::FTPArchive::Release::Architectures=amd64 arm64 armhf" \
	release dists/stable > dists/stable/Release

gpg --batch --yes --pinentry-mode loopback -u "$GPG_KEY_ID" \
	--clearsign -o dists/stable/InRelease dists/stable/Release
gpg --batch --yes --pinentry-mode loopback -u "$GPG_KEY_ID" \
	-abs -o dists/stable/Release.gpg dists/stable/Release
gpg --export "$GPG_KEY_ID" > piperbox.gpg
gpg --export --armor "$GPG_KEY_ID" > piperbox.asc
echo "mkrepo: $(ls pool | wc -l) debs, signed by $GPG_KEY_ID"
```

Note `head -n -N` (drop all but the last N) is GNU coreutils — fine, this only runs on debian/ubuntu.

- [ ] **Step 4: Run test to verify pass**

Run: `./test/test-mkrepo.sh`
Expected: `OK`.

- [ ] **Step 5: Write mirror-pool.sh + its test**

`scripts/mirror-pool.sh`:

```sh
#!/bin/sh
# mirror-pool.sh BASE_URL POOL — download the live repo's pool so a publish
# run starts from current state (the published site is the only durable copy;
# Actions has no persistent state). A 404 on Packages means first publish —
# succeed with an empty pool.
set -eu
base="$1"; pool="$2"
mkdir -p "$pool"
for arch in amd64 arm64 armhf; do
	packages="$(curl -fsSL "$base/dists/stable/main/binary-$arch/Packages" 2>/dev/null)" || continue
	echo "$packages" | awk '/^Filename: /{print $2}' | while read -r f; do
		[ -e "$pool/$(basename "$f")" ] || curl -fsSL "$base/$f" -o "$pool/$(basename "$f")"
	done
done
echo "mirror-pool: $(ls "$pool" 2>/dev/null | wc -l) debs"
```

Append to `test/test-mkrepo.sh`, inside the container script after the `echo OK` line is moved down (mirror test reuses the site just built):

```sh
	# mirror-pool round-trip: serve the site, mirror it, byte-compare the pool
	apt-get install -y -qq curl python3 >/dev/null
	(cd /tmp/site && python3 -m http.server 8099 >/dev/null 2>&1 &)
	sleep 1
	sh scripts/mirror-pool.sh http://127.0.0.1:8099 /tmp/mirrored
	cmp /tmp/site/pool/piperd_0.0.2_${arch}.deb /tmp/mirrored/piperd_0.0.2_${arch}.deb \
		|| { echo "FAIL: mirror differs"; exit 1; }
	# first-publish: empty remote must succeed with empty pool
	sh scripts/mirror-pool.sh http://127.0.0.1:8099/nonexistent /tmp/empty
	[ -z "$(ls /tmp/empty 2>/dev/null)" ] || { echo "FAIL: empty mirror not empty"; exit 1; }
	echo OK
```

Run: `./test/test-mkrepo.sh` → `OK`.

- [ ] **Step 6: Write the publish workflow + static assets**

`site-static/piperbox.sources` (copied into the site by the workflow; also the README's copy-paste block):

```
Types: deb
URIs: https://apt.piperbox.dev
Suites: stable
Components: main
Signed-By: /etc/apt/keyrings/piperbox.gpg
```

`.github/workflows/publish.yml`:

```yaml
name: Publish
on:
  repository_dispatch:
    types: [publish]
  workflow_dispatch:
    inputs:
      tag:
        description: "piper release tag (vX.Y.Z, stable only)"
        required: true

permissions:
  contents: read
  pages: write
  id-token: write

concurrency:
  group: publish
  cancel-in-progress: false

jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v5
      - name: Resolve tag
        run: |
          tag="${{ github.event.inputs.tag || github.event.client_payload.tag }}"
          case "$tag" in
            *-*) echo "refusing RC tag $tag — apt repo is stable-only"; exit 1 ;;
            v*) echo "TAG=$tag" >> "$GITHUB_ENV" ;;
            *) echo "not a tag: $tag"; exit 1 ;;
          esac
      - name: Import signing key
        env:
          APT_SIGNING_KEY: ${{ secrets.APT_SIGNING_KEY }}
        run: |
          printf '%s' "$APT_SIGNING_KEY" | gpg --batch --import
          echo "GPG_KEY_ID=$(gpg --list-secret-keys --with-colons | awk -F: '/^fpr/{print $10; exit}')" >> "$GITHUB_ENV"
      - name: Mirror current pool
        run: sh scripts/mirror-pool.sh https://apt.piperbox.dev pool
      - name: Add release debs
        env:
          GH_TOKEN: ${{ github.token }}
        # --clobber: a same-tag republish (the recovery case workflow_dispatch
        # exists for) re-downloads files the mirror step already fetched.
        run: gh release download "$TAG" -R piperbox/piper -p '*.deb' -D pool --clobber
      - name: Install repo tooling
        run: sudo apt-get update -qq && sudo apt-get install -y -qq apt-utils dpkg-dev
      - name: Build + sign repo
        run: |
          sh scripts/mkrepo.sh pool site
          cp site-static/piperbox.sources site/
      - name: Gate — apt install from the built tree before it can go live
        # A broken metadata/signing state must never become the live repo:
        # a real apt transaction against site/ via file://, maintainer scripts
        # included (they no-op outside systemd by design).
        run: |
          docker run --rm -v "$PWD/site":/site:ro debian:stable sh -eu -c '
            mkdir -p /etc/apt/keyrings
            cp /site/piperbox.gpg /etc/apt/keyrings/piperbox.gpg
            printf "Types: deb\nURIs: file:///site\nSuites: stable\nComponents: main\nSigned-By: /etc/apt/keyrings/piperbox.gpg\n" \
              > /etc/apt/sources.list.d/piperbox.sources
            apt-get update -qq
            apt-get install -y -qq piperd piper >/dev/null
            piperd --version && piper --version'
      - uses: actions/upload-pages-artifact@v5
        with:
          path: site

  deploy:
    needs: build
    runs-on: ubuntu-latest
    environment:
      name: github-pages
      url: ${{ steps.deployment.outputs.page_url }}
    steps:
      - id: deployment
        uses: actions/deploy-pages@v5

  verify:
    needs: deploy
    runs-on: ubuntu-latest
    container: debian:stable
    steps:
      - name: apt install from the live repo
        run: |
          set -eu
          tag="${{ github.event.inputs.tag || github.event.client_payload.tag }}"
          apt-get update -qq && apt-get install -y -qq curl ca-certificates >/dev/null
          mkdir -p /etc/apt/keyrings
          # Pages/CDN propagation can lag the deploy — and the likeliest stale
          # state is the PREVIOUS repo still serving fine, so the version
          # assertion must sit inside the retry, not after it.
          for i in $(seq 1 10); do
            if curl -fsSL https://apt.piperbox.dev/piperbox.gpg -o /etc/apt/keyrings/piperbox.gpg \
              && curl -fsSL https://apt.piperbox.dev/piperbox.sources -o /etc/apt/sources.list.d/piperbox.sources \
              && apt-get update -qq \
              && apt-get install -y -qq --reinstall piperd piper \
              && piperd --version | grep -q "${tag#v}"; then
              piperd --version && piper --version
              exit 0
            fi
            sleep 30
          done
          echo "verify failed: apt.piperbox.dev did not serve $tag within 10 tries"
          exit 1
```

`README.md` for the repo:

```markdown
# piperbox/apt

The Piper apt repository, served at **https://apt.piperbox.dev** via GitHub
Pages. Published automatically by
[publish.yml](.github/workflows/publish.yml) on every stable
[piperbox/piper](https://github.com/piperbox/piper) release
(`repository_dispatch` from its release workflow).

## Use it

    sudo install -d -m 0755 /etc/apt/keyrings
    sudo curl -fsSL https://apt.piperbox.dev/piperbox.gpg -o /etc/apt/keyrings/piperbox.gpg
    sudo curl -fsSL https://apt.piperbox.dev/piperbox.sources -o /etc/apt/sources.list.d/piperbox.sources
    sudo apt update && sudo apt install piperd piper

## How it works

`scripts/mirror-pool.sh` pulls the live pool (the published site is the only
durable state), the workflow adds the new release's debs, `scripts/mkrepo.sh`
prunes to the newest 3 versions per package/arch, regenerates and signs the
metadata (no-expiry RSA-4096 key held only in this repo's Actions secrets),
and deploy-pages ships the site. A verify job then does a real `apt install`
from the live URL in a Debian container. Test locally: `./test/test-mkrepo.sh`
(needs Docker).
```

- [ ] **Step 7: Full local run + commit (local scratch repo)**

Run: `./test/test-mkrepo.sh` → `OK`.

```bash
git add -A
git commit -m "feat: apt repo publisher — mkrepo/mirror scripts, publish workflow, container test

Part of piperbox/piper#<deb-apt issue>

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 4: Infrastructure setup (repo, Pages, key, DNS, secrets)

**Files:** none in this repo (GitHub/DNS state). Steps marked **[USER]** need Faruk (web UI or DNS); everything else runs from the terminal.

**Interfaces:**
- Consumes: the scratch repo from Task 3.
- Produces: live `piperbox/apt` with Pages-on-workflow + custom domain; secrets `APT_SIGNING_KEY` (in piperbox/apt) and `APT_REPO_TOKEN` (in piperbox/piper) that Tasks 3/5's workflows reference.

- [ ] **Step 1: Create and push the repo**

```bash
gh repo create piperbox/apt --public --description "Piper apt repository — https://apt.piperbox.dev" --disable-wiki
cd ~/scratch/piperbox-apt && git remote add origin git@github.com:piperbox/apt.git && git push -u origin main
```

- [ ] **Step 2: Enable Pages (workflow build mode)**

```bash
gh api -X POST repos/piperbox/apt/pages -f build_type=workflow
```

(409 means Pages already exists — then `gh api -X PUT repos/piperbox/apt/pages -f build_type=workflow`.)

- [ ] **Step 3: [USER] Generate + store the signing key** — run these yourself; the private key should exist only in the Actions secret and wherever you back it up:

```bash
gpg --batch --pinentry-mode loopback --passphrase '' --quick-gen-key "Piper apt repository <apt@piperbox.dev>" rsa4096 sign 0
```

(Passphrase-less on purpose: CI signs unattended, and the key exists only in
the Actions secret + your offline backup — a passphrase would just be a second
secret to rotate in lockstep.)

```bash
gpg --list-secret-keys --keyid-format long apt@piperbox.dev
```

```bash
gpg --export-secret-keys --armor apt@piperbox.dev | gh secret set APT_SIGNING_KEY -R piperbox/apt
```

Back the private key up somewhere offline (`gpg --export-secret-keys --armor apt@piperbox.dev > backup.asc` onto storage you trust), because losing it means re-keying every user. The key has **no expiry** by design (`0` above) — expiry is the #1 real-world apt-repo breaker.

- [ ] **Step 4: [USER] Create the dispatch PAT** — fine-grained PAT, repo access: `piperbox/apt` only, permission: Contents read/write (that's what `POST /dispatches` needs). The spec's single PAT also covers `piperbox/homebrew-tap`, but that repo doesn't exist yet — plan 2 *extends this PAT's repo access* (an edit, not a new secret) when it creates the tap. Create at https://github.com/settings/personal-access-tokens/new, then:

```bash
gh secret set APT_REPO_TOKEN -R piperbox/piper
```

(paste the token at the prompt — it goes only into the Actions secret).

- [ ] **Step 5: [USER] DNS** — add `apt.piperbox.dev` **CNAME → `piperbox.github.io`**, DNS-only (grey cloud if Cloudflare — proxying breaks GitHub's cert provisioning).

- [ ] **Step 6: Set the custom domain and confirm HTTPS**

```bash
gh api -X PUT repos/piperbox/apt/pages -f cname=apt.piperbox.dev
```

Wait for `gh api repos/piperbox/apt/pages -q .https_enforced` to be `true` (GitHub provisions the cert; can take ~15 min), then `gh api -X PUT repos/piperbox/apt/pages -F https_enforced=true` if not already.

---

### Task 5: Release-workflow dispatch + first live publish

**Files:**
- Modify: `.github/workflows/release.yml`
- Modify: `PROGRESS.md`

**Interfaces:**
- Consumes: `APT_REPO_TOKEN` secret (Task 4); the `publish` dispatch contract (Task 3).
- Produces: every future stable tag auto-publishes to apt.piperbox.dev.

- [ ] **Step 1: Add the dispatch step**

Append to the `goreleaser` job's steps in `.github/workflows/release.yml`:

```yaml
      - name: Publish to apt repo
        # Stable tags only: RCs carry a -suffix and never reach apt.
        if: ${{ !contains(github.ref_name, '-') }}
        env:
          GH_TOKEN: ${{ secrets.APT_REPO_TOKEN }}
        run: |
          gh api repos/piperbox/apt/dispatches \
            -f event_type=publish \
            -f 'client_payload[tag]=${{ github.ref_name }}'
```

- [ ] **Step 2: Update PROGRESS.md** — add one line in the appropriate section, e.g. `- apt repo: piperd+piper debs published to apt.piperbox.dev on stable tags, self-verified [#<deb-apt issue>]`.

- [ ] **Step 3: `make verify`, commit, open the PR**

```bash
git add .github/workflows/release.yml PROGRESS.md
git commit -m "feat: dispatch apt-repo publish on stable release tags

Part of #<deb-apt issue>

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
git push
gh pr create --base main --title "deb packages + self-hosted apt repo at apt.piperbox.dev" \
  --body "Deliverable 1 of the onboarding & packaging spec (docs/superpowers/specs/2026-07-30-onboarding-packaging-design.md): nfpm debs with service-owning maintainer scripts, CI-verified; piperbox/apt Pages publisher, container-tested. First live publish rides the next stable release. Closes #<deb-apt issue>.

🤖 Generated with [Claude Code](https://claude.com/claude-code)"
```

- [ ] **Step 4: First live publish — after the PR merges, cut the next stable release**

The **existing** releases carry no `.deb` assets — the nfpm config lands with this PR — so the first publish cannot target an old tag; it rides the next release. After squash-merge, cut a stable release per the `release` skill (a minor bump: the release now ships debs). The tag flows tag → goreleaser (debs on the Release) → dispatch → piperbox/apt publish. Watch it:

```bash
gh run watch -R piperbox/apt
```

Expected: build, deploy, and `verify` jobs all green — `verify` is a real `apt install piperd piper` from apt.piperbox.dev in a Debian container. If `verify` times out on DNS/cert propagation (first publish only), re-run the job once before debugging.

- [ ] **Step 5: Local end-to-end smoke (the user's own eyes)**

```bash
docker run --rm debian:stable sh -c 'apt-get update -qq && apt-get install -y -qq curl ca-certificates >/dev/null && mkdir -p /etc/apt/keyrings && curl -fsSL https://apt.piperbox.dev/piperbox.gpg -o /etc/apt/keyrings/piperbox.gpg && curl -fsSL https://apt.piperbox.dev/piperbox.sources -o /etc/apt/sources.list.d/piperbox.sources && apt-get update -qq && apt-get install -y piperd piper && piper --version && piperd --version'
```

Expected: both versions print the just-cut release.

- [ ] **Step 6: Make the apt smoke a standing release step** — add to `.claude/skills/release/SKILL.md`, in its post-release verification section, a step running exactly the Step-5 `docker run` smoke plus "confirm the piperbox/apt `Publish` run is green" (`gh run list -R piperbox/apt -w Publish -L 1`). Commit it with the Task 5 commit (it's part of this deliverable's testing surface, not plan 4's — plan 4 adds only the brew smoke).
