#!/bin/sh
# release_all.sh <version> — builds EVERY asset a release ships, from the HOST.
#
# Runs on the host (Git Bash) because the two builds need different images:
# the Go cross-compile needs golang:1.22-alpine and the AppImage packaging needs
# a debian with wget/imagemagick. A single container cannot do both — an earlier
# version of this script tried and could not have run.
#
# Why it exists at all: build_all.sh does NOT build the AppImages. On 2026-09-07
# nine releases went out with the cross-compiled binaries fresh and the two
# AppImages left over from an earlier build, so the asset published under a tag
# reported an OLDER version than its own tag. The Linux nodes consume the
# AppImage: they saw an update, downloaded it, verified the hash CORRECTLY,
# restarted, and were the same version again. 113 starts, 272 service restarts
# and ~940 MB in 24 h, with the process exiting 0 every time. Nothing errored.
#
# So the check below asks each artifact what version it IS and refuses to
# continue when any disagrees with the tag. A file timestamp is not evidence of
# a build — that is exactly what was trusted, and it was true for four of six.
set -e
V="$1"
[ -n "$V" ] || { echo "uso: sh dist/release_all.sh <version>   (p.ej. $(date +%Y%m%d%H%M))"; exit 2; }
SRC="C:/sofmat-go"

echo "$V" > "$SRC/dist/VERSION.txt"

echo "=== 1/3  binarios (golang:1.22-alpine)"
MSYS_NO_PATHCONV=1 docker run --rm -v "$SRC:/src"   -v sofmat-gocache:/root/.cache/go-build -v sofmat-gomod:/go/pkg/mod   -w /src golang:1.22-alpine sh dist/build_all.sh "$V" | tail -2

echo "=== 2/3  AppImages (debian:stable-slim) — build_all.sh NO los hace"
MSYS_NO_PATHCONV=1 docker run --rm -v "$SRC/dist:/dist" -w /dist   debian:stable-slim bash /dist/build_appimage.sh | tail -3

echo "=== 3/3  VERIFICACION: cada artefacto debe DECIR $V"
MSYS_NO_PATHCONV=1 docker run --rm -v "$SRC/dist:/dist" debian:stable-slim sh -c "
fail=0
for b in soflink-linux-amd64 soflink-linux-arm64; do
  chmod +x /dist/\$b
  got=\$(/dist/\$b version 2>&1 | tail -1 | awk '{print \$NF}')
  [ \"\$got\" = \"$V\" ] && echo \"  ok   \$b -> \$got\" || { echo \"  FALLO \$b -> \$got (esperaba $V)\"; fail=1; }
done
chmod +x /dist/soflink-x86_64.AppImage
got=\$(APPIMAGE_EXTRACT_AND_RUN=1 /dist/soflink-x86_64.AppImage version 2>&1 | tail -1 | awk '{print \$NF}')
[ \"\$got\" = \"$V\" ] && echo \"  ok   soflink-x86_64.AppImage -> \$got\" || { echo \"  FALLO AppImage -> \$got (esperaba $V)\"; fail=1; }
[ \"\$fail\" = 0 ] || { echo 'NO PUBLICAR: hay artefactos que no declaran la version'; exit 1; }
echo 'TODOS-COHERENTES'
"
echo "listo: $V   (ahora .relver_pending + publish_release.ps1)"
