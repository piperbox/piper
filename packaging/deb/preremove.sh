#!/bin/sh
# dpkg runs this as `prerm remove|upgrade ...`; tear down only on real removal.
set -eu
[ "$1" = remove ] || exit 0
[ -d /run/systemd/system ] || exit 0
systemctl disable --now piperd || true
