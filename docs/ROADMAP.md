# Roadmap

sofmat is early. This is what exists and what comes next. Direction over dates.

## v0 — foundations (here now)
- **Partitioner** — fit (hard) + fastest, memory-bandwidth aware, N-1 fallbacks,
  network-fraction veto.
- **Transport** — binary frame (no pickle), HMAC auth, bounded TCP; RDMA-ready
  interface.
- **Coordinator** — master runs the plan across stage workers; per-token KPI;
  `MockBackend` so the whole pipeline runs end-to-end with no GPU.
- **Leak-guard** — secret / private-infra + anti-deserialisation gate (pre-commit
  + CI).
- Each module independently tested; an end-to-end mock pipeline is green in CI.

## v1 — real inference
- **`StageBackend` = a thin llama.cpp driver** — sofmat is orchestration, not a
  new inference engine; the per-stage backend hands its layer range to llama.cpp
  and wires its in/out to the transport. (Not a from-scratch torch engine.)
- **`sofmat serve` — one OpenAI-compatible API across the cluster.**
  `sofmat serve --config cluster.yaml` brings the model up split across every
  node and exposes `/v1/chat/completions` on the master. Any OpenAI client talks
  to the whole cluster as a single endpoint — the product: LM Studio, distributed.
- **Worker-to-worker chain** — replace the v0 star (master round-trips each
  stage) with a direct chain (worker → worker, master only at the ends) to halve
  boundary crossings. The `Transport` interface already allows it.
- **Exact DP partitioner** — replace the v0 waterfilling heuristic with the
  optimal chain-partitioning dynamic program (cost = memory-bandwidth model).
- **Config parser + validation** — typed load of `config.local.yaml`, fail-closed.
- **Weight distribution ("serve one to all")** — keep the full model on a single
  node; each worker pulls only *its* layer range once at startup over the
  network, instead of every node holding a full copy of a 100+ GB file. A shared
  mount is the simple alternative; streaming per-stage at load time is the goal.
- **Benchmark harness** — measure tokens/s and network fraction on a real cluster
  against **prima.cpp** and **llama.cpp RPC**. If we don't clearly beat them, the
  honest move is to contribute there rather than reinvent — and the bench says so.

## v1.5 — speed and capacity levers
- **Speculative decoding on the pipeline** (SpecPipe / FlowSpec style) — a small
  draft model on the master, workers verify; hides the pipeline bubble for fast
  single-user decode.
- **KV-cache tier-2** — spill KV to CPU RAM (GPU → CPU → GPU, HMA style) so a
  node's system RAM becomes usable capacity; quantised KV (q8/q4).
- **Size-conditional compression** — compress activations/KV in prefill and on
  KV migration during failover; *never* on the tiny decode hot-path.

## Research
The reasoning and prior art behind these choices — pipeline vs tensor parallelism
over Ethernet, heterogeneous partitioning, prima.cpp/Halda, KV offload — is in
[`docs/research/`](research/), with verified citations.

## Non-goals
- Beating single-host latency for one user (the network adds per-token latency;
  pooling is for **capacity** and throughput).
- Requiring RDMA / InfiniBand. sofmat targets the Ethernet you already have.
