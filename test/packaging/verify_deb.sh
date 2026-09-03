#!/bin/sh
# Asserts a goreleaser snapshot produced well-formed debs. Run after:
#   goreleaser release --snapshot --clean
# Skips cleanly when dpkg-deb is unavailable (macOS without `brew install dpkg`),
# matching the repo's Docker-dependent-test convention.
set -eu
command -v dpkg-deb >/dev/null 2>&1 || { echo "verify_deb: skip (no dpkg-deb)"; exit 0; }
dist="${1:-dist}"
fail() { echo "verify_deb: $*" >&2; exit 1; }
script_dir=$(CDPATH='' command cd -- "$(dirname -- "$0")" && pwd)
config="${GORELEASER_CONFIG:-$script_dir/../../.goreleaser.yaml}"
case "$config" in
	/*) ;;
	*) config="$PWD/$config" ;;
esac
[ -f "$config" ] || fail "missing GoReleaser config: $config"
repo_dir=$(CDPATH='' command cd -- "$script_dir/../.." && pwd)
# Render the configured templates with both Debian prerelease and semver build
# metadata; this catches a template that merely contains the expected text.
if ! (CDPATH='' command cd -- "$repo_dir" && go run ./test/packaging/verify_deb_templates.go "$config"); then
	fail "GoReleaser nFPM template behavior check failed"
fi

metadata="$dist/metadata.json"
[ -f "$metadata" ] || fail "missing GoReleaser metadata: $metadata"
metadata_version="$(sed -n 's/.*"version":"\([^"]*\)".*/\1/p' "$metadata")"
[ -n "$metadata_version" ] || fail "missing version in GoReleaser metadata: $metadata"
case "$metadata_version" in
	*-*) expected_version="${metadata_version%%-*}~${metadata_version#*-}" ;;
	*) expected_version="$metadata_version" ;;
esac
# nFPM maps a semver prerelease hyphen to Debian's '~' and preserves '+'.

for deb in "$dist"/*.deb; do
	[ -e "$deb" ] || continue
	case "${deb##*/}" in
		*~*) fail "deb filename contains '~': ${deb##*/}" ;;
		*+*) fail "deb filename contains '+': ${deb##*/}" ;;
	esac
done

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

for deb in "$dist"/piperd_*_amd64.deb "$dist"/piper_*_amd64.deb; do
	version="$(dpkg-deb -f "$deb" Version)"
	[ "$version" = "$expected_version" ] || fail "Debian Version mismatch in ${deb##*/}: got $version, want $expected_version"
done

echo "verify_deb: ok"
