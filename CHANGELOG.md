# Changelog

Historial de versiones de soflink. Cada release publica 5 binarios (Windows / Linux x86_64+arm64 AppImage / macOS arm64+intel) con auto-update desde GitHub.

## v202608231328 (2026-08-23)
Gestion del llama-server en el HOST de cada nodo (eject/load en toda la flota):

- Eject/load enrutados al nodo del instance, no siempre al nodo de control fijo. El eject de un modelo cargado ahora se dirige al nodo que HOSPEDA el endpoint (resuelto por la IP del propio endpoint -> plano de control :1357), y ya no depende solo del registro local del coordinador (un decode lanzado por fuera tambien se puede expulsar). Cargar un preset se enruta al plano de control del nodo MAIN del preset, para poder (re)lanzarlo desde otro nodo.
- Eject seguro por PID propio: el eject deja de usar un patron generico (taskkill /IM llama-server.exe, pkill -f llama-server) que tumbaria un llama-server de produccion ajeno en el mismo host; ahora detiene SOLO los PID que lanzo este soflink (se registran al arrancar cada proceso).
- rpc_exe en el config: nueva ruta del ggml-rpc-server del host para la fase de fleet-load de las uniones multi-nodo (accesor + auto-descubrimiento junto al binario). El lanzamiento del worker RPC queda como TODO documentado: la union no se dispara a medias.
- Panel: el boton Cargar aparece para cualquier preset cuyo nodo main exponga plano de control, no solo el local; su etiqueta indica el nodo main destino.

## v202608230234 (2026-08-23)
Arreglo de red domestica (cortes incluso por cable):

- Barrido de descubrimiento LAN: el intervalo del barrido PERIODICO sube de 20s a 300s (5 min). Se mantiene UN barrido al arranque, asi que el auto-descubrimiento de nodos nuevos (la DGX, etc.) sigue funcionando; el barrido periodico solo hace falta para nodos nuevos, que es raro (toda la flota ya esta en el config explicito). A 20s, un barrido /24 en :1357 lanzaba ~253 SYN por nodo cada 20s y saturaba la tabla de conexiones (conntrack) del router domestico Orbi -> caidas de red.
- Concurrencia del barrido limitada de 256 a 16 conexiones simultaneas: el pico de sondeos sube de forma gradual en vez de abrir las 253 de golpe, un goteo que el router absorbe.

## v202608230158 (2026-08-23)
Reduccion drastica del churn de conexiones (arreglo de red domestica):

- Telemetria con HTTP keep-alive: los sondeos periodicos del panel (/gpu de cada nodo, /props, /v1/models y el fan-out de /api/version?local=1) ahora comparten un unico http.Transport con pool de conexiones (MaxIdleConnsPerHost=8, IdleConnTimeout=90s) en vez de abrir un socket nuevo por tick.
- Bodies drenados hasta EOF antes de Close, para que la conexion keep-alive vuelva de verdad al pool y se reutilice (sin drenar, se cerraba y generaba un TIME_WAIT por sondeo).
- Panel: intervalo de refresco de estado subido de 3s a 5s.

Resultado: el numero de conexiones TCP cortas y de TIME_WAIT generadas por la telemetria baja a ~0 por sondeo, aliviando la tabla de conexiones del router.

## v202608230043 (2026-08-23 00:43)
FIX CRITICO de red:

- Cortada la recursion de panelVersion: cada /api/version consultaba la version de TODOS los nodos, y cada nodo consultado hacia lo mismo -> explosion exponencial que saturaba la LAN (tumbo la red). Ahora el fan-out usa ?local=1 y una peticion con ?local=1 NO reenvia. Bucle roto.
- Retiradas las releases v202608222358 y v202608230003 (contenian ese bug). NO las useis.
- Incluye el display NET en Mb/s y el sensor Linux robusto de las versiones previas.


## v202608230003 (2026-08-23 00:03)
Mejoras:

- **Sensor NET de Linux robusto**: coge la interfaz con MAS trafico en /proc/net/dev (antes filtraba docker/br/veth y en hosts con mucho Docker se quedaba en 0). Arregla NET=0 en node-c/node-d.
- Boton de update: se llama **'actualizar'** y SOLO aparece si algun nodo esta por debajo de la ultima version (desaparece cuando toda la flota esta al dia).


## v202608222358 (2026-08-23 00:00)
Mejoras:

- **Sensor NET de Linux robusto**: coge la interfaz con MAS trafico en /proc/net/dev (antes filtraba docker/br/veth y en hosts con mucho Docker se quedaba en 0). Arregla NET=0 en node-c/node-d.
- Boton de update: se llama **'actualizar'** y SOLO aparece si algun nodo esta por debajo de la ultima version (desaparece cuando toda la flota esta al dia).


## v202608222341 (2026-08-22 23:41)
Mejoras:

- **Token de GitHub en config** (`github_token` en config.local.json): el auto-update va AUTENTICADO (5000 req/h) en vez del anonimo (60/h por IP compartida). Asi la flota no vuelve a perder el canal de update por rate-limit.
- Boton **'actualizar todos'**: un clic dispara el update en TODOS los nodos (fan-out desde el coordinador), no solo el local.
- **FIX de release**: los AppImage de Linux (x86_64/aarch64) ahora se REGENERAN en cada release. Antes se subian los de una tanda vieja, y los nodos Linux/AppImage se quedaban atascados en la version anterior aunque 'actualizaran'.
- (de v2318) Indicadores NET rx/tx por nodo, tarjeta de estado al lanzar modelos, fix de carga en subcarpeta.


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
