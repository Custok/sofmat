#!/bin/bash
# verificar-appimage.sh — comprueba que soflink-x86_64.AppImage es bit a bit un
# asset oficial de las releases de GitHub Custok/sofmat antes de arrancarlo.
#
# Extraído de levantar.sh (el operador del nodo main, 2026-09-07) para poder usarlo como
# ExecStartPre de la unidad systemd: el arranque manual y el gestionado por
# systemd comparten EXACTAMENTE la misma verificación de integridad.
#
# El autoupdate reescribe el binario en cada release (06-09: 1625 → 1750 → 1805
# → 1815 → 1930 → 2100 → 2130 → 2215 → 2345), así que un hash fijo se queda
# obsoleto cada vez: se comprueba contra las últimas 10 releases y el REF_SHA
# del fichero solo actúa de respaldo cuando no hay red.
# Sin coincidencia con ninguna release ni con REF_SHA → posible binario
# re-etiquetado: NO arrancar (salida 1).
#
# 2026-09-09 (el operador del nodo main, orden de David): se añade la comprobación
# ETIQUETA ↔ VERSIÓN DECLARADA. Motivo: entre el 07-09 13:29 y las 20:11 se
# publicaron CATORCE releases con el MISMO binario (sha d51d181d…), que declara
# `202609071545`. El hash era oficial en las catorce, así que esta verificación
# las daba por buenas. El efecto fue un bucle de autoupdate que no converge:
# 272 reinicios en .63 y 90 en .51, ~4,8 GB descargados, 145 API keys rotadas,
# CERO líneas de error. Se detectó comparando dos números que debían coincidir.
#
# Esta comprobación NO aborta el arranque a propósito: un artefacto mal
# etiquetado deja el servicio DEGRADADO, pero abortar dejaría a los tres nodos
# SIN COORDINADOR, que es peor. Grita en el log y sigue. El bucle se impide por
# la otra vía: `-no-update` en el ExecStart (actualización deliberada con
# ./actualizar-soflink.sh, que sí exige que etiqueta y versión coincidan).
set -u
cd "$(dirname "$(readlink -f "$0")")"

REF_FILE="${REF_FILE:-levantar.sh}"   # dónde vive el REF_SHA de respaldo (una sola copia); en nodos sin levantar.sh: REF_FILE=<fichero con REF_SHA="…">
ARCH="$(uname -m)"                    # x86_64 | aarch64: coincide con el nombre del asset
BIN="soflink-${ARCH}.AppImage"

[ -x "$BIN" ] || { echo "❌ no existe $BIN en $(pwd)"; exit 1; }
REF_SHA="$(grep -oE '^REF_SHA="[0-9a-f]{64}"' "$REF_FILE" | cut -d'"' -f2)"
GOT_SHA="$(sha256sum "$BIN" | cut -d' ' -f1)"
echo "sha256: $GOT_SHA"

# ── ANCLA LOCAL: lo que el propio updater dejo escrito al arrancar ───────────
# Desde v202609101226 el binario escribe installed{version,sha,at} al ARRANCAR,
# con el sha de SI MISMO. Es el unico que sabe la verdad sin red: ya verifico
# digest y version declarada al instalarse.
# Se mira ANTES que GitHub porque no depende de internet: tras un apagon el nodo
# arranca antes que el router, y sin esto el guard se quedaba sin ancla -> exit 1.
STATE="soflink-update-state.json"
INST_SHA=""
INST_VER=""
if [ -f "$STATE" ]; then
  # se extrae el bloque installed y de ahi sha y version, sin interpretes de por medio
  BLOQUE="$(tr -d '\n\r' < "$STATE" | grep -o '"installed"[^}]*}')"
  INST_SHA="$(printf '%s' "$BLOQUE" | grep -oE '"sha"[[:space:]]*:[[:space:]]*"[0-9a-f]{64}"' | grep -oE '[0-9a-f]{64}')"
  INST_VER="$(printf '%s' "$BLOQUE" | grep -oE '"version"[[:space:]]*:[[:space:]]*"[0-9]+"' | grep -oE '[0-9]+')"
fi
if [ -n "$INST_SHA" ] && [ "$GOT_SHA" = "$INST_SHA" ]; then
  echo "✅ hash = installed.sha que el updater escribio al arrancar (${INST_VER:-version desconocida}) — sin necesitar red"
  exit 0
fi

MATCH="$(curl -s --max-time 15 'https://api.github.com/repos/Custok/sofmat/releases?per_page=10' 2>/dev/null | python3 -c '
import sys, json
got = sys.argv[1]
try:
    rels = json.load(sys.stdin)
except Exception:
    sys.exit(0)
for r in rels:
    for a in r.get("assets", []):
        if a.get("name") == "soflink-" + sys.argv[2] + ".AppImage" and a.get("digest", "") == "sha256:" + got:
            print(r["tag_name"]); sys.exit(0)
' "$GOT_SHA" "$ARCH")"

if [ -n "$MATCH" ]; then
  echo "✅ hash = asset oficial del release $MATCH (GitHub)"

  # ¿La versión que el binario DECLARA coincide con la etiqueta del release?
  DECLARADA="$(timeout 20 "./$BIN" version 2>/dev/null | awk '{print $NF}')"
  ESPERADA="${MATCH#v}"
  if [ -z "$DECLARADA" ]; then
    echo "⚠️  no se pudo leer la version declarada por el binario — sigo (no bloqueo el arranque)"
  elif [ "$DECLARADA" != "$ESPERADA" ]; then
    echo "❌ ARTEFACTO MAL ETIQUETADO: el release dice $ESPERADA y el binario declara $DECLARADA"
    echo "   El hash ES oficial, asi que no es un binario manipulado: es un release publicado"
    echo "   con un artefacto que no se recompilo. Sintoma tipico: bucle de autoupdate que"
    echo "   nunca converge (ver cabecera). ARRANCO IGUALMENTE para no dejar la flota sin"
    echo "   coordinador, pero NO actualices con este artefacto y avisa en el bus."
  else
    echo "✅ version declarada ($DECLARADA) = etiqueta del release ($MATCH)"
  fi

  if [ "$GOT_SHA" != "$REF_SHA" ]; then
    sed -i "s|^REF_SHA=.*|REF_SHA=\"$GOT_SHA\"|" "$REF_FILE" && echo "   REF_SHA de respaldo actualizado a $MATCH"
  fi
  exit 0
fi
if [ "$GOT_SHA" = "$REF_SHA" ]; then
  echo "✅ hash = REF_SHA de respaldo (sin acceso a la API de GitHub)"
  exit 0
fi

# ── El hash no cuadra con NINGUNA release ni con REF_SHA ──────────────────────
# Decision de David (09-09-2026), igual en los tres nodos: no abortar y no
# arrancar el artefacto sospechoso, sino RESTAURAR LA COPIA DE SEGURIDAD
# verificada y arrancar con ella.
#
# Por que: las causas frecuentes de este caso son inocentes y se arreglan solas
# con el respaldo — descarga truncada, `mv` interrumpido, disco lleno, un
# binario compilado en local para una prueba. Solo la ultima causa posible es
# maliciosa, y ante ella restaurar tambien es lo correcto: NO se ejecuta el
# artefacto que no cuadra. Abortar dejaba el nodo en `failed` sin que nada
# avisara (StartLimitBurst=5 en 10 s: cinco intentos en medio segundo y fuera).
echo "❌ HASH NO CUADRA con ninguna release oficial de GitHub ni con REF_SHA ($REF_SHA)"
echo "   Busco una copia de seguridad verificable con la que arrancar…"

RESTAURADA=""
# NO enumerar esquemas de nombres: se enumera lo que hay en el disco. En .63
# conviven SEIS formas distintas (.v<ver>-<ts> · .pre-<ver>-<ts> · .<ver> ·
# .bak-<ver> · .bak-pre<ver>-<fecha> · .old, que lo deja el propio autoupdate).
# Un glob escrito desde lo que uno recuerda ve 5 de 11 y dice "no hay respaldos"
# teniendo seis al lado. Se excluye solo lo apartado por sospechoso.
# Es seguro ampliarlo porque el filtro de verdad es el sha + la etiqueta declarada,
# no el nombre: un fichero que no sea un binario oficial no pasa de la primera linea.
for CANDIDATO in $(ls -1t "$BIN".* 2>/dev/null | grep -v '\.sospechoso-'); do
  [ -f "$CANDIDATO" ] || continue
  C_SHA="$(sha256sum "$CANDIDATO" | awk '{print $1}')"
  C_TAG="$(curl -s --max-time 15 'https://api.github.com/repos/Custok/sofmat/releases?per_page=10' 2>/dev/null | python3 -c '
import sys, json
got = sys.argv[1]
try:
    rels = json.load(sys.stdin)
except Exception:
    sys.exit(0)
for r in rels:
    for a in r.get("assets", []):
        if a.get("name") == "soflink-" + sys.argv[2] + ".AppImage" and a.get("digest", "") == "sha256:" + got:
            print(r["tag_name"]); sys.exit(0)
' "$C_SHA" "$ARCH")"
  # No basta con que el sha del candidato sea OFICIAL: el artefacto del bucle del
  # 07-09 es oficial en CATORCE releases y declara 202609071545. Restaurarlo seria
  # meter de vuelta justo el binario que causo los 281 arranques. Asi que al
  # candidato se le exige lo mismo que al binario nuevo: que DECLARE su etiqueta.
  if [ -n "$C_TAG" ]; then
    C_DECL="$(timeout 20 "./$CANDIDATO" version 2>/dev/null | awk '{print $NF}')"   # ./ obligatorio: ls no lo pone y sin el, bash busca en PATH
    if [ "$C_DECL" != "${C_TAG#v}" ]; then
      echo "   · descartada $CANDIDATO (sha oficial de $C_TAG pero declara $C_DECL: mal etiquetada)"
      continue
    fi
  fi
  if [ -n "$C_TAG" ] || [ "$C_SHA" = "$REF_SHA" ]; then
    ORIGEN="${C_TAG:-REF_SHA}"
    # se guarda el sospechoso antes de sustituirlo: sin el no hay forma de saber que paso
    mv -f "$BIN" "$BIN.sospechoso-$(date +%Y%m%d-%H%M%S)" || {
      echo "   ❌ no pude apartar el binario sospechoso — NO arranco"; exit 1; }
    cp -a "$CANDIDATO" "$BIN" || {
      echo "   ❌ no pude restaurar $CANDIDATO — NO arranco"; exit 1; }
    chmod +x "$BIN"
    RESTAURADA="$CANDIDATO"
    echo "   ✅ RESTAURADA la copia $CANDIDATO (verificada contra $ORIGEN)"
    echo "   El binario que no cuadraba queda guardado como $BIN.sospechoso-* — NO lo borres:"
    echo "   es la unica prueba de que ocurrio. Avisa en el bus."
    break
  fi
  echo "   · descartada $CANDIDATO (su sha tampoco cuadra)"
done

if [ -z "$RESTAURADA" ]; then
  # REGLA DE LA FLOTA (10-09-2026, consensuada por los tres, ordenada por David):
  # cuando ningun respaldo cuadre, GRITA Y ARRANCA. Nunca `exit 1`.
  # Medido: `exit 1` + Restart=always + StartLimitBurst=5 deja la unidad en
  # `failed` tras cinco intentos en medio segundo, EN SILENCIO. Y la condicion
  # que lo dispara no es exotica: un APAGON — el nodo arranca antes que el
  # router, el guard no alcanza GitHub y ningun respaldo cuadra si REF_SHA esta
  # desfasado. .63 y .51 han perdido la luz los mismos trece dias desde junio.
  # Un nodo que arranca sin verificar es malo; uno que NO arranca es peor.
  echo "   ⚠️  NINGUNA copia cuadra y no hay ancla local: ARRANCO IGUALMENTE,"
  echo "      sin haber podido verificar este binario."
  echo "      Revisa el nodo: o el updater no escribio installed{}, o el binario"
  echo "      no es el que el updater instalo. NO actualices desde aqui sin mirarlo."
  exit 0
fi
exit 0
