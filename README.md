# sofmat

![sofmat — pool the VRAM of several machines to run one model that fits on none of them](assets/banner.png)

**Load one big model across several machines' GPUs and serve it behind a single OpenAI-compatible API — like LM Studio, but the model is split across your whole cluster.**

Point any OpenAI client at one endpoint; sofmat runs the model in pipeline across every GPU in your cluster. It's for models too big for any single machine, on the plain 10 GbE Ethernet you already have — no InfiniBand, no RDMA.

> Early work-in-progress. Apache-2.0. Built in the open.

sofmat runs one large language model across a cluster of ordinary, *mismatched* machines (different GPUs, different amounts of memory, consumer 10 GbE, TCP — no InfiniBand, no RDMA). The goal is **capacity** — running models too big for a single host — while keeping the network off the critical path so it stays fast.

## Why
A single consumer GPU can't hold a 100B+ model. Buying datacenter interconnect isn't an option for most people. sofmat treats a handful of everyday PCs as one pool of VRAM, connected by the Ethernet you already have.

## What sofmat is (and isn't)
sofmat is the **orchestration layer**, not a new inference engine. The actual
layer math is done by a proven engine (**llama.cpp**); sofmat is what drives it
across a cluster and adds everything that engine doesn't do on its own:
memory-bandwidth-aware partitioning, **serve-the-model-once** (each node fetches
only its layers, authenticated), elastic membership with failover, authenticated
transport, and network-vs-compute instrumentation. On one node it's just
llama.cpp; sofmat is the part that makes several mismatched machines act as one.

The distributed-systems core — partitioner, transport, coordinator, served-weights
loader — is built and tested. A mock backend runs the whole pipeline end to end
**with no cluster**, so you can check the plumbing on a laptop; the thin adapter
that drives llama.cpp per stage is what lights up real multi-GPU inference.

```bash
git clone https://github.com/Custok/sofmat.git && cd sofmat
python coordinator/test_pipeline.py   # end to end: partition -> stages -> transport -> reassembly
python partitioner/test_solver.py     # which layers land on which GPU
python transport/test_transport.py    # binary framing + HMAC auth over TCP
```

Python 3.10+. A real multi-GPU run needs torch on each node and your own model:
copy `config.example.yaml` to `config.local.yaml` (git-ignored), describe your
cluster, and see [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md).

## How it works
- **Pipeline-parallel across hosts, tensor-parallel within a host.** Only the per-token activation crosses the network between pipeline stages (tens of KB), so plain 10 GbE is enough. All-reduce-heavy tensor parallelism stays *inside* a machine.
- **Master + workers.** A lightweight coordinator drives the forward pass, sampling and the autoregressive loop; each worker owns a contiguous range of layers.
- **Memory-bandwidth-aware partitioning.** Layers are assigned by each host's memory bandwidth (not just free VRAM), with VRAM + KV-cache as a hard constraint. A big-but-slow node gets fewer layers.
- **Network-transparent by design.** Success = the bottleneck is GPU compute, not the wire or the pipeline bubble (target: network+wait < ~15% of per-token time).
- **Fault-tolerant.** Nodes may join and leave; a worker that vanishes mid-forward triggers re-partitioning instead of taking the whole model down.

## Design principles
- **Config-driven, infra-agnostic.** You describe *your* cluster; nothing about any specific deployment is baked in. See `config.example.yaml`.
- **Security first (OWASP Top 10).** Authenticated master↔worker transport, binary wire framing (**never `pickle`** — deserializing untrusted data over a socket is remote code execution), strict validation of everything received before it touches the GPU.
- **Pure standard library where possible;** heavy deps (torch, gRPC) added deliberately with a lockfile.

## Repository layout
```
config.example.yaml     # copy to config.local.yaml (git-ignored) and describe your cluster
transport/              # inter-host tensor transport (binary framing, HMAC auth, TCP; RDMA-ready interface)
partitioner/            # heterogeneous layer partitioner (fit + fastest, memory-bandwidth aware)
coordinator/            # the master: runs the plan across workers, drives the pipeline, measures the KPI
leak-guard/             # secret / private-infra scanner + OWASP anti-deserialization gate (pre-commit + CI)
docs/research/          # annotated research briefs on distributed LLM inference
```

## Status
**Done and tested:** heterogeneous layer **partitioning**, authenticated binary **transport**, the **coordinator** (pipeline forward loop + re-shard when a node is lost), and **served-weights** loading (one copy of the model; each node fetches only its own layers). **In progress — the core:** the **torch/CUDA execution backend** that puts real model layers on each GPU and turns the proven orchestration into real multi-GPU inference, then the benchmark against prima.cpp and llama.cpp RPC. Not production-ready yet — but the hard part, making many mismatched machines behave as one, already runs.

## License
Apache-2.0.
