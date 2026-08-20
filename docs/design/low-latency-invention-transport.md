# Low-latency invention — overlapped transport + speculative-wave (node-c draft)

> Section owner: transport lane. Feeds the synthesis in `low-latency-invention.md`.
> Anonymous node labels only; no infra values.

## The problem, precisely
Single-stream autoregressive decode over a pipeline is **latency-bound by
construction**: token *t+1* depends on token *t*, so exactly one stage computes
at a time and the per-hop round-trip (RTT + software wakeup, measured ~3.8 ms)
sits on the critical path of **every** token. The pipeline "bubble" (idle
stages) is inherent. More bandwidth does nothing; more nodes make it worse.

## The reframe
**Turn latency-bound into throughput-bound by fabricating independent work from
one stream, then executing it without serializing.** Two mechanisms, and the
gain only appears when BOTH are present:

### 1. Speculative wave (draft on the free node)
A small draft model (0.5–1.5B, same tokenizer/family) runs **locally on the
otherwise-idle node**, generating a wave of *k* candidate tokens with **zero
network**. The large pooled model then processes the wave. Correctness is
preserved by rejection sampling — the emitted tokens are **identical** to
non-speculative decode; speculation only changes *how many* pipeline traversals
we pay per accepted token.

### 2. Overlapped transport (un-park sofmat's own transport)
The current engine's inter-node RPC is **fully synchronous** (verified in
source: `graph_compute` blocks on the reply; every `*_async` hook is NULL). So
today a wave would still be *serialized* — no gain. The invention needs
sofmat's **own** transport (the parked `transport/` module): `send_activation`
is non-blocking with a per-stage double buffer, so **stage N sends candidate t
while already computing candidate t+1**. Effective per-stage cost becomes
`max(T_compute, T_net)` instead of the sum.

### Why 1×2, not 1+2
The wave supplies *independent work* (different candidate tokens have no
data-dependency until verification); the overlapped transport lets the stages
run that work **concurrently** — stage N verifying candidate t while stage N+1
verifies candidate t-1, like a CPU instruction pipeline. The pipeline goes from
**idle (latency-bound) to full (throughput-bound)**, and the ~3.8 ms/hop is
**amortized across the whole wave** instead of paid per token. Either mechanism
alone leaves the pipeline starved or serialized.

## Rough upside (to be measured, not promised)
Today: ~3.8 ms/hop × 2 network hops = ~7.6 ms/token on the critical path.
- Wave k=4, ~70% acceptance ⇒ ~2.8 accepted tokens per pipeline traversal ⇒
  hop cost per accepted token ~2.7 ms.
- Overlap removes the *blocking* portion of each hop from the compute path.
- Combined, a plausible **2–3× single-stream** on our latency-bound regime —
  the first lever that raises single-stream tok/s without touching weights.

## Concrete design (my deliverables)
- **Draft server on the free node** (`node-c`): a standalone `llama-server`
  running the small draft; exposes a "give me k continuations of this prefix"
  call. It never joins the main pipeline — its whole job is fabricating the
  wave locally.
- **Un-parked `Transport`**: the `BufferedSender` (already implemented,
  overlapped, FIFO, error-surfacing) becomes the inter-stage carrier for the
  wave, replacing the synchronous RPC on the sofmat path. Per-stage double
  buffer keeps every stage busy on the next candidate.
- **Interface to the coordinator**: coordinator asks the draft for a wave,
  streams candidates into the pipeline through the overlapped transport, and
  runs rejection sampling on the verified outputs.

## Cost / honest risks
- **Un-parking the transport is a real lift** (Fase 1+): sofmat stops being a
  thin gateway over the engine's RPC and starts moving tensors itself on the
  speculative path. Non-trivial; this is the invention, not a flag.
- **Draft compatibility**: needs a 0.5–1.5B model with the same tokenizer/family
  as the pooled model; acceptance rate decides the whole win — measure it.
- **Draft VRAM** on the free node (fits a small model easily).
- **Verification batching** must be exact (a bug here changes outputs) — pairs
  with partitioner-lane's tree/early-exit gating and gateway-lane's measurement.

## Fits the panel
The panel already shows per-stage `barrier_ms`/wait% — the exact metric that
proves the pipeline went from idle to full. "Wait% collapses under the wave" is
the visible signal that the invention works.
