#!/bin/sh
# release_all.sh <version> — builds EVERY asset a release ships, in one pass.
#
# Why this exists: build_all.sh does NOT build the AppImages (build_appimage.sh
# does), and on 2026-09-07 nine releases went out with the cross-compiled
# binaries fresh and the two AppImages left over from an earlier build. The
# published asset therefore reported an OLDER version than its own tag.
#
# The Linux nodes consume the AppImage, so they compared their version against
# the latest release, saw an update, downloaded it, verified the hash CORRECTLY,
# rewrote themselves, restarted... and were still the same version. The loop
# never converged: 113 starts, 272 service restarts and ~940 MB downloaded in
# 24 h, with the process exiting 0 every time. Nothing failed. It just never
# achieved anything.
#
# So: one script builds all six, and the check below refuses to let a release
# out unless the artifacts SAY the version themselves. A file timestamp is not
# evidence — that is exactly what was trusted last time.
set -e
V="$1"
[ -n "$V" ] || { echo "uso: release_all.sh <version>"; exit 2; }

echo "$V" > /src/dist/VERSION.txt
sh /src/dist/build_all.sh "$V"
echo "--- ahora los AppImage, que build_all.sh NO construye ---"
bash /src/dist/build_appimage.sh

echo "=== VERIFICACION: cada artefacto debe DECIR $V ==="
fail=0
for b in soflink-linux-amd64 soflink-linux-arm64; do
  got=$(/src/dist/$b version 2>&1 | tail -1 | awk '{print $NF}')
  [ "$got" = "$V" ] && echo "  ok  $b -> $got" || { echo "  FALLO $b -> $got (esperaba $V)"; fail=1; }
done
chmod +x /src/dist/soflink-x86_64.AppImage
got=$(APPIMAGE_EXTRACT_AND_RUN=1 /src/dist/soflink-x86_64.AppImage version 2>&1 | tail -1 | awk '{print $NF}')
[ "$got" = "$V" ] && echo "  ok  soflink-x86_64.AppImage -> $got" || { echo "  FALLO AppImage -> $got (esperaba $V)"; fail=1; }

[ "$fail" = "0" ] || { echo "NO PUBLICAR: hay artefactos que no declaran $V"; exit 1; }
echo "TODOS-COHERENTES-$V"
