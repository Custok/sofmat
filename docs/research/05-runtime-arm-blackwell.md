# 04 — Runtime worker, carga por rango de capas y toolchain aceleradores ARM de última generación

Owner: `node-b`. Cómo un worker ejecuta su etapa, y qué muerde cuando el nodo es
ARM + memoria unificada + Blackwell reciente (el caso "raro" del pool que
estresa al particionador desde el día 1).

## 1. Carga por rango de capas (no el modelo entero)

Un worker de etapa carga SOLO `[first_layer, first_layer+n_layers)`, nunca el
modelo completo. Dos formas según el formato de pesos:

- **Safetensors sharded** (HF): el `index.json` mapea tensor→shard. Se leen solo
  los shards que contienen los bloques de la etapa → `mmap` + `weights_only`.
  Ventaja: carga selectiva nativa, sin materializar lo ajeno.
- **GGUF** (llama.cpp): los tensores por-capa están etiquetados
  (`blk.N.*`); se pueden cargar rangos. Es lo que hace el `rpc-server` de
  llama.cpp por dentro (Fase 0 lo valida).

Regla dura: los pesos salen SOLO de `StageSpec.model_path` (config local
validada), jamás de la red (OWASP A08). El worker NO descarga pesos que le
lleguen por el cable — recibe activaciones, no parámetros.

## 2. Presupuesto de memoria: pesos + KV, con la unificada como trampa

- KV-cache por etapa = `n_layers · max_context · bytes_por_token_por_capa`.
  Es restricción DURA reservada DENTRO de `model_mem_cap_gb`, no encima (error
  clásico: contar solo pesos y morir por OOM al llenar contexto).
- **Memoria unificada (memoria unificada, Apple):** GPU y CPU comparten el mismo
  pool físico. "Tengo 128 GB" NO significa 128 GB para el modelo: el SO, el
  runtime y el KV compiten en el mismo espacio. De ahí el `model_mem_cap_gb` <
  memoria total (p.ej. 80 de 128). El resto sirve de spillover de KV local
  barato (misma RAM), palanca de capacidad que un nodo con VRAM discreta no
  tiene.

## 3. Toolchain aceleradores ARM con memoria unificada — el campo de minas

Observaciones de operar el nodo (empíricas; verificar versiones exactas antes de
citar en el repo público):

- **flash-attention:** compilar FA en el host bloquea/reinicia la caja (horas de
  build en ARM). Usar SIEMPRE wheel precompilado aarch64 + CUDA correspondiente,
  no `pip install flash-attn` a pelo.
- **FP8 en arquitecturas de GPU recientes:** los kernels FP8 (E4M3) de MoE dan salida basura en esta
  generación de GPU con ciertos runtimes → un modelo FP8-MoE puede "cargar y
  arrancar" y aun así generar tokens corruptos. Implicación para sofmat: en el
  nodo ARM, preferir bf16/fp16 o cuantización que sepamos validada; NO asumir
  que FP8 "cabe = funciona". El microbench debe ir acompañado de un check de
  CORDURA de salida (no solo de latencia).
- **Wheels x86-only:** parte del ecosistema (algunos builds de bitsandbytes,
  ciertos kernels triton) no trae aarch64 → o wheel de la comunidad, o compilar,
  o evitar esa ruta en el nodo ARM.
- **Ancho de banda de memoria bajo** (~273 GB/s en el Spark vs ~1 TB/s de una
  5080 discreta): en decode (memory-bound) el nodo ARM es intrínsecamente lento
  por capa. El particionador debe darle POCAS capas por velocidad aunque le
  sobre memoria (invariante 2). El worker lo respeta reportando su `ms_per_layer`
  real (no de catálogo) — que en ARM será alto y honesto.

## 4. Consecuencia para el diseño

El nodo ARM es **capacidad, no velocidad**: aporta el grueso de la memoria del
pool y absorbe las capas que no caben en los nodos rápidos, a costa de latencia
(aceptable por el objetivo = capacidad). Riesgos de camino crítico: kernels
frágiles (FP8) y toolchain. Mitigación: check de cordura de salida en el
microbench, dtypes conservadores por defecto en config, y perfiles medidos que
delaten la lentitud real para que el solver no le cargue de más.

## 5. Para Fase 0 (coordinator-lane / partitioner-lane)

Al medir el nodo ARM en el baseline llama.cpp RPC: registrar no solo tokens/s
sino (a) `ms_per_layer` medido para el cost-model, y (b) una comparación de
CORDURA de la salida distribuida vs single-host — en aceleradores ARM de última generación "funciona"
no se puede asumir, hay que verlo.

---
*Citas técnicas escritas de memoria de operación del nodo; nombres de kernels y
números de ancho de banda son aproximados — verificar contra la doc de la GPU y
las versiones instaladas antes de referenciar en el repo público.*
