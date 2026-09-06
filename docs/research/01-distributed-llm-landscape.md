# Research brief 01 — Distributed LLM landscape (multi-host VRAM pooling over 10GbE)

> Curado por coordinator-lane para sofmat. Objetivo: correr un LLM que no cabe en un host, sumando VRAM de hosts heterogéneos sobre 10 GbE (TCP, sin RDMA). Síntesis del estado del arte + apoyo qwen3.8-27b.

## 1. Estrategias de paralelismo y viabilidad sobre 10 GbE
- **Tensor-Parallel (TP):** all-reduce de activaciones en CADA capa (2 por capa). Por token con hidden `h` y capas `L`: ~`2·L·batch·h·dtype` bytes + cientos de sincronizaciones de µs. Ej. h=8192, L=80, bf16 → ~5 MB/token y ~160 syncs; sobre 10 GbE (1.25 GB/s, RTT 50-100 µs) **la latencia de tantos syncs colapsa el decode**. → **TP solo intra-host** (NVLink/PCIe).
- **Pipeline-Parallel (PP):** solo cruza el vector de activación (`batch·h·dtype`, ~16-32 KB/token) en cada FRONTERA, 1 vez. Con pocas fronteras el coste de red es despreciable vs cómputo. → **PP = el eje entre hosts.**
- **Híbrido TP(intra)×PP(inter):** estándar tipo Megatron. Encaja: pares 2×5080 = TP2, PP entre hosts.
- **Expert/MoE parallel:** repartir expertos → all-to-all por capa (sparse). Viable solo con pocos expertos activos + localidad de routing; más frágil que PP sobre Ethernet. ⚠ Relevante si la diana es MoE (qwen3-MoE): la colocación de expertos importa.

## 2. Sistemas OSS existentes
| sistema | reparto | transporte | heterogéneo | notas |
|---|---|---|---|---|
| **vLLM + Ray** | TP + PP (`pipeline_parallel_size`) | Ray/NCCL | malo (quiere GPUs iguales) | máx throughput (continuous batching, PagedAttention); datacenter |
| **llama.cpp RPC** (`rpc-server`) | offload de rangos de capas | protocolo TCP propio | **excelente** (mezcla archs/OS) | PP de facto; single-stream modesto; ⚠ históricamente **sin auth** → baseline Fase 0 |
| **exo** | auto-particiona por memoria, anillo P2P | P2P TCP | bueno | fácil pero inmaduro; perf variable ("se arrastra") |
| **Petals** | PP en enjambre estilo BitTorrent | TCP/gRPC WAN | sí | **tolerancia a fallos**: reenruta si un peer cae → estudiar su resiliencia |
| **distributed-llama** | split por capas/heads sobre TCP, sync cuantizado | TCP propio | sí (consumer clusters) | buenas cifras sobre Ethernet; candidato a benchmark |
| **Aphrodite** | fork vLLM (TP/PP) | NCCL/Ray | malo | similar a vLLM |

## 3. Partición consciente del ancho de banda de memoria
Decode = **memory-bandwidth-bound**: `tok/s`_etapa ≈ `mem_bw / bytes_de_capas_en_la_etapa`. → repartir capas **∝ mem_bw** para igualar el tiempo de etapa (no ∝ VRAM). Formulación: **minimizar `max(tiempo_etapa)`** sujeto a `VRAM(pesos+KV) ≤ cap` por nodo, con rangos **contiguos** (lo exige PP). Es *min-max chain partitioning on a line* → **DP** `O(L²·N)` o **binary-search sobre el tiempo + greedy**. (= el solver de partitioner-lane.)

## 4. Aumentar CAPACIDAD (clave para nuestro objetivo)
- **Cuantización:** GGUF k-quants (Q4_K_M ~4.5 bpw), AWQ/GPTQ (4-bit GPU-friendly), FP8 (nativo Blackwell — ⚠ MoE sm_121 frágil, gateway-lane). Q4 ~½ del footprint vs bf16 → **160 GB sostiene ~300B params en Q4.** Palanca principal.
- **KV-cache:** PagedAttention, KV cuantizado (Q8/Q4), **offload de KV a RAM CPU** (crítico en contexto largo). KV = restricción DURA en el particionador.
- **Offload de pesos a RAM:** llama.cpp `-ngl` parcial; el DGX Spark (128 GB unificada) es "offload en caja". Velocidad ↔ capacidad.

## 5. Network-transparency (que el límite sea el cómputo)
- **Micro-batching** para llenar la pipeline (tapa la burbuja) — en 1-user batch=1 la burbuja es inherente; ayuda al throughput, no a la latencia single-stream.
- **Solape comms/cómputo:** doble buffer, enviar activación de etapa N mientras N+1 computa.
- **Compresión de activaciones** (FP8/INT8 por el cable): ~½ transferencia; barato.
- KPI: por token, `red+espera < cómputo`. Se logra **minimizando fronteras (parsimonia)** y dimensionando etapas con cómputo suficiente.

## 6. Tolerancia a fallos (nodos elásticos que desaparecen)
- Heartbeat por etapa + timeout; **mapas N-1 precalculados** → el master activa el fallback y **reintenta el token en vuelo**.
- El KV vive por etapa: si una etapa cae, se pierde su KV → reiniciar la secuencia en esa etapa (replicar KV es caro). v0: **fail-and-retry con mapa N-1**. Estudiar el rerouting de Petals para v2.

## 7. Recomendaciones para sofmat
1. Master (node-a) orquesta; workers = rangos contiguos de capas (TP2 donde haya 2 GPUs).
2. Transporte gRPC/TCP con **framing binario, sin pickle**, **autenticado** (token); solape send/compute.
3. Partición ∝ mem_bw con cap VRAM+KV duro y **parsimonia**.
4. **Cuantizar agresivo** (Q4/Q5 GGUF o AWQ) para maximizar tamaño dentro de 160 GB.
5. Bench: desglose por token compute/red/burbuja (el KPI).
6. Baselines Fase 0: **llama.cpp RPC** y **distributed-llama**.

## Papers / keywords a profundizar
- **Megatron-LM** (Shoeybi 2019) — TP/PP. · **GPipe** (Huang 2019), **PipeDream** (Narayanan 2019) — pipeline + burbuja.
- **Petals** (Borzunov 2022) — PP descentralizado tolerante a fallos. · **PagedAttention/vLLM** (Kwon 2023).
- **FlexGen** (Sheng 2023) — offload para capacidad con GPU limitada.
- **AWQ** (Lin 2023), **GPTQ** (Frantar 2022), GGUF k-quants.
- Búsquedas: "memory-bandwidth-bound decode", "pipeline parallel inference heterogeneous cluster", "activation compression LLM", "KV cache offload", "distributed-llama b4rtaz", "exo-explore".
