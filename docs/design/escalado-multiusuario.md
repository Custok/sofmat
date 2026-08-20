# sofmat — scaling to many users (measured)

> Design of record for the multi-user frontier: from one stream to many. Two
> **distinct** axes — capacity (batching) and latency-isolation (disaggregation)
> — every claim backed by a measured number on the live pipeline. Anonymous node
> labels; no infra values. Companion: [`kv-handoff-desagregado.md`](kv-handoff-desagregado.md).

## 0. The filter that shaped this

A brainstorm (from the served model itself) proposed aggressive **network**
hacks — XDP/DPDK, io_uring, delta-KV, clock-skew prefetch. We **discarded them**,
because we had **measured** that single-stream is **compute-bound** (~9 ms/token
verification; the network is only ~7.6 ms of an 18 ms fixed cost and does not
scale with wave depth → eliminating it entirely is **+12% at most**). Attacking
the network chases a bottleneck that is not dominant. Lesson: measure before you
build. The two ideas that survived the filter are compute/latency-isolation
plays, below.

## 1. Continuous batching — capacity, already active (~1.9×)

llama-server already runs continuous batching over the distributed pipeline.
Measured (draft-mtp on, live):

| concurrency | aggregate tok/s | per-stream |
|---|---|---|
| 1 | 46.8 | 46.8 |
| 2 | 75.9 | 37.9 |
| 4 | **87.7** | 21.9 |

**~1.9× capacity at 4 slots, free.** Sublinear (the compute-bound verification
is shared across streams); per-stream degrades. **The speculative wave COEXISTS
with parallel slots** (`draft-mtp` stays active at N=4 — no ola-vs-batching
trade-off). Tuning `-np` / `--cont-batching` in a relaunch may push the
aggregate further; the base is already ~2×.

## 2. Prefix caching — the multi-user compute saver (next cheap win)

Decode is compute-bound; the cheapest way to save compute across users is to
**not recompute a shared prompt prefix**. Measured: llama-server already reuses a
prefix within a slot (1395 tokens cached, **−44% prefill** on the 2nd request).
Two regimes:

- **v1 — slot-affinity (gateway policy, no engine, no KV bytes moved):** the
  gateway hashes each request's **stable prefix** (system prompt / tenant header)
  and routes same-prefix requests to the **same slot** → the per-slot prompt
  cache reuses the prefix by itself. Composes with continuous batching. The
  business case is direct: many tenants sharing one system prompt → the common
  prefill is paid **once**. **Implementable this week (gateway v0).**
- **v2 — distributed shared cache (with transport):** KV keyed by prefix hash
  (`llama_state_seq_get_data_ext` blob); on a cache hit a client sends an 8-byte
  id instead of MB of KV; on a miss the transport fetches the blob. Composes with
  the P/D split (the prefill node checks the cache before computing).

## 3. Disaggregated serving (prefill / decode) — the differentiator

Prefill (process the whole prompt, one big parallel pass, **compute-bound**) and
decode (one token at a time, **bandwidth-bound**) have opposite profiles.
Co-located, a long prefill **starves** every active decode. Measured:

- Prefill: **1100–1350 tok/s** (20–90× decode); a **32k prompt = 18.6 s** of
  prefill.
- **Interference: a decode running under a 32k prefill is ×8.5 slower** (TTFT
  ×1.4, wall ×8.5 — the prefill hogs the pipeline ~18.6 s). **Severe when it
  happens.**

**Whether to build it is a FREQUENCY question, not a magnitude one:** with a
single operator + the reviewer model, interference is sporadic → **today it is
over-engineering.** As a product with several tenants hitting the same API
(peritos, other in-house consumers), it is frequent → **disaggregation is the
v2 differentiator** (llama.cpp does not disaggregate across machines; sofmat
would). Mechanism + scatter design: [`kv-handoff-desagregado.md`](kv-handoff-desagregado.md).
The **KV handoff (0.5–10 GB, bandwidth-bound) is where the overlapped transport
earns its real ×** — opposite profile to the 10 KB latency-bound wave.

- **Prefill node candidate:** the unified-memory host (compute-friendly, large
  memory for big-prompt KVs, zero interference with the decode pipeline); the
  free worker as a second/alternative prefill node.
- **Placement is another dimension of the partitioner's map** (node → role →
  layers), decided by the same expected-cost objective.

## 4. Speculative Prefill — F2 of disaggregation

The served model's best idea, and the wave's philosophy applied to the handoff:
the decode side starts on a **draft of the prompt** while the real KV
(0.4–8 s transfer) arrives in the background over the overlapped transport, then
reconciles. Hides the handoff latency. Caveat (partitioner): approximate state →
a reconcile-rollback, costlier than the token wave, so it goes **after** the
blocking handoff works, not before. Design-worthy; build later.

## 5. Phasing & lanes

- **Now (free / this week):** batching ~1.9× is live; **prefix-cache v1 =
  slot-affinity** in the gateway (gateway-lane) once gateway v0 is greenlit.
- **Spec now, build on real load:** disaggregation (partitioner role-map =
  partitioner-lane; KV scatter transport = transport-lane; gateway admission + prefill→decode
  routing = gateway-lane; synthesis/choreography = coordinator).
- **Later:** speculative prefill; distributed prefix cache v2.

Everything is orchestration over llama.cpp; the model weights are never touched.
Network-stack hacks are **out** — measured to be a +12% ceiling on a non-dominant
bottleneck.
