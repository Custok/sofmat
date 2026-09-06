#!/bin/bash
set -e
apt-get update -qq >/dev/null 2>&1
apt-get install -y -qq wget imagemagick file ca-certificates >/dev/null 2>&1

convert -size 256x256 xc:'#12263a' -gravity center -pointsize 110 -fill '#7fd1ff' -annotate 0 'sl' /tmp/soflink.png

wget -q https://github.com/AppImage/appimagetool/releases/download/continuous/appimagetool-x86_64.AppImage -O /tmp/ait 2>/dev/null \
  || wget -q https://github.com/AppImage/AppImageKit/releases/download/continuous/appimagetool-x86_64.AppImage -O /tmp/ait
chmod +x /tmp/ait

make_appdir() {
  bin="$1"; dir="$2"
  rm -rf "$dir"; mkdir -p "$dir/usr/bin"
  cp "/dist/$bin" "$dir/usr/bin/soflink"; chmod +x "$dir/usr/bin/soflink"
  cp /tmp/soflink.png "$dir/soflink.png"
  cat > "$dir/soflink.desktop" <<'DESK'
[Desktop Entry]
Name=soflink
Exec=soflink
Icon=soflink
Type=Application
Categories=Utility;
Terminal=true
DESK
  cat > "$dir/AppRun" <<'RUN'
#!/bin/sh
HERE="$(dirname "$(readlink -f "$0")")"
exec "$HERE/usr/bin/soflink" "$@"
RUN
  chmod +x "$dir/AppRun"
}

make_appdir soflink-linux-amd64 /tmp/AD
ARCH=x86_64 /tmp/ait --appimage-extract-and-run /tmp/AD /dist/soflink-x86_64.AppImage 2>&1 | tail -2 || echo "amd64 fail"

if wget -q https://github.com/AppImage/type2-runtime/releases/download/continuous/runtime-aarch64 -O /tmp/rt64 2>/dev/null; then
  make_appdir soflink-linux-arm64 /tmp/AD2
  ARCH=aarch64 /tmp/ait --appimage-extract-and-run --runtime-file /tmp/rt64 /tmp/AD2 /dist/soflink-aarch64.AppImage 2>&1 | tail -2 || echo "arm64 fail"
fi

echo "=== AppImages ==="
ls -la /dist/*.AppImage 2>/dev/null
