# 02 — Particionamiento óptimo de LLMs en pipeline sobre hosts heterogéneos

> Brief de investigación (módulo: particionador).
> Citas centrales VERIFICADAS con búsqueda web (20-08-2026):
> - PipeEdge: Hu, Imes et al., IEEE 2022 — https://ieeexplore.ieee.org/document/9996638/
>   (código: https://github.com/usc-isi/PipeEdge). Confirmado: DP óptimo con
>   heterogeneidad de cómputo, memoria Y red.
> - GPipe: Huang et al. — https://arxiv.org/pdf/1811.06965 . Confirmadas la
>   fórmula de burbuja (p−1)/(m+p−1) y la regla práctica m ≥ 4p.
> - HetPipe: Park et al., 2020 (confirmado).
> - HexGen: Jiang et al., 2024 — partición asimétrica TP/PP sobre GPUs y redes
>   heterogéneas (confirmado); **novedad encontrada al verificar: HexGen-2
>   (ICLR 2025, https://arxiv.org/html/2502.07903) — inferencia DESAGREGADA
>   (prefill/decode separados) en entornos heterogéneos**: refuerza la
>   recomendación §2 de tratar prefill y decode como regímenes distintos.
> - Petals (Borzunov et al., 2022) y Alpa (Zheng et al., OSDI 2022): de
>   memoria técnica, consistentes con lo anterior; verificar ID exacto si se
>   citan formalmente en el repo.

## 1. Formulación del problema (el nuestro, con nombre académico)

Repartir N capas CONTIGUAS entre H hosts ordenados es el problema clásico de
**chain partitioning / contiguous array partitioning min-max**: dividir una
secuencia de costes c_1..c_N en H tramos contiguos minimizando el coste del
tramo peor. Con costes por-host (heterogéneo: coste de la capa i en el host h)
sigue siendo resoluble EXACTO:

- **DP O(N²·H)** (o O(N·H·log) con búsqueda binaria sobre el makespan +
  test de factibilidad greedy). Para N≈100 capas y H≤5 es instantáneo.
- Nuestra v0 usa waterfilling + reparación local (heurística). **Recomendación
  v1: sustituir por el DP exacto** — mismo input, garantía de óptimo, coste
  despreciable. La heurística puede quedarse como semilla/verificación.
- Con caps de memoria por host (nuestro `model_mem_cap_gb`) el DP solo añade
  una comprobación de factibilidad por tramo. La parsimonia (mínimo nº de
  etapas) se resuelve iterando H=1,2,… y quedándose con el primer H factible
  (exactamente lo que hace la v0).

## 2. Burbuja de pipeline: la matemática que nos aplica

- **GPipe** (Huang et al., 2019): con m micro-batches y H etapas, la fracción
  de burbuja es (H−1)/(m+H−1). Regla práctica del paper: m ≥ 4·H para que la
  burbuja sea <~20%. → Para nuestro batch=1 interactivo (m=1) la burbuja es
  máxima: en DECODE autoregresivo el pipeline se comporta como una CADENA
  SECUENCIAL (cada token atraviesa todas las etapas). Nuestro KPI de
  red<10-15% ya modela esto bien: en decode lo que importa es
  Σ(etapas) + fronteras, no la burbuja clásica.
- **PipeDream / 1F1B** (Narayanan et al., 2019/2021): schedule que solapa
  forward/backward — aplica a TRAINING; para inferencia pura nos aplica solo
  la variante de streaming de requests (varias peticiones en vuelo llenan el
  pipeline → throughput). Es la base técnica de nuestra "Fase 1.5".
- **Prefill vs decode**: el prefill SÍ se beneficia de micro-batching por
  chunks (chunked prefill, estilo Sarathi/vLLM): trocear el prompt en chunks
  que avanzan en pipeline tapa la burbuja del prefill largo. Recomendación:
  el coordinador debe tratar prefill (compute-bound, paralelizable por
  chunks) y decode (memory-bound, secuencial) como regímenes DISTINTOS.

## 3. Sistemas y papers directamente relevantes

- **PipeEdge** (Hu et al., 2022): partición óptima de transformers en
  pipeline sobre dispositivos edge HETEROGÉNEOS con DP; es el paper más
  cercano a sofmat (incluye memoria como restricción y enlaces lentos).
- **HetPipe** (Park et al., 2020): pipeline sobre GPUs heterogéneas,
  formaliza el reparto desigual de capas por velocidad.
- **Alpa** (Zheng et al., 2022) y **AMP**: auto-paralelización jerárquica
  (inter-op = pipeline, intra-op = tensor) — valida nuestra arquitectura
  TP-intra × PP-inter como óptima estructural, con búsqueda automática.
- **Metis / HexGen** (2023-24): serving en clusters heterogéneos con
  particiones asimétricas TP×PP por grupo de GPUs — HexGen en concreto
  reparte sobre GPUs de distinta VRAM/velocidad conectadas por red lenta
  (nuestro caso exacto, orientado a throughput).
- **Petals** (Borzunov et al., 2022): inferencia colaborativa por bloques
  sobre WAN — demuestra viabilidad de PP sobre redes MUY lentas y aporta el
  patrón de rebalanceo dinámico cuando aparecen/desaparecen nodos (nuestra
  membresía elástica + mapas N-1).
- **exo / distributed-llama / llama.cpp RPC**: implementaciones prácticas;
  llama.cpp RPC (nuestra Fase 0) hace PP de facto con reparto por VRAM — su
  punto débil documentado es justo el que ataca sofmat: reparto ciego a la
  velocidad de memoria y sin tolerancia a fallos.
- **Decode memory-bound**: en batch=1, t_capa ≈ bytes_capa / BW_memoria
  (el roofline de inferencia; base de nuestro modelo de coste). Con
  cuantización, los bytes bajan → sube tok/s lineal — la palanca de
  capacidad barata sigue siendo quant agresiva antes que más hosts.

## 4. Implicaciones concretas para sofmat

1. **v1 del solver = DP exacto** (sustituir heurística; trivial con N≈100).
2. **Separar prefill/decode en el coste**: el veto "red<15%" debe evaluarse
   en DECODE (peor caso); el prefill se salva con chunked-prefill.
3. **KV por etapa** ya lo hacemos como los papers (cada etapa guarda el KV
   de SUS capas); con contextos largos considerar KV-offload a RAM del host
   (un nodo de memoria unificada con RAM reservada fuera del cap puede usarla
   como spillover local del KV).
4. **Rebalanceo estilo Petals** para la elasticidad: nuestros mapas N-1
   precalculados son la versión "instantánea"; a futuro, rebalanceo
   incremental (mover solo capas frontera) minimiza la recarga.
5. **min_usable_tokens_s** (stub ya en config): la literatura de serving
   heterogéneo (HexGen) confirma que a veces conviene NO usar el nodo
   grande-lento aunque quepa — el suelo de velocidad es la forma estándar
   de expresarlo. Decisión de producto, mecanismo listo.

## 5. Preguntas abiertas (para siguientes briefs / bench de Fase 0)

- ¿Overhead real por frontera en nuestra 10GbE-TCP (RTT+framing)? El bench
  de node-a debe darlo medido; la literatura reporta 0.5-2 ms alcanzable
  con TCP afinado (nodelay, buffers) — si medimos >5 ms, mirar el framing.
- ¿Cuánto degrada el KV-quant (q8/q4) la calidad en nuestros modelos
  objetivo? Palanca directa de capacidad (el KV compite con los pesos
  dentro del cap).
- Speculative decoding distribuido (draft en el master, verify en el
  pipeline): reduciría el nº de travesías de pipeline por token generado —
  prometedor para recuperar latencia 1-user, pero complejidad alta (Fase 2+).
