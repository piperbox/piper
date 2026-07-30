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
