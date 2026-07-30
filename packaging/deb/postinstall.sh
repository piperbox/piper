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
