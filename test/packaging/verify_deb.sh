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
[ -f "$config" ] || fail "missing GoReleaser config: $config"

for pkg in piperd piper; do
	template="$(awk -v wanted="$pkg" '
		/^nfpms:/ { in_nfpms=1; current=""; next }
		in_nfpms && /^[^[:space:]]/ { in_nfpms=0; current="" }
		in_nfpms && /^  - id: / { current=$3 }
		in_nfpms && current == wanted && /^[[:space:]]+file_name_template:/ {
			print
			exit
		}
	' "$config")"
	case "$template" in
		*'replace (replace .ConventionalFileName "~" ".") "+" "."'*) ;;
		*) fail "nfpms $pkg template does not sanitize '+': ${template:-<missing>}" ;;
	esac
done

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
	case "$version" in
		*~*) ;;
		*) fail "Debian Version lost prerelease separator in ${deb##*/}: $version" ;;
	esac
done

echo "verify_deb: ok"
