#!/bin/bash
# actualizar-soflink.sh — actualización DELIBERADA de soflink, con las dos
# comprobaciones que el autoupdate del binario no hace.
#
# Por qué existe (metahuman-dev, 2026-09-09, orden de David):
# el autoupdate integrado comparaba su versión con la última release y, si
# diferían, descargaba y se reiniciaba. Entre el 07-09 13:29 y las 20:11 se
# publicaron 14 releases con el MISMO artefacto (declara 202609071545), así que
# la comparación NUNCA convergía: 272 reinicios en .63, 90 en .51, ~4,8 GB
# descargados, 145 API keys rotadas y CERO líneas de error.
#
# Este script no puede entrar en ese bucle porque exige que el artefacto
# DECLARE la versión de su etiqueta antes de instalarlo. Si no lo hace, no
# instala y avisa: el bucle se convierte en un mensaje.
#
# Uso:  ./actualizar-soflink.sh          → comprueba e instala si procede
#       ./actualizar-soflink.sh --check  → solo informa, no toca nada
set -u
cd "$(dirname "$(readlink -f "$0")")"

BIN="soflink-x86_64.AppImage"
REPO="Custok/sofmat"
SOLO_CHECK=0
[ "${1:-}" = "--check" ] && SOLO_CHECK=1

ACTUAL="$(timeout 20 "./$BIN" version 2>/dev/null | awk '{print $NF}')"
[ -n "$ACTUAL" ] || { echo "❌ no se pudo leer la version actual de ./$BIN"; exit 1; }

REL="$(curl -s --max-time 20 "https://api.github.com/repos/$REPO/releases/latest" 2>/dev/null)"
TAG="$(printf '%s' "$REL" | python3 -c 'import sys,json; print(json.load(sys.stdin).get("tag_name",""))' 2>/dev/null)"
DIGEST="$(printf '%s' "$REL" | python3 -c '
import sys,json
d=json.load(sys.stdin)
for a in d.get("assets",[]):
    if a.get("name")=="soflink-x86_64.AppImage": print((a.get("digest") or "")[7:])
' 2>/dev/null)"
[ -n "$TAG" ] || { echo "❌ sin respuesta de la API de GitHub — no toco nada"; exit 1; }

ESPERADA="${TAG#v}"
echo "instalada: $ACTUAL   ·   ultima release: $TAG"
[ "$ACTUAL" = "$ESPERADA" ] && { echo "✅ ya estas en la ultima. Nada que hacer."; exit 0; }

TMP="./$BIN.new.$$"
trap 'rm -f "$TMP"' EXIT
echo "descargando $TAG …"
curl -sL --max-time 180 -o "$TMP" \
  "https://github.com/$REPO/releases/download/$TAG/soflink-x86_64.AppImage" || {
  echo "❌ descarga fallida — sigo con la version actual"; exit 1; }
chmod +x "$TMP"

GOT="$(sha256sum "$TMP" | cut -d' ' -f1)"
if [ -n "$DIGEST" ] && [ "$GOT" != "$DIGEST" ]; then
  echo "❌ el sha256 descargado NO coincide con el digest del release — NO instalo"
  echo "   esperado $DIGEST"; echo "   obtenido $GOT"; exit 1
fi
echo "✅ sha256 = digest oficial del release"

# LA comprobación que faltaba: ¿el artefacto declara la versión de su etiqueta?
DECL="$(timeout 20 "$TMP" version 2>/dev/null | awk '{print $NF}')"
if [ "$DECL" != "$ESPERADA" ]; then
  echo "❌ ARTEFACTO MAL ETIQUETADO: $TAG contiene un binario que declara $DECL"
  echo "   NO instalo. Instalarlo reproduciria el bucle del 07-09 (14 releases, mismo binario)."
  echo "   Avisa en el bus y que se republique el release recompilado."
  exit 2
fi
echo "✅ version declarada ($DECL) = etiqueta ($TAG)"

[ "$SOLO_CHECK" = "1" ] && { echo "(--check: no instalo)"; exit 0; }

# La copia de seguridad y la instalacion van ENCADENADAS: sin set -e, un cp que
# falle no aborta el script, y sin este && instalaria quedandome sin rollback.
# El respaldo se nombra por la version que CONTIENE ($ACTUAL), no por aquella a
# la que se iba a actualizar ($ESPERADA). Con el nombre viejo, "pre-202609092243"
# contenia 202609091815: en un rollback urgente eliges por nombre y restauras la
# version equivocada. La etiqueta debe decir lo que hay dentro.
RESPALDO="$BIN.v$ACTUAL-$(date +%Y%m%d-%H%M%S)"
cp -a "$BIN" "$RESPALDO" || { echo "❌ no pude guardar la copia de seguridad — NO instalo"; exit 1; }
# "no vacia" no basta: una copia truncada tampoco sirve de rollback (debian-dev, 09-09).
[ "$(sha256sum "$RESPALDO" | awk '{print $1}')" = "$(sha256sum "$BIN" | awk '{print $1}')" ] || {
  echo "❌ la copia de seguridad NO coincide en sha con el binario actual — NO instalo"; exit 1; }
mv -f "$TMP" "$BIN" || { echo "❌ fallo al instalar; la copia esta en $RESPALDO"; exit 1; }
# rename, no cp: sobrescribir el binario en uso da ETXTBSY
trap - EXIT
echo "✅ instalado $TAG. Reinicia el servicio:  sudo systemctl restart soflink.service"
