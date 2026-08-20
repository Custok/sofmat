# sofmat — the Speculative Wave Pipeline (killing the latency tax)

> **Design of record (draft for consensus).** The invention: operate a
> distributed pipeline so a single stream does NOT pay the per-token network
> latency. Reframe: **latency-bound → throughput-bound**, with **zero changes to
> the model weights** — pure orchestration.
>
> Synthesized by the coordinator lane from three sections:
> [`low-latency-invention-scheduler.md`](low-latency-invention-scheduler.md)
> (the brain), [`low-latency-invention-transport.md`](low-latency-invention-transport.md)
> (the engine), and this document's choreography + draft-source matrix.
> Public repo — anonymous node labels only, no infra values.

## 1. The problem

Single-stream autoregressive decode over a pipeline is latency-bound **by
construction**: token *t+1* depends on token *t*, so exactly one stage computes
at a time and the per-hop round-trip (RTT + wakeup, measured ≈3.8 ms) sits on
the critical path of **every** token. The pipeline "bubble" (idle stages) is
inherent. More bandwidth does nothing; more nodes make it worse. Our measured
baseline: **23.7 tok/s** (Q8 weights, 150k context, 2 nodes) — good, but every
token still pays the serial hops.

## 2. The invention, in one line

**Fabricate independent work from one stream (speculation), then execute it
without serializing (overlapped transport), so the pipeline is never idle and
the per-hop latency is paid once per WAVE of ~k tokens instead of once per
token.**

## 3. Architecture — three interchangeable parts

```
  draft source  ──k candidates──▶  SCHEDULER  ──(k, tree)──▶  PIPELINE (verify in batch)
      ▲  (α, t_draft)                next_wave()               │  overlapped transport
      └──────────── refill, no bubble ◀── accept prefix / rollback on 1st reject (lossless) ◀┘
```

- **The brain — scheduler** (`next_wave(α_ema, stage_times, boundary_ms,
  t_draft) → (k, tree_shape)`): derives the optimal wave depth *k\** and tree
  shape from the measured acceptance rate α. Not tuned — **derived**:
  `cost(k) = [T_pipeline(k) + k·t_draft] / E[accepted(k, α)]`, with
  `E[accepted] ≈ (1−α^(k+1))/(1−α)`. α is content-dependent (code ~0.85–0.95,
  prose ~0.6–0.75) and measured live (EMA). See the scheduler section.
- **The engine — overlapped transport**: today's inter-node RPC is **fully
  synchronous** (verified in source: `graph_compute` blocks on the reply), so a
  wave would still serialize → no gain. The invention un-parks sofmat's own
  `Transport` (`BufferedSender`, non-blocking, per-stage double buffer): stage N
  sends candidate *t* while computing *t+1*; effective per-stage cost becomes
  `max(T_compute, T_net)`, not the sum. See the transport section.
- **The choreography — coordinator** (this lane): the wave/verify/rollback state
  machine. Ask the draft for a wave → stream candidates through the overlapped
  transport → run **rejection sampling** on the verified outputs (emitted tokens
  **identical** to non-speculative decode) → advance the accepted prefix →
  refill from the most-likely path. Never blocks.

**Why 1×2, not 1+2:** the wave supplies *independent* work; the overlapped
transport runs it *concurrently* across stages. Either mechanism alone leaves
the pipeline starved (speculation without overlap) or serialized (overlap
without speculation). Together, idle → full.

## 4. The draft source is a *parameter*, not a fixed choice

The brain and the engine are **identical** regardless of where candidates come
from — only α and t_draft change. We have **three low/zero-cost draft sources**,
and the winner is decided **by measured α**, not opinion. Crucially: **the draft
already lives inside the model — no separate model is required.**

| # | draft source | cost | expected α | note |
|---|---|---|---|---|
| 1 | **Integrated MTP head** (`nextn`, in the GGUF) | ~0 (already loaded, currently ignored) | high but **depth-1** (~0.85 first token, decays) | confirmed present: `supportsMtp=true, nextnPredictLayers=1`. Gives cheap short waves (k≈2–3); chain autoregressively (DeepSeek-MTP style) for deeper. **Blocker: does our `llama-server` build expose nextn as speculative? (upstream support is half-baked — grep to confirm.)** The head lives at the pipeline END → one extra return hop per wave; the partitioner must place it consciously. |
| 2 | **Early-exit self-speculation (PPSD)** | 0 hardware, 0 model | medium–high | the model's own early layers propose, the rest verifies; needs a calibrated confidence gate (partitioner-lane's early-exit lane). |
| 3 | **External 0.5–1.5B draft** on the free worker | 1 free GPU (already idle) | high (draft trained for it) | works **today** via `--model-draft`; a separate small model of the same tokenizer/family. |

## 5. Frontier grounding (verified 2026-08-20)

| paper | why it matters to us |
|---|---|
| **PipeInfer** (SC'24, 2407.11798) | async+continuous pipelined speculation, up to **2.15×**, explicitly designed for **low acceptance + low interconnect bandwidth** — our 10 GbE-TCP case, exactly. The blueprint for the engine. |
| **PPSD** (2509.19368) | early-exit **is** self-speculation over PP — unifies draft #2 with no extra model/GPU. |
| **FlowSpec** (2507.02620), **SpecPipe** (2504.04104) | continuous speculation over PP for distributed/edge — confirms the direction. |
| **SPD zero-bubble** (2605.30852) | 2026 state of the art: zero-bubble speculation via PP. |

The invention is not speculative research — it is *co-designing* known
speculative decoding with our pipeline. What's ours: the draft-source matrix
(esp. the integrated MTP), the derived-k\* scheduler, and the overlapped
transport for our synchronous-RPC reality.

## 6. Phased plan (ordered by "what works today")

- **Phase 1 — prove the architecture (external draft, works today).** External
  0.5–1.5B draft on the free worker via `--model-draft`; un-park the transport;
  build the wave/verify/rollback coordinator. Measure α, k\*, effective tok/s.
  **Success = 23.7 → ~50+ single-stream.**
- **Phase 2 — make it free/elegant.** Swap the external draft for the
  **integrated MTP head** and/or **early-exit self-speculation** — same
  scheduler, only α changes. They can compose (MTP as the draft's draft). Gated
  on the MTP-build-support grep.

## 7. Expected gain (to measure, not promise)

With T_traversal ≈ 46 ms and α ≈ 0.7: **k\* ≈ 4–6 → ~2.3–2.8× → 23.7 → ~55–65
tok/s**, single-stream, **keeping 150k context, zero weight change**. With α ≈
0.9 (code): k\* ≈ 8–10, **>3×**. Phase 2 can make this cost ~0 extra hardware.

## 8. How the panel proves it

The live panel already surfaces per-stage `barrier_ms` / `wait%` — the exact
signal. **"Wait% collapses under the wave"** is the visible proof the pipeline
went idle → full. The throughput readout should become a **curve tok/s(fill)**
(decode slows at high context because attention cost grows with position — a
finding from measuring the full-150k case), not a single number.

## 9. Honest risks

- The real speedup depends on α and on batch verification being genuinely
  sublinear in *k* on our engine — **Phase 1 measures both.**
- Un-parking the transport is a **real lift** (sofmat stops being a thin gateway
  and moves tensors itself on the speculative path). This is the invention, not
  a flag.
- Batch verification / rejection sampling must be exact — a bug changes outputs.
- MTP as a free draft depends on build support (grep pending).

## 10. Lanes

Brain (scheduler / k\* / tree / early-exit / routing) = partitioner lane ·
Engine (overlapped transport / draft-server) = transport lane · Orchestration
(gateway executes the policy) + measurement (α, tok/s, wait%) = gateway lane ·
Choreography + synthesis = coordinator lane. Everything is orchestration — the
model weights are never touched.

## 11. Consensus refinements (v0.2)

- **The integrated MTP head is a FLAG, not custom C-API** (corrected: the
  earlier "C-API only" verdict was from an old checkout). Our compiled
  `llama-server` exposes **`--spec-type draft-mtp`** (a complete implementation —
  chain-heads, nextn-layer-offset, `llama_get_embeddings_nextn` — not a stub),
  alongside `draft-eagle3 / dflash / dspark / ngram-*`. So Phase 2 (integrated
  draft) is **also just a flag**, like Phase 1 — no un-park, no C-API. **The
  draft lives INSIDE the pooled model → the free worker is not needed as a
  draft-server, and there is no tokenizer-mismatch risk (same model = same
  vocab).** Limit: `nextn=1` caps the MTP wave at depth ~1 (short waves — great
  for prose/low-α; deep waves for code still want a same-sub-family external
  draft or EAGLE). **RPC × speculative compose** (`ctx_tgt`/`ctx_dft` are
  independent; no anti-RPC guard) — the distributed target is transparent to the
  speculative loop. Net: the whole invention's Phase-1/2 first numbers are
  reachable by relaunch flags, not weeks of engine work; the un-parked transport
  becomes an *optimization* if the native speculative overlaps poorly with the
  synchronous RPC, not a prerequisite.
- **Co-location constraint — first-class in the map: `colocate(nextn, lm_head,
  sampler)`.** Chaining the MTP head for k>1 runs, per draft token,
  hidden→nextn→head→sample→re-embed; if these span nodes each draft token pays
  hops and the wave dies. Fix: **invert the stage order** so the master node
  hosts the model's TAIL (final layers + nextn + head + sampler). The draft loop
  and the rollback become **100% local to the coordinator**; verification
  traversal keeps the same hop count (it's a cycle).
- **Measured-by-the-model α (starting estimate, real α_ema decides):** code
  0.85–0.95 · technical/factual 0.75–0.85 · prose/creative 0.60–0.75 → code
  k\*≈8–10, **>3× (→ ~70+ tok/s)**; prose → short waves. Confirms per-context
  dynamic k.
- **Spike (exploratory, not core) — "Ghost Layer":** replicate the boundary
  layer on both sides and compute it redundantly (~5% extra FLOPs) so the RPC
  becomes an async confirmation off the critical path. Pairs with the overlapped
  transport; evaluate after Phase 1.
- **Phase-1 draft model:** Qwen3-0.6B (minimal t_draft, latency-bound-friendly)
  as primary, Qwen3-1.7B staged for the α A/B — same tokenizer family as the
  pooled model (mandatory). The scheduler's argmin picks between fast-low-α and
  slow-high-α.
- **The free worker is freed** by the C-API path (draft inside the pooled model)
  — reassign to batch throughput, a second tenant, or the kernel A/B bench.

## 12. Phase-2 roadmap (measured, correctly labelled)

The ×2.78 single-stream result sits at the **simple-speculation ceiling
(~66–75 tok/s, compute-bound)**: per-wave time ≈ T_fixed(~18 ms; RTT is only
~7.6 of it) + k·t_verify(~9 ms/token), plus a **serial draft ~12 ms/token**
(nextn→head→sample→re-embed, chained, local to the master). Two **distinct**
further multipliers — do not conflate them:

- **Single-stream (one conversation faster) — CONTINUOUS speculation** (the
  original "refill, no bubble"): draft wave *t+1* **while** verifying wave *t*,
  hiding the serial draft behind the in-flight verification traversal
  (per-wave cost sum()→max()). Estimate **~66 → 90–110 in code**. Pieces:
  scheduler+tree (partitioner) + overlapped transport as an **enabler**
  (transport) + wave/rollback choreography (coordinator). **Weeks**;
  correctness-tricky (drafting before acceptance is known → costlier rollback).
- **Capacity (serve more streams) — continuous batching (F1.5):** amortizes the
  irreducible per-token verification compute **across** streams. Does not speed
  a single conversation; multiplies total throughput.
- **Overlapped transport ALONE ≈ +12%** (hides only the fixed RTT, which does
  not scale with k) — an optimization, **not** the ×4 lever. Ultimate
  single-stream ceiling = the 27B verification forward itself
  (kernels / quant / more GPU).
- **The cheap measurement that orders all of this:** a per-wave breakdown of
  **draft-serial-ms / verify-traversal-ms / network-ms**. It says which axis
  pays and whether the transport's +12% is worth building. Do NOT build the
  transport engine on an unmeasured "×4" attribution.
- **EAGLE-3** (parallel draft in one pass — would attack the serial draft) is
  **unavailable** for this vocab (no `qwen35` EAGLE head exists); blocked unless
  one is trained.

## 13. F2 — Continuous speculation (design of record)

Synthesized from [`continuous-speculation-motor.md`](continuous-speculation-motor.md)
(motor/transport) + [`kv-handoff-desagregado.md`](kv-handoff-desagregado.md); all
figures are wall-clock (`completion_tokens/clock`), never `predicted_per_second`
(speculation confounds it).

**Where we start (measured):** the discrete MTP wave (`--spec-type draft-mtp`)
gives, on the distributed pipeline, an honest baseline of **~63 code / 47
technical / 38 prose tok/s** (×2.5–2.7 over ~24). `n_max` is flat past ~3 — the
wall isn't wave depth, it's the **verification traversal** + the MTP's **serial
draft** (~12 ms/token, chained on the same GPU as verification → it does not
overlap).

**The reframe.** The discrete wave's wall is that draft and verification
**serialize on the same hardware**. Continuous speculation runs the **draft
always ahead, on a SEPARATE node**, so wave *t+1* is drafted *while* the pipeline
verifies wave *t* → the draft cost disappears from the path (hidden behind the
verification traversal). Per-wave cost goes from `T_verif + k·t_draft` to
`T_verif`.

**Why the free node (not the integrated MTP) — with data.** The integrated MTP
lives inside the pooled model → its draft serializes on the tail GPU; it cannot
be moved off-node. A small **external draft on the free node** *does* overlap:
measured on a dedicated GPU, **255 tok/s = 3.9 ms/token, ~4× ahead of the
pipeline's ~15.9 ms/token consumption** → the draft stays comfortably ahead,
never the bottleneck. **α is NOT the risk** (it was the doubt): the small
external draft *ties* the co-trained MTP on acceptance (code 0.948 vs 0.98,
prose 0.389 vs 0.394, measured); the free-node advantage is the **overlap**, not
a better α. Cost: the free node is spent as a draft-server. Mandatory: GGUF
header-check (sub-family vocab) before staging — firm project rule.

**The two knobs** (from the scheduler; the motor consumes them). In continuous
mode the discrete wave dissolves into two α_ema-adaptive knobs:
- **c\*** — verification chunk size (candidates per micro-injection), argmin
  against *occupancy* cost, not full-traversal cost.
- **L\*** — the draft's lead window (how far ahead before it pauses). Everything
  drafted past the first reject is garbage → `L* ≈ 1/(1−α) + slack` (code
  L*≈8–10, prose L*≈2–3). The objective shifts from "minimize per-wave cost" to
  **minimize wasted work with the pipeline never idle**.

**What the motor adds (transport lane).**
1. **Overlapped wave transport** (`BufferedSender`, already implemented + parked):
   stream wave *t* to verify *while* the free node drafts *t+1*. Here the overlap
   pays fully (in the discrete wave it was only +12%, because there the wall was
   compute; here it hides the whole draft).
2. **Cancellation transport (new — the critical piece):** on the first reject,
   all in-flight draft past the rejected token is garbage → the cancel message
   must **overtake the in-flight work** (out-of-band, prioritized over the
   candidate stream). Without it the free node wastes cycles drafting a doomed
   branch. This is the difference between efficient-continuous and
   wasteful-continuous.
3. **Draft↔verify↔rollback choreography** (with the scheduler): candidate tree +
   rollback on reject. Lossless (output identical to non-speculative decode).

**Honest ceiling (measure before promising).** With the draft hidden, ceiling ≈
pure verification traversal = `T_pipeline / E[accepted]`. Current bracket:
**85–155 tok/s in code** — wide because `T_pipeline` and `t_draft_MTP` are still
*confounded* (only their per-wave sum is observed). **Closes for free in the
prototype** (the external draft runs there by clock → decouples `T_pipeline`,
without touching production). **NOT ×4 — realistic ~×2–2.5 over the ~63 baseline
in code; in prose continuous adds little** (α≈0.40 → run ~1.6, the verification
ceiling sits near today's). It is a **code / structured** lever, not a prose one.
**Weeks of build** (cancellation transport + tree/rollback + choreography), not a
flag.

**Order (measure → decide → build).** Baseline ✓ · α_ext ✓ (tied) · t_draft_ext
✓ (3.9 ms) → GO to the free-node design. **The minimal prototype closes the
ceiling bracket BEFORE committing the weeks** — if the cleared ceiling justifies
them, build the cancellation transport + choreography + tree; if not (ceiling
near the low ~85 and baseline already ~63), continuous is marginal and stays
documented, unbuilt. **The prototype decides, not the promise.**
