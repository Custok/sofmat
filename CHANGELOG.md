# Changelog

Historial de versiones de soflink. Cada release publica 5 binarios (Windows / Linux x86_64+arm64 AppImage / macOS arm64+intel) con auto-update desde GitHub.

## v202608222334 (2026-08-22 23:34)
Mejoras:

- Boton 'actualizar flota': un solo clic dispara el 'actualizar ahora' en TODOS los nodos (el coordinador hace fan-out a cada soflink), no solo el local. Ademas muestra el resultado por nodo.
- (de v2318) Indicadores NET (rx/tx) por nodo, tarjeta de estado al lanzar modelos, y fix de carga de modelos en subcarpeta.

Nota operativa: la API publica de GitHub son 60 req/h por IP; si toda la flota comparte IP y se sondea en rafaga se agota y el auto-update deja de ver releases. En regimen normal (poll cada 30 min) queda muy por debajo.


## v202608222318 (2026-08-22 23:18)
Mejoras:

- Al LANZAR un modelo aparece una TARJETA con estado en vivo: Lanzando -> cargando (Ns) -> verde 'CARGADO y sirviendo' o rojo 'crasheo (sin VRAM / arch no soportada)'. Botones Cerrar y Relanzar SIEMPRE visibles (Cerrar ademas hace eject para no dejar el proceso huerfano).
- Indicadores de RED (NET down/up) por fin funcionan: el sensor de cada nodo publica rx/tx en Mbps (Windows: netstat -e; Linux: /proc/net/dev, ignorando lo/docker/veth). Antes el endpoint /gpu no publicaba red y el panel mostraba '-' o 0.00.
- Fix: lanzar un modelo que vive en su subcarpeta ya NO falla con 'modelo no esta descargado' (se preserva la subcarpeta, con anti-traversal).


## v202608222300 (2026-08-22 23:00)
Fix:

- Lanzar un modelo que vive en su subcarpeta (p.ej. Qwen3.8-27B-Q4_0/Qwen3.8-27B-Q4_0.gguf) ya NO falla con 'modelo no está descargado'. El chequeo de existencia y el flag -m ahora preservan la subcarpeta; antes filepath.Base la descartaba y buscaba la ruta plana. Afectaba a TODOS los modelos organizados en carpetas al lanzarlos en modo Individual/local. Se mantiene el anti-traversal (rechaza '..' y rutas de mas de 2 tramos).


## v202608222106 (2026-08-22)
Mejoras:

- Boton 'actualizar ahora' en el header cuando hay una version nueva: dispara el auto-update inmediato (swap + re-exec) en vez de esperar al check de 30 min.
- El header muestra el NUMERO de version (v20260822...) en vez de la fecha formateada.
- Al lanzar un modelo: si falla al cargar salen botones Cerrar y Relanzar; y el fallo se detecta RAPIDO (el daemon rastrea el proceso y sabe si murio) en vez de esperar el timeout de 180s.


## v202608222043 (2026-08-22)
Mejoras:

- Modelos por CARPETA: cada modelo descargado va a su propia subcarpeta (todas las partes de un split GGUF juntas). El panel lista UNA fila por modelo con el tamano total, y borrar elimina la carpeta entera (todas las partes). Asi un modelo de varios ficheros nunca aparece como varias filas. Sigue mostrando los .gguf sueltos antiguos por compatibilidad.


## v202608222028 (2026-08-22)
Mejoras:

- Header con VERSION: muestra la version en ejecucion (formateada como fecha/hora del build), la version DISPONIBLE en GitHub si hay una mas nueva, y un checkbox de auto-update que se puede activar/desactivar en vivo.
- Modelos partidos (split GGUF de varios ficheros, ...-00001-of-00002) aparecen como UNA sola fila (la parte cargable) con el tamano total, en vez de una fila por parte.


## v202608222012 (2026-08-22)
Mejoras (todo pensado para que el user solo ejecute el binario):

- UN SOLO BINARIO auto-contenido: el daemon soflink ahora lanza y para modelos el mismo (/control/load|eject|kill integrado, in-process en su propio host). Ya NO hace falta el node-agent aparte. Cross-platform (Windows/Linux/macOS).
- AUTO-CONFIGURACION de rutas: el exe descubre solo llama-server (junto al binario, subcarpetas comunes, PATH) y guarda/lee los modelos en una carpeta ABSOLUTA junto al binario, independiente de desde donde lo ejecutes. Cero config manual.


## v202608221852 (2026-08-22)
Mejoras desde la version anterior:

- Autoactualizador en RUNTIME: el binario re-comprueba GitHub cada 30 min mientras corre (antes solo al arranque), asi las guardias persistentes cogen releases nuevas sin reiniciar a mano. Al encontrar version nueva: swap del binario + re-exec.
- Lanzado con feedback real: tras arrancar el proceso, el panel sondea /health de la instancia y muestra 'cargando modelo... (Ns)' -> 'modelo CARGADO y sirviendo (Ns)' o 'no cargo en 180s'. Antes ponia 'lanzado' aunque el modelo fallara al cargar.


## v202608221833 (2026-08-22)
Mejoras desde la version anterior:

- Catalogo GGUF-only: el catalogo de descarga solo lista repos GGUF (filter=gguf); los repos NVFP4/AWQ/MLX ya no aparecen (su listado de cuantizaciones salia vacio).
- Descargas con velocidad + ETA: la seccion Descargas muestra en vivo MB/s y ETA ademas de % y tamano.
- Borrar modelos: nuevo boton en Modelos locales que elimina el .gguf del disco, con confirmacion y endurecido contra path-traversal (solo .gguf del models dir, requiere API key).
