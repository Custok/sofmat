# Research brief 04 — Bleeding-edge 2025-2026 (búsqueda web REAL, citas verificadas)

> Curado por coordinator-lane para sofmat. Fuente: WebSearch (ago-2026). URLs reales al final; **estas citas SÍ se pueden referenciar en el repo** (verificadas, no de memoria).

## Lo más importante: PRIMA.CPP — es casi sofmat, y es el LISTÓN
**Prima.cpp** (arXiv 2504.08791): llama.cpp distribuido que corre **30-70B en clusters domésticos heterogéneos** (CPU/GPU mixtos, RAM/VRAM insuficiente, discos lentos, Wi-Fi, OS distintos). Aporta dos ideas que nos tocan de lleno:
- **Pipelined-Ring Parallelism (PRP):** solapa **disco/IO + cómputo + comunicación** en un anillo → clave si offloadeamos pesos (nuestro Spark).
- **Halda:** scheduler **heterogeneity-aware** que co-optimiza carga CPU/GPU **y selección de dispositivos** bajo restricción RAM/VRAM. → **es la versión avanzada del particionador de partitioner-lane** (añade el eje CPU/GPU y el "usar o no un device", que valida nuestro `min_usable_tokens_s`).
- **Resultados:** 70B a **674 ms/token** TPOT en 4 equipos de consumo, <6% presión de memoria; 32B con **speculative decoding** a **26 tok/s**; **5-17× menor TPOT que llama.cpp, exo y distributed-llama**; OOM-free.
- **Implicación:** llama.cpp-RPC/exo NO son el listón real — **Prima.cpp sí**. Fase 0 debe medir contra Prima.cpp, y deberíamos **estudiar Halda y PRP a fondo** (¿construir sobre ideas suyas o batirlas?).

## Parallax — valida nuestra apuesta PP sobre red de consumo
**Parallax** (gradient.network): PP como estrategia base para shardear en máquinas heterogéneas de consumo por internet. **3.1× menos latencia e2e, 5.3× mejor inter-token latency, 3.1× más throughput** vs baselines. TP intra-nodo × PP inter-nodo = exactamente nuestra estructura. Confirma el diseño.

## Otros sistemas/paper 2025-2026 a vigilar
- **Hetis** (arXiv 2509.08309): serving en clusters GPU heterogéneos con **paralelismo fino y DINÁMICO** → relevante para membresía elástica (node-c/d entran/salen).
- **ShuntServe** (arXiv 2606.18600): serving coste-eficiente en **spot GPUs heterogéneas** → patrones de tolerancia a nodos que desaparecen (= nuestro caso PSU/avatares).
- **HARP** (2509.24859), **Unified Tensor Resharding** (2606.26633): resharding dinámico — para rebalanceo incremental futuro (mover solo capas frontera, estilo Petals).

## Red (guía práctica 2026)
10GbE **funciona** para PP (con batch grande); 25GbE cómodo; 100GbE/IB ideal. → nuestra 10GbE es viable para PP; el margen está en **minimizar y solapar** transferencias (ya en el KPI).

## Palanca de VELOCIDAD que pide el owner: SPECULATIVE DECODING
Prima.cpp saca 26 tok/s en 32B con speculative decoding. Es la palanca directa para "lo más rápido posible": un draft model pequeño propone, el grande verifica. **Añadir al roadmap de sofmat** (Fase 1.5) — encaja con master+workers (el draft puede vivir en node-a).

## Acciones para sofmat
1. **partitioner-lane:** leer **Halda** (Prima.cpp) — co-optimización CPU/GPU + device-selection; es tu particionador +1. Y PipeEdge (brief 02) para el DP.
2. **Fase 0 (coordinator-lane + .51/.58):** baseline contra **Prima.cpp** (no solo llama.cpp-RPC) — es el número real a batir.
3. **Roadmap:** speculative decoding (velocidad), PRP-style IO/compute overlap (para offload en Spark), rebalanceo incremental (Hetis/Unified Resharding).
4. **Gobernanza:** estas citas están verificadas por búsqueda → OK para el repo público.

## Fuentes
- Prima.cpp — arXiv 2504.08791: https://arxiv.org/pdf/2504.08791 · https://arxiv.org/html/2504.08791v3
- Parallax — https://gradient.network/parallax.pdf
- Hetis — https://arxiv.org/pdf/2509.08309
- ShuntServe — https://arxiv.org/pdf/2606.18600
- HARP — https://arxiv.org/pdf/2509.24859 · Unified Tensor Resharding — https://arxiv.org/pdf/2606.26633
- Multi-node local LLM 2026 guide — https://fungies.io/multi-node-local-llm-inference-guide-2026/
