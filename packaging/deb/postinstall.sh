#!/bin/sh
# dpkg runs this as `postinst configure <old-version>`; $2 is empty on a fresh
# install. Outside systemd (containers, chroots) the files land but nothing is
# started, per Debian convention.
# Both systemctl calls below are `|| true`: if piperd can't actually start
# yet (e.g. Docker isn't installed on a fresh box, so SupplementaryGroups=docker
# fails exec), that must not fail the postinst and wedge dpkg/apt — the unit
# starts on next boot, or the user runs `piper agent up` once prerequisites exist.
set -eu
[ "$1" = configure ] || exit 0
[ -d /run/systemd/system ] || exit 0
systemctl daemon-reload
if [ -z "${2:-}" ]; then
	systemctl enable --now piperd || true
elif systemctl is-active --quiet piperd; then
	# Upgrade of a running service: restart so the new binary actually serves
	# (#375). A deliberately stopped piperd stays stopped.
	systemctl restart piperd || true
fi
