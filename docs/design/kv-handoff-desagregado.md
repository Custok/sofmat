# Serving desagregado (prefill/decode) — el handoff de KV

> Section owner: transport lane. Feeds the synthesis in `docs/design/escalado.md`.
> Anonymous node labels only; no infra values in any payload or example.

## El problema, medido
Prefill (procesa el prompt de una pasada, **compute-bound**, satura la GPU) y decode
(un token cada vez, **latency/bandwidth-bound**) tienen perfiles opuestos. Co-locados
en el mismo pipeline se estorban: un prefill largo **acapara** el pipeline y congela el
decode de todos los slots activos. Medido en banco: un decode de 40 tokens pasa de su
tiempo normal a **×8.5** cuando entra un prefill de prompt largo en paralelo (el prefill
retiene el pipeline varios segundos). La magnitud del problema no es teórica; la
**frecuencia** decide cuándo construir la solución (esporádico con 1-2 usuarios; frecuente
como producto multi-tenant con muchos consumidores de la misma API).

Desagregar = **nodo(s) de PREFILL dedicado(s)** procesan prompts y **transfieren el KV** al
**pipeline de DECODE**, que fluye sin picos. llama.cpp **no** desagrega entre máquinas →
es territorio propio del motor.

## El mecanismo del handoff existe en el motor base — parcialmente
El payload del handoff es la **serialización del KV de una secuencia**, que el motor ya
expone (`llama.h`):
- `llama_state_seq_get_size_ext(ctx, seq_id, flags)` — tamaño del blob.
- `llama_state_seq_get_data_ext(ctx, dst, size, seq_id, flags)` — extrae el KV de esa
  secuencia a un buffer.
- `llama_state_seq_set_data_ext(ctx, src, size, seq_id, flags)` — lo inyecta en otro contexto.
- Flags relevantes: `ON_DEVICE` (mantener el estado en GPU, evita el round-trip a host en
  el extremo que lo permita), `SWA_ONLY`/`PARTIAL_ONLY` (subconjuntos).

Este API ya lo usa el propio server para su prompt-cache (guardar/restaurar por slot). **Pero
está diseñado para round-trip al MISMO contexto** (reinicio, cache local). El handoff
desagregado NO es ese caso.

## Lo que SÍ es trabajo propio: el mapeo KV cross-topología (scatter)
El contexto de **prefill** corre el **modelo completo en un nodo** (candidato natural: un nodo
de mucha memoria y fuerte en compute — su punto no-débil; cero interferencia con el decode).
El contexto de **decode** es **pipeline-parallel**: el KV de la capa L vive en el nodo que
hospeda L.

Por tanto el handoff **no es "mover un blob a un sitio"**: es un **SCATTER por-capa** — el
KV extraído del prefill (todas las capas) debe **repartirse a cada nodo de decode según el
mapa de capas del decode**, e inyectarse con `set_data_ext` en el contexto local de cada uno.
Ese mapeo entre **una topología (prefill full) y otra (decode pipeline)** es exactamente lo
que el API base no hace y lo que sofmat aporta.

Consecuencias de diseño para el transporte:
- **Perfil BULK, bandwidth-bound** (0,5–10 GB por handoff según longitud de prompt y quant de
  KV) — **opuesto** al de la especulación (activaciones de ~10 KB, latency-bound). El
  `TcpTransport` v0 (pensado para envíos pequeños) necesita un **modo bulk chunked** con
  back-pressure y checksum por chunk.
- **Multiplexado por-frontera:** un stream por nodo de decode destino; el `framing` ya acota
  y valida marcos, se extiende con un tipo de marco `KV_CHUNK{layer_range, seq_id, offset}`.
- **KV cuantizado ayuda:** con KV-q4 el blob es ~1/4 del BF16 → el handoff de prompts ≤32k es
  calderilla; a contextos muy largos el coste del handoff entra en la decisión de si desagregar
  paga (regla: desagregar cuando `interferencia_evitada > coste_handoff`).

## Speculative Prefill (F2 dentro de la spec) — el hogar real del transporte-overlapped
El transporte no-bloqueante/double-buffer (`BufferedSender`), que en la especulación de token
sólo valía ~+12% (el cuello allí era cómputo, no red), **aquí sí paga**: mientras el KV real
viaja en background (0,4–8 s de bulk), el nodo de decode **arranca sobre un DRAFT del prompt**
y **reconcilia** al llegar el KV verdadero. Esconde la latencia del handoff detrás de cómputo.
Coste honesto: el estado inicial es **aproximado → posible rollback** tras reconciliar — es la
misma filosofía de la ola con **mayor coste de corrección**, así que va **después** de que el
desagregado básico (handoff bloqueante) funcione, no antes.

## Prefix caching — mismo payload, dos regímenes
El payload cacheado es el mismo `state_seq_get_data` **keyed por hash del prefijo**.
- **v1 sin transporte (afinidad de slot en el gateway):** rutear peticiones del mismo
  system-prompt/tenant al **mismo slot del mismo nodo** → el prefijo ya está en la cache local
  del slot, **cero bytes de KV por la red**. Es una política de gateway, no motor. Ganancia
  inmediata multi-usuario (system-prompts compartidos: el prefill del prefijo común es gratis
  desde la 2ª petición). Compone directo con el continuous batching.
- **v2 con transporte (cache distribuida):** prefijo computado en un nodo, reutilizado en otro
  → se envía un **ID de prefijo (bytes)** y, en cache-miss, el transporte hace el **fetch del
  blob KV** entre nodos (mismo scatter cross-topología de arriba).

## Cómo compone con lo demás
- **Continuous batching** (ya activo, ~1,9× a 4 slots, y la especulación MTP convive con los
  slots paralelos): el desagregado quita el interference que lo estrangula bajo prefills largos;
  el prefix-caching reduce el trabajo de prefill repetido entre slots.
- **Ola especulativa** (decode): el pipeline de decode sigue corriendo su `--spec-type draft-mtp`;
  el handoff sólo cambia de DÓNDE viene el KV inicial, no cómo decodifica.

## F1 — IMPLEMENTADO (2026-09-06): handoff por estado de slot, sin scatter
La primera versión real NO usa el scatter por-capa: el decode corre el modelo **completo en un
nodo** (una topología por rol, misma cuantización en los dos), así que el payload es el
**estado del slot** que el propio server ya sabe guardar y restaurar (`--slot-save-path`,
`POST /slots/{id}?action=save|restore|erase`). Lo que aporta sofmat es la **secuencia** entre
dos motores en dos máquinas y el transporte del fichero, todo en el gateway (fail-soft).

**Receta medida (spike F0, 27B Q6_K, 10GbE):**
1. Gateway: estimación ≥ `PrefillThresholdTokens` (6144) → `apply-template` + `tokenize` en el
   prefill (misma plantilla que el decode: mismo gguf + `--jinja`; ids verificados idénticos
   entre nodos) → recuento exacto ≥ `PrefillExactMinTokens` (8192) o decode directo.
2. Prefill: `/completion {prompt: tokens[:-1], n_predict: 0, id_slot: 0, cache_prompt: true}`
   → `slots/0?action=save {filename}` → `erase`. Se retiene el ÚLTIMO token porque la cache
   recurrente del modelo híbrido no se puede truncar: el decode debe recibir el prompt idéntico
   y procesar solo ese token. Prefills serializados (presupuesto KV unificado entre slots).
3. Transporte: el soflink del nodo **decode** baja el fichero directo del soflink del nodo
   **prefill** (`POST /control/kv-fetch {url, name}` ← `GET /kv/<name>`; un salto, sin pasar por
   el gateway) a su propio `kv_state_dir` (= `--slot-save-path` del decode).
4. Decode: `slots/<slot>?action=restore {filename}` y la petición original (`/v1/chat/completions`,
   JSON o SSE) con `id_slot: <slot>` + `cache_prompt: true` → `timings.prompt_n = 1`,
   `cache_n = N-1`. Fichero borrado en ambos nodos.
5. Registro por petición (`GET /api/requests`): vía (`prefill` / `decode` / `decode-fallback` con
   causa), tokens, `prefill_ms`, `save_ms`, `fetch_ms`, `restore_ms`, `handoff_ms`, `prompt_n`,
   `cache_n`, `kv_miss` (prompt_n > 64 tras un handoff = el motor reprocesó).

| prompt | estado | save | GET (10GbE) | restore | handoff total | reprocesar en decode |
|---|---|---|---|---|---|---|
| 8k | 290 MiB | 0,19 s | 0,27 s | 67 ms | **0,35 s** | 3,5 s |
| 32k | 711 MiB | 0,48 s | 0,67 s | 139 ms | **0,9 s** | 14,7 s |
| 100k | 1 815 MiB | 1,18 s | 1,71 s | 418 ms | **2,2 s** | ~66 s |

Estado ≈ 19 KB/token + 150 MiB. Umbral: desde 8k el ahorro (3,1 s) supera el coste; por debajo
no compensa. El scatter por-capa de arriba sigue siendo el camino cuando el decode vuelva a ser
pipeline multi-nodo (topologías distintas por rol).

Código: `internal/gateway/gateway.go` (Prepare/Finish), `internal/coordinator/kvpipe.go`
(drivers), `internal/coordinator/kvstate.go` (`/kv/*`, `/control/kv-fetch`, `--slot-save-path`).

**Límite conocido (e2e real, 2026-09-06) y política resultante.** Tras el `restore`, la generación
del decode baja de ~66-69 a ~31-33 tok/s: la aceptación del borrador MTP cae del ~60 % al ~17-25 %.
Descartado: el `n_max` del gateway (mismo tg directo con 3/16/defecto), dejar K tokens sin procesar
para "calentar" la MTP (K = 1/64/512/1024 iguales), el ubatch del prefill (`-b 2048 -ub 512` = decode,
sin cambio). En el MISMO proceso (save en un slot, restore en otro) el coste es −16 %; entre procesos
−52 %: lo que se pierde está en cómo llama-server serializa el estado (el contexto del borrador MTP y/o
el estado recurrente), y eso es territorio del motor. Consecuencia: **el traspaso no acelera una
petición en solitario** (11,5k: 8,0 s por el gateway vs 5,8 s directo, porque el pp del prefill es el
mismo que el del decode y se suman ~0,7 s de save+fetch+restore); **sí protege a los demás**:
| escenario (A streaming en el decode, B = prompt largo) | A durante B | pausa máx. de A |
|---|---|---|
| B directo al decode (co-locado) | **3,4 tok/s** (de 43) | 1,18 s |
| B por el gateway → prefill + handoff | ~40 tok/s | 0,06 s |
Por eso el gateway aplica `kv_handoff: busy` (por defecto): sonda `GET /slots` del decode y traspasa
SOLO si algún slot está `is_processing`; con el decode libre va directo. `always` fuerza el traspaso.

## Non-goals / abierto
- No construir aún: **spec de diseño**; se implementa cuando la carga real (multi-tenant) haga
  frecuente el interference ×8.5.
- Abierto (medir antes de construir): ¿el `set_data_ext` acepta inyección parcial por-capa o hay
  que trocear el blob manualmente?; ¿`ON_DEVICE` evita el round-trip a host en ambos extremos o
  sólo en el productor?; coste real del scatter sobre la 10GbE con KV-q4 a 32k/150k.
- Seguridad: el blob KV viaja autenticado y con checksum (mismo `common/auth` + framing validado
  del transporte); nunca en claro sin el token compartido.
