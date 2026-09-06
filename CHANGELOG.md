# Changelog

Historial de versiones de soflink. Cada release publica 5 binarios (Windows / Linux x86_64+arm64 AppImage / macOS arm64+intel) con auto-update desde GitHub.

## v202609062345 (2026-09-06)
Handoff: comprobar los soflink de los dos nodos ANTES de gastar el prefill.

- Visto en produccion: el soflink del nodo prefill no volvio tras un reinicio del host (AppImage sin unidad); el gateway hizo 33 s de prefill + save y solo entonces fallo el `kv-fetch` (conexion rechazada) → 57 s en vez de 24 s directo. Ahora `Prefill` pide `/soflink/hello` (1,5 s) al soflink del prefill y al del decode antes de tocar el motor; si alguno no responde, error inmediato (`prefill_error: prefill/decode soflink unreachable`) + cortacircuitos, sin trabajo en GPU.

## v202609062215 (2026-09-06)
Gateway robusto con el prefill caido (nodo ejectado o host apagado).

- Visto al ejectar el prefill de un nodo por hardware: el gateway ya degradaba a decode directo (200, causa en el registro), pero cada prompt largo volvia a intentar el tokenize contra el nodo. Ahora un fallo en el lado prefill (tokenize, prefill, save, fetch, restore) abre un cortacircuitos de 30 s (`PrefillBreaker`; registro `admission: prefill-down`) y las llamadas al prefill/decode de control usan un transporte con dial acotado a 2 s: un host apagado cuesta 2 s una vez cada 30 s, no un timeout TCP por peticion.

## v202609062130 (2026-09-06)
Admision consciente de la cache + modo `auto` (nuevo por defecto).

- Visto con el HUD de David (agente que encadena vueltas de tools): con `always`, cada vuelta pasaba el prompt ENTERO por el prefill (34k → 18 s + traspaso) aunque el decode tenia 22k de ese prefijo calientes y lo habria continuado en ~1 s. Ahora el gateway guarda los ids del ultimo prompt por prefijo y cuenta como NUEVO solo lo que sigue al prefijo comun con la vuelta anterior (`new_tokens` en el registro; `cache-hot` cuando no llega al suelo de 8 192).
- `kv_handoff: auto` (por defecto): decode ocupado → traspaso (protege los streams); decode libre → modelo de coste con las tasas MEDIDAS por tamano (EMA): `directo = nuevos / pp_decode` frente a `prefill = total / pp_prefill + traspaso`. Con los datos de hoy: 46k frio → prefill (27 s vs 33 s); 11,5k frio → directo; vuelta de agente 34k con 22k calientes → directo. `busy` y `always` siguen disponibles. Registro: `admission: decode-cheaper` cuando el modelo elige directo.

## v202609062100 (2026-09-06)
Admision: el registro de prefijos calientes se alinea con la cache real del decode.

- Visto en la prueba de David desde el HUD: un prompt de 16 816 tokens se admitio como `small-new-prefill` (est. 3 688 nuevos) porque el registro del gateway creia caliente el system-prompt de una peticion anterior, pero el decode ya lo habia desalojado (`cache_n 0`) y lo proceso entero (7,4 s). Fix: (1) el registro tiene tantas entradas como slots tiene el decode (4), no 512: no puede haber mas prefijos calientes que slots; (2) si el decode responde con `cache_n` por debajo de la mitad del prefijo en una peticion directa, el gateway olvida ese prefijo (`prefix_cold: true` en el registro) y la siguiente peticion se admite por su tamano completo.

## v202609061930 (2026-09-06)
Handoff: transporte del sidecar del borrador MTP (parche llama.cpp incluido en docs/patches).

- Causa del tg a la mitad tras un restore, localizada en llama.cpp a3b1eff: `slots/save|restore` a fichero solo serializan el contexto del modelo principal; el borrador MTP (contexto `ctx_dft` aparte) arranca frio en otro proceso. Parche `docs/patches/llama-a3b1eff-slot-save-dft.patch` (server-context, speculative, server-task): el save escribe ademas `<fichero>.dft` (KV del borrador + `pending_h`) y el restore lo lee si existe; respuesta con `n_written_dft` / `n_read_dft`. Compilado en CPU sin errores; build CUDA sm120 por la flota.
- soflink: `POST /control/kv-fetch` del `<estado>.dft` best-effort tras el estado principal (registro `dft`, `dft_bytes`), borrado de ambos ficheros en los dos nodos, nombres `.bin.dft` admitidos en `/kv/*`. Con motores sin parche no cambia nada.

## v202609061815 (2026-09-06)
Handoff: restaurar siempre en un slot LIBRE del decode.

- Medido con un usuario en streaming: un `restore` dirigido al slot que estaba generando se encola detras de ese stream (31,5 s en vez de 0,2 s para un estado de 32k). Ahora el driver consulta `GET /slots` del decode y restaura en el slot pedido solo si esta libre; si no, en el primer slot libre (con todos ocupados, se encola). El gateway sigue al driver: `id_slot`, cabecera y registro reflejan el slot usado.
- Interferencia medida (A en streaming en el decode, B = prompt de 32k): B directo al decode deja a A en 3,4 tok/s (de 36) con pausas de 1,1 s; B por el gateway (prefill + handoff) deja a A en 37,8 tok/s (de 41,9) con pausa maxima 0,07 s.

## v202609061805 (2026-09-06)
Politica del handoff: solo cuando protege a alguien.

- Nuevo `kv_handoff` en la config del coordinador: `busy` (por defecto) = un prompt largo se traspasa al nodo de prefill SOLO si el decode esta generando para otras peticiones (sonda `GET /slots`, `is_processing`); con el decode libre va directo. `always` = traspaso siempre que se admita.
- Motivo (medido en la e2e real, 27B Q6_K): tras un `restore` la generacion baja de ~66 a ~32-38 tok/s porque el estado de slot de llama-server no incluye el contexto del borrador MTP (dejar K tokens sin procesar para "calentarlo" no lo recupera: K=1/64/512/1024 iguales). El traspaso vale para que un prompt largo no bloquee los streams vivos (interferencia x5,8 medida), no para acelerar una peticion en solitario.
- Registro: `admission: decode-idle` cuando la politica manda directo; una sonda caida se lee como "libre" (camino rapido).

## v202609061750 (2026-09-06)
Prefill/decode desagregado REAL: KV handoff entre nodos (F1 de docs/design/kv-handoff-desagregado.md).

- Gateway `/v1/chat/completions` (JSON y streaming): un prompt largo (estimacion >= 6144 tokens y recuento EXACTO >= 8192 con la plantilla de chat aplicada) se procesa en el nodo de PREFILL y su estado KV viaja al nodo de DECODE, que solo procesa el ultimo token (`timings.prompt_n = 1`). Medido en el spike F0 (27B Q6_K, 10GbE): handoff 0,35 / 0,9 / 2,2 s a 8k / 32k / 100k tokens frente a 3,5 / 14,7 / 66 s reprocesando el prompt en el decode.
- Receta del motor: `apply-template` + `tokenize` en el prefill (misma plantilla que el decode: mismo gguf + `--jinja`), `/completion` con `tokens[:-1]` y `n_predict 0` (la cache recurrente del modelo hibrido no se puede truncar), `slots/0?action=save` + `erase` (presupuesto KV unificado; prefills serializados), el soflink del nodo decode baja el estado directo del soflink del prefill (`POST /control/kv-fetch` <- `GET /kv/<nombre>`), `slots/<slot>?action=restore` y la peticion original con `id_slot` + `cache_prompt`. Ficheros borrados en los dos nodos tras el restore.
- Todo fail-soft: cualquier fallo (recuento, prefill, fetch, restore) degrada a decode directo sin perder la peticion; la causa queda en el registro.
- Nuevo `GET /api/requests`: registro por peticion con decision de admision, via (`decode` / `prefill` / `decode-fallback`), tokens, `prefill_ms`, `save_ms`, `fetch_ms`, `restore_ms`, `handoff_ms`, `prompt_n`, `cache_n`, `kv_miss`, tg tok/s.
- Config nueva por nodo: `kv_state_dir` (= `--slot-save-path` del llama-server del nodo). Activa `GET/DELETE /kv/<nombre>` y `POST /control/kv-fetch` en ese nodo, y anade `--slot-save-path` a los llama-server que lance soflink. Sin ella el gateway sigue decode-only.
- Umbral de admision: 2048 -> 6144 tokens estimados + suelo exacto 8192 (medido: por debajo de 8k el handoff no compensa).
- Tests: gateway con backends simulados (recuento exacto, pin de slot, metricas, kv_miss, streaming) y e2e del coordinador con dos llama-server falsos + tres soflink reales (prefill, decode, gateway) en loopback.

## v202609061625 (2026-09-06)
Mejoras desde la version anterior:

- Sensor /gpu con instantanea en cache: el daemon muestrea nvidia-smi + CPU/RAM en segundo plano cada 2 s y /gpu responde siempre desde la ultima lectura (campo nuevo `age_ms`). En hosts Windows cargados nvidia-smi tardaba 0,4-7 s y el panel marcaba el nodo como caido a ratos.
- Agregador de nodos del panel: timeout por nodo 1,2 s -> 3 s (cola larga de latencia entre nodos de la LAN).
- Panel: tok/s en vivo por instancia (muestreo de /slots entre ticks), etiqueta `model_name` por instancia y ajustes de control (kill por puerto solo de procesos propios).
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
