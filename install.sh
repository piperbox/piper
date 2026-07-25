#!/bin/sh
# Piper installer — https://github.com/piperbox/piper
# Places the piper + piperd binaries; lifecycle belongs to `piper agent`.
set -eu

PIPER_REPO="${PIPER_REPO:-piperbox/piper}"
PIPER_BASE_URL="${PIPER_BASE_URL:-https://github.com}"
PIPER_API_URL="${PIPER_API_URL:-https://api.github.com}"
PIPER_VERSION="${PIPER_VERSION:-}"
PIPER_PREFIX="${PIPER_PREFIX:-}"
cli_only="${PIPER_CLI_ONLY:-}"
use_rc="${PIPER_RC:-}"

die() { echo "piper-install: $*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

while [ $# -gt 0 ]; do
	case "$1" in
		--cli-only) cli_only=1 ;;
		--rc) use_rc=1 ;;
		--version) shift; PIPER_VERSION="${1:-}" ;;
		--version=*) PIPER_VERSION="${1#--version=}" ;;
		-h|--help) echo "Usage: install.sh [--cli-only] [--rc] [--version vX.Y.Z]"; exit 0 ;;
		*) die "unknown option: $1" ;;
	esac
	shift
done

detect_os() {
	os="$(uname -s)"
	case "$os" in
		Linux) echo linux ;;
		Darwin) echo darwin ;;
		*) die "unsupported OS: $os" ;;
	esac
}

detect_arch() {
	arch="$(uname -m)"
	case "$arch" in
		x86_64|amd64) echo amd64 ;;
		aarch64|arm64) echo arm64 ;;
		armv7l|armv7) echo armv7 ;;
		*) die "unsupported architecture: $arch" ;;
	esac
}

fetch() { # fetch URL DEST
	if have curl; then curl -fsSL "$1" -o "$2"
	elif have wget; then wget -qO "$2" "$1"
	else die "need curl or wget"; fi
}

# fetch_archive is fetch with a progress meter, for the only downloads big
# enough to wait on (~18 MB per binary). Both tools write the meter to stderr,
# so it survives `curl … | sh`; it is shown only when stderr is a terminal, so
# CI logs stay clean. busybox wget has no --show-progress — the plain -q path
# is the fallback there.
fetch_archive() { # fetch_archive URL DEST
	if [ ! -t 2 ]; then fetch "$1" "$2"; return; fi
	if have curl; then curl -fL --progress-bar "$1" -o "$2"
	elif have wget && wget --help 2>&1 | grep -q -- --show-progress; then
		wget -q --show-progress -O "$2" "$1"
	else fetch "$1" "$2"; fi
}

fetch_stdout() { # fetch URL -> stdout
	if have curl; then curl -fsSL "$1"
	elif have wget; then wget -qO- "$1"
	else die "need curl or wget"; fi
}

sha256_of() { # sha256_of FILE -> hash
	if have sha256sum; then sha256sum "$1" | awk '{print $1}'
	elif have shasum; then shasum -a 256 "$1" | awk '{print $1}'
	else die "need sha256sum or shasum"; fi
}

# first_tag reads a GitHub releases JSON body on stdin and echoes the first
# tag_name. grep -o isolates each match (robust to pretty or compact JSON);
# head -n1 takes the newest (GitHub lists newest first).
first_tag() {
	grep -o '"tag_name": *"[^"]*"' | head -n1 | sed -E 's/.*"([^"]+)"$/\1/'
}

# resolve_version echoes the release tag to install.
resolve_version() {
	[ -n "$PIPER_VERSION" ] && { echo "$PIPER_VERSION"; return; }
	if [ -n "$use_rc" ]; then
		tag="$(fetch_stdout "$PIPER_API_URL/repos/$PIPER_REPO/releases" | first_tag)" || true
		[ -n "${tag:-}" ] || die "could not resolve latest pre-release from GitHub"
		echo "$tag"
	else
		tag="$(fetch_stdout "$PIPER_API_URL/repos/$PIPER_REPO/releases/latest" | first_tag)" || true
		[ -n "${tag:-}" ] || die "no stable release yet — re-run with --rc to install the latest pre-release"
		echo "$tag"
	fi
}

# download_verify NAME TAG OS ARCH DESTDIR
download_verify() {
	name="$1"; tag="$2"; os="$3"; arch="$4"; dest="$5"
	ver="${tag#v}"
	archive="${name}_${ver}_${os}_${arch}.tar.gz"
	tmp="$(mktemp -d)"
	trap 'rm -rf "$tmp"' EXIT
	echo "downloading $name $tag ($os/$arch)…" >&2
	fetch_archive "$PIPER_BASE_URL/$PIPER_REPO/releases/download/$tag/$archive" "$tmp/$archive" \
		|| die "download failed: $archive"
	fetch "$PIPER_BASE_URL/$PIPER_REPO/releases/download/$tag/checksums.txt" "$tmp/checksums.txt" \
		|| die "download failed: checksums.txt"
	want="$(grep " ${archive}\$" "$tmp/checksums.txt" | awk '{print $1}')"
	[ -n "$want" ] || die "no checksum for $archive"
	got="$(sha256_of "$tmp/$archive")"
	[ "$want" = "$got" ] || die "checksum mismatch for $archive (want $want got $got)"
	tar xzf "$tmp/$archive" -C "$tmp"
	install -m 0755 "$tmp/$name" "$dest/$name"
	rm -rf "$tmp"
	trap - EXIT
}

cli_prefix() {
	[ -n "$PIPER_PREFIX" ] && { echo "$PIPER_PREFIX"; return; }
	if [ "$(id -u)" -eq 0 ]; then echo /usr/local/bin; else echo "$HOME/.local/bin"; fi
}

os="$(detect_os)"
arch="$(detect_arch)"
tag="$(resolve_version)"
prefix="$(cli_prefix)"
mkdir -p "$prefix"
if [ -n "$cli_only" ]; then
	download_verify piper "$tag" "$os" "$arch" "$prefix"
	echo "installed piper $tag -> $prefix/piper"
else
	download_verify piperd "$tag" "$os" "$arch" "$prefix"
	download_verify piper "$tag" "$os" "$arch" "$prefix"
	echo "installed piper + piperd $tag -> $prefix"
	if [ "$os" = linux ]; then
		echo "next: piper agent up   (or: piper agent daemonize — durable system service on :80/:443)"
	else
		echo "next: see docs/manual-setup.md (Run the agent on macOS)"
	fi
fi
case ":$PATH:" in
	*":$prefix:"*) ;;
	*) echo "note: $prefix is not on your PATH — add it to use piper" ;;
esac

# A copy earlier on PATH wins over the one just written, so the install looks
# successful while the old binary keeps running — three upgrades in a row on a
# real box read as "the fix didn't work" because of this. Under sudo the check
# would use root's PATH and miss the invoking user's ~/.local/bin entirely, so
# that location is probed directly.
warn_shadow() {
	name="$1"
	found="$(command -v "$name" 2>/dev/null || true)"
	if [ -n "$found" ] && [ "$found" != "$prefix/$name" ]; then
		echo "warning: $found shadows $prefix/$name on your PATH — that older copy is what will run"
		echo "         remove it, or put $prefix first in PATH"
		return 0
	fi
	if [ -n "${SUDO_USER:-}" ]; then
		home="$(getent passwd "$SUDO_USER" 2>/dev/null | cut -d: -f6 || true)"
		other="$home/.local/bin/$name"
		if [ -n "$home" ] && [ "$other" != "$prefix/$name" ] && [ -x "$other" ]; then
			echo "warning: $other shadows $prefix/$name on $SUDO_USER's PATH — that older copy is what will run"
			echo "         remove it, or put $prefix first in PATH"
		fi
	fi
	return 0
}

warn_shadow piper
[ -n "$cli_only" ] || warn_shadow piperd
