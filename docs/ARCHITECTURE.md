# Architecture

sofmat runs one large language model across several heterogeneous hosts joined
by ordinary Ethernet (TCP, no RDMA). This document explains how the pieces fit
so you can read the code — or fork it — with the whole picture in mind.

## The one idea that drives everything: interconnect bandwidth
Two ways to split a model across machines, and a 10 GbE link only tolerates one:

- **Tensor parallelism** all-reduces activations *every layer* — hundreds of
  tiny, latency-bound synchronisations per token. That belongs on NVLink /
  InfiniBand. Over 10 GbE it crawls. So sofmat keeps tensor parallelism **inside
  a single host** (the GPUs on one machine), never across the network.
- **Pipeline parallelism** hands each host a *contiguous range of layers* and
  passes only the per-token activation (tens of KB) across each stage boundary,
  once. That is cheap on 10 GbE. So sofmat pipelines **across hosts**.

Everything else follows from this: TP within a host × PP between hosts.

## Components and data flow
```
 config.local.yaml
        │  (your cluster: nodes, memory caps, measured speeds)
        ▼
 partitioner/        solver.solve(nodes, model, boundary_overhead_ms, …)
   → PartitionPlan: which contiguous layer range lives on which node, and the
     predicted per-token time and network fraction. Emits N-1 fallback plans.
        │
        ▼
 coordinator/        Coordinator(plan, endpoints, token)   ← the master (node-a)
   → dials one authenticated channel per stage and pushes the hidden state
     through the stages token by token; measures network vs compute (the KPI).
        │  transport/ (binary frame, HMAC auth, bounded TCP)
        ▼
 StageWorker(backend) on each node   ← runs one contiguous layer range
   → StageBackend.forward(hidden) : the real one is torch/CUDA (the runtime);
     MockBackend is a dependency-free stand-in so the pipeline runs with no GPU.
```

## Modules
- **`partitioner/`** — pure-arithmetic solver. Hard constraint: weights + KV
  fit each node's `model_mem_cap_gb`. Objective (design decision): among fitting
  splits, the *fastest*; `min_usable_tokens_s` is a speed floor that decides how
  many hosts to use; ties break toward fewer hosts. Rejects any split where the
  network exceeds `network_time_budget` of token time. Assigns layers by
  measured memory bandwidth, not free VRAM.
- **`transport/`** — the inter-host activation channel and nothing else:
  `send_activation` / `recv_activation` behind a `Transport` interface (TCP
  today; a zero-copy RDMA backend slots in unchanged). A **binary frame** (never
  `pickle`), every field validated before the payload is returned, an **HMAC
  challenge-response** on every connection, and a length prefix bounded by
  `max_activation_mb`.
- **`coordinator/`** — the master. Runs the plan, owns one channel per stage,
  drives the autoregressive loop, and re-shards onto a fallback plan when a
  worker disappears. Ships with `MockBackend` and an end-to-end test.
- **`leak-guard/`** — the gate that keeps the repo publishable: a pre-commit +
  CI scanner for secrets and private infrastructure, plus
  anti-deserialisation checks.

## The KPI: transparent to the network
Success is **not** "it fits" — it is that the bottleneck is GPU compute, not the
wire. The coordinator measures, per token, how much time went to the network and
pipeline wait versus compute; the partitioner refuses any plan whose network
fraction would exceed ~10–15 %. If the network dominates, the stages were sized
too small for the boundary overhead — use fewer, larger stages.

## Fault tolerance
Nodes join and leave (an elastic node may vanish mid-forward). The partitioner
precomputes a best plan **excluding each node**; when the transport reports a
broken boundary, the coordinator re-shards onto that fallback instead of taking
the whole model down.

## v0 vs v1 (see ROADMAP.md)
v0 uses a **star** topology: the master round-trips the activation to each stage
in turn. It is the simplest thing that is correct and testable on one machine.
v1 chains workers directly (worker → worker, master only at the ends) to halve
the boundary crossings — the `Transport` interface already supports it.
