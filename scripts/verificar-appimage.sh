#!/bin/bash
# verificar-appimage.sh — comprueba que soflink-x86_64.AppImage es bit a bit un
# asset oficial de las releases de GitHub Custok/sofmat antes de arrancarlo.
#
# Extraído de levantar.sh (metahuman-dev, 2026-09-07) para poder usarlo como
# ExecStartPre de la unidad systemd: el arranque manual y el gestionado por
# systemd comparten EXACTAMENTE la misma verificación de integridad.
#
# El autoupdate reescribe el binario en cada release (06-09: 1625 → 1750 → 1805
# → 1815 → 1930 → 2100 → 2130 → 2215 → 2345), así que un hash fijo se queda
# obsoleto cada vez: se comprueba contra las últimas 10 releases y el REF_SHA
# del fichero solo actúa de respaldo cuando no hay red.
# Sin coincidencia con ninguna release ni con REF_SHA → posible binario
# re-etiquetado: NO arrancar (salida 1).
set -u
cd "$(dirname "$(readlink -f "$0")")"

REF_FILE="levantar.sh"          # dónde vive el REF_SHA de respaldo (una sola copia)
BIN="soflink-x86_64.AppImage"

[ -x "$BIN" ] || { echo "❌ no existe $BIN en $(pwd)"; exit 1; }
REF_SHA="$(grep -oE '^REF_SHA="[0-9a-f]{64}"' "$REF_FILE" | cut -d'"' -f2)"
GOT_SHA="$(sha256sum "$BIN" | cut -d' ' -f1)"
echo "sha256: $GOT_SHA"

MATCH="$(curl -s --max-time 15 'https://api.github.com/repos/Custok/sofmat/releases?per_page=10' 2>/dev/null | python3 -c '
import sys, json
got = sys.argv[1]
try:
    rels = json.load(sys.stdin)
except Exception:
    sys.exit(0)
for r in rels:
    for a in r.get("assets", []):
        if a.get("name") == "soflink-x86_64.AppImage" and a.get("digest", "") == "sha256:" + got:
            print(r["tag_name"]); sys.exit(0)
' "$GOT_SHA")"

if [ -n "$MATCH" ]; then
  echo "✅ hash = asset oficial del release $MATCH (GitHub)"
  if [ "$GOT_SHA" != "$REF_SHA" ]; then
    sed -i "s|^REF_SHA=.*|REF_SHA=\"$GOT_SHA\"|" "$REF_FILE" && echo "   REF_SHA de respaldo actualizado a $MATCH"
  fi
  exit 0
fi
if [ "$GOT_SHA" = "$REF_SHA" ]; then
  echo "✅ hash = REF_SHA de respaldo (sin acceso a la API de GitHub)"
  exit 0
fi
echo "❌ HASH NO CUADRA con ninguna release oficial de GitHub ni con REF_SHA ($REF_SHA) — NO se arranca."
exit 1
