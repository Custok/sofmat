# Research brief 06 — Palancas 2026: velocidad (spec-decoding en pipeline) + capacidad (KV offload/compresión)

> Curado por coordinator-lane. Búsqueda web REAL (ago-2026), citas verificadas → aptas para el repo público. Dos ejes que el owner pidió: **rápido** y **que quepa más**.

## A. VELOCIDAD — speculative decoding SOBRE pipeline (mata la burbuja del PP)
El talón de Aquiles del PP en 1-usuario es la burbuja (cada token recorre toda la cadena). La literatura 2025-2026 lo ataca metiendo **speculative decoding dentro del pipeline**:
- **SpecPipe** (arXiv 2504.04104): rellena el pipeline con tokens especulativos paso a paso → **idealmente 1 token decodificado por paso de pipeline**, maximizando utilización del hardware. Ataca directamente la latencia del PP.
- **FlowSpec** (arXiv 2507.02620): speculative decoding **pipeline-parallel, tree-based**, para inferencia distribuida en dispositivos de recursos limitados (edge/consumo) = nuestro caso exacto.
- **Speculative Pipeline Decoding** (arXiv 2605.30852): especulación **zero-bubble** vía PP.
- **Early-exit self-speculative + PP** (arXiv 2509.19368); **distributed speculative decoding** (ScienceDirect S2949715925000782).
- Base: spec-decoding = generar varios tokens en el tiempo de uno **sin cambiar la distribución de salida** (~20-50% más rápido; medium/practical-llm-systems).

**Para sofmat:** roadmap **Fase 1.5 → speculative decoding tipo SpecPipe/FlowSpec**. El **draft model pequeño vive en node-a (master)**; los workers verifican en pipeline. Es la palanca #1 para "lo más rápido posible" en 1-usuario sin más hosts.

## B. CAPACIDAD + ANCHO DE BANDA — KV offload/compresión (valida el tier-2 de partitioner-lane)
"El KV-cache es el CENTRO del serving en 2026" (lecompute). Técnicas directamente aplicables:
- **LMCache** (CacheGen): **comprime el KV ANTES de transferirlo** + offload a CPU + caché distribuida → reduce el ancho de banda inter-nodo. → **comprimir KV/activaciones antes de cruzar nuestra 10GbE.**
- **HMA (Heterogeneous Memory Architecture):** KV sigue trayectoria **GPU → CPU → GPU sin cambio de formato** → así se usan los **48 GB reservados del Spark + las RAM de 96 GB** como spillover de KV = **el tier-2 CPU/RAM de partitioner-lane, confirmado por literatura**. Es la palanca para pasar de 160 GB sin el A100.
- **KVServe** (arXiv 2605.13734): compresión de KV service-aware para serving **desagregado** comunicación-eficiente.
- **DiffKV / R-KV / BumbleBee:** cuantización+pruning de KV por-token/por-head (importancia heterogénea).
- **HiSparse:** offload GPU→CPU de KV para atención sparse, **>3× throughput** a 256 req.
- **PM-KVQ** (ICLR 2026): cuantización KV de precisión mixta progresiva para CoT largo.
- Referencias marco: survey *Awesome-KV-Cache-Optimization* (ACL 2026); *Demystifying Heterogeneous LLM Inference* (arXiv 2606.29708).

**Para sofmat:** (1) **KV quantizado (q8/q4)** como default → capacidad y menos transferencia; (2) **tier-2 RAM** (partitioner-lane v2) con trayectoria HMA GPU→CPU→GPU; (3) **comprimir KV/activaciones antes del cable** (estilo LMCache) → ayuda al KPI de red-transparencia.

## Acciones
1. **Roadmap velocidad:** spec-decoding en pipeline (SpecPipe/FlowSpec) = Fase 1.5, draft en master. La palanca #1 para 1-usuario rápido.
2. **partitioner-lane:** tu tier-2 CPU/RAM (v2) está validado por HMA/LMCache → cost-model con BW de CPU y compresión de KV.
3. **transport-lane:** compresión **CONDICIONAL AL TAMAÑO** (matiz de transport-lane que evita optimizar en falso): NO en el hot-path de **decode** (activación 8-16 KB → ~10-13 µs en 10GbE; comprimir cuesta más y añade latencia al camino crítico). SÍ en **prefill** (activaciones de MB) y en **migración de KV** al remapear por caída (mover KV comprimido = menos downtime).
4. Default de capacidad: **KV cuantizado**.

## Fuentes
- SpecPipe — https://arxiv.org/abs/2504.04104 · FlowSpec — https://arxiv.org/html/2507.02620 · Speculative Pipeline Decoding — https://arxiv.org/html/2605.30852v2 · Early-exit+PP — https://arxiv.org/pdf/2509.19368 · Distributed spec — https://www.sciencedirect.com/science/article/pii/S2949715925000782
- KVServe — https://arxiv.org/pdf/2605.13734 · Heterogeneous inference survey — https://arxiv.org/pdf/2606.29708 · Awesome-KV-Cache-Optimization (ACL 2026) — https://github.com/jjiantong/Awesome-KV-Cache-Optimization · KV-cache central 2026 — https://lecompute.fr/en/runtimes/kv-cache-objet-central-serving/
