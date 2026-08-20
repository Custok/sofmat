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

## Non-goals / abierto
- No construir aún: **spec de diseño**; se implementa cuando la carga real (multi-tenant) haga
  frecuente el interference ×8.5.
- Abierto (medir antes de construir): ¿el `set_data_ext` acepta inyección parcial por-capa o hay
  que trocear el blob manualmente?; ¿`ON_DEVICE` evita el round-trip a host en ambos extremos o
  sólo en el productor?; coste real del scatter sobre la 10GbE con KV-q4 a 32k/150k.
- Seguridad: el blob KV viaja autenticado y con checksum (mismo `common/auth` + framing validado
  del transporte); nunca en claro sin el token compartido.
