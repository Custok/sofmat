# sofmat — web panel (design of record)

> **Status:** consensus draft, pending the operator's visual OK on the hero
> screen before any code. Integrated by the coordinator lane from three section
> docs: [`web-panel-agent.md`](web-panel-agent.md) (per-node telemetry),
> this document's Status-API/gateway section, and
> [`web-panel-frontend.md`](web-panel-frontend.md) (UI).
>
> **Public repo — governance:** anonymous node labels only (`node-a/b/c`). No
> hostnames, IPs, GPU models, or sizes in any payload, example, or the frontend
> bundle. Real values live only in `config.local.yaml`. `leak-guard` covers
> `web/`, `docs/`, and the agent.

## 1. What it is

One screen that shows a cluster serving a model too big for any single machine:
the served model, the pipeline of layers across nodes, per-node GPU/CPU/RAM/net
health, and the live stream of requests the model receives — like a single-box
model server's console, but for a model split across the whole cluster.

It is **read-only observability**. It does not control the run; it reflects it.

## 2. Architecture

Four pieces, each an owned lane:

```
  ┌── node-a ──┐   ┌── node-b ──┐   ┌── node-c ──┐     each node runs a
  │ sofmat-    │   │ sofmat-    │   │ sofmat-    │     sofmat-agent (NVML +
  │ agent      │   │ agent      │   │ agent      │     psutil), authenticated
  └─────┬──────┘   └─────┬──────┘   └─────┬──────┘
        │  GET /metrics (common/auth, 401 if unauth)
        └────────────────┼────────────────┘
                    ┌─────▼──────┐   polls agents + engine, aggregates
                    │  gateway   │   /props, /v1/models, partitioner map
                    │ Status API │
                    └─────┬──────┘   serves static UI + /api/* (one origin,
                          │          all authenticated, SSE for live)
                    ┌─────▼──────┐
                    │  frontend  │   Vite + React + TS, live over SSE
                    └────────────┘
```

1. **`sofmat-agent`** — per-node telemetry over an authenticated endpoint. Owns
   the honest per-node reading. Full contract: [`web-panel-agent.md`](web-panel-agent.md).
2. **Gateway / Status API** — aggregates the agents + the engine into `/api/*`,
   serves the static UI, holds auth. Section 3 below.
3. **Frontend** — the hero screen. Full contract: [`web-panel-frontend.md`](web-panel-frontend.md).
4. **Config contract** — the node list and topology come from sofmat's config;
   the panel is generic. Section 5 below.

## 3. Gateway / Status API (lane: gateway/auth)

The gateway is the single entry point. It serves the built UI **and** the API
from **one origin** (no CORS), and **every `/api/*` route is authenticated**
with `common/auth` (HMAC, `SOFMAT_AUTH_TOKEN`). A metrics feed still reveals the
cluster shape, so nothing is open on the LAN.

### `GET /api/status` — aggregated snapshot (polled)
```json
{
  "model":   {"loaded": true, "id": "<model-id>", "ctx": 25000,
              "quant": "<quant>", "size_gb": 0, "slots": 4},
  "throughput": {"decode_tok_s": 0.0, "ttft_ms": 0},
  "pipeline": {
    "total_layers": 65,
    "stages": [
      {"node": "node-a", "layers": [0, 26], "stage_ms_est": 0,
       "barrier_ms": 0.0, "kv_used_per_slot_mb": 0, "bottleneck": false}
    ],
    "fallbacks": [{"down": "node-b", "remap": "node-a+node-c", "est_tok_s": 0.0}]
  },
  "nodes": [ /* per-node hardware from each sofmat-agent (see web-panel-agent.md) */
    {"node": "node-a", "role": "master", "up": true, "connected": true,
     "gpus": [ {"idx": 0, "vram_used_mb": 0, "vram_total_mb": 0,
                "util_pct": 0, "temp_c": 0, "power_w": 0} ],
     "cpu": {"util_pct": 0}, "ram": {"used_mb": 0, "total_mb": 0},
     "net": {"rx_mbps": 0.0, "tx_mbps": 0.0, "rtt_ms": 0.0},
     "wait_pct": 0, "hop_ms": 0.0}
  ],
  "auth": {"required": true, "principal": "<who>"}
}
```

### Sourcing — who measures what
The gateway merges two independent sources; the schema fields carry different
provenance and must not be confused:
- **`sofmat-agent` (per-node hardware)** — `gpus` (util, **total** VRAM, temp,
  power), `cpu`, `ram`, `net` + `rtt_ms` per link. What a node can honestly
  read about itself via the OS/driver.
- **coordinator / runtime (the pipeline)** — `stage.barrier_ms` (from the
  per-stage compute/recv/send/wait telemetry) and `stage.kv_used_per_slot_mb`
  (the engine's KV accounting). **The agent cannot produce these**: the OS
  cannot split KV from weights inside `vram_used`, and pipeline wait is a
  runtime quantity, not a node one. `probe_boundary_overhead_ms` is the
  authoritative per-boundary latency.
- **partitioner** — `stage_ms_est` + `bottleneck`.

`wait_pct` / `hop_ms` shown on a node card are the gateway projecting the
coordinator's per-stage latency onto that node's view. The straggler is the
stage with the largest `barrier_ms`; feeding `barrier_ms / stage_layers` back
into `NodeProfile.ms_per_layer` (with hysteresis) turns partitioning from a
one-shot startup calc into **continuous rebalancing** — the straggler
self-corrects. `fallbacks` = the N−1 rescue maps, made visible.

### `GET /api/stream` — live telemetry (SSE)
Server-Sent Events: unidirectional, auto-reconnect, no handshake — the right
fit for push telemetry (WS only if the UI ever needs to control the run, which
it does not in v1). Emits `status` deltas on each poll tick.

### `GET /api/requests` — live request log (SSE)
The stream of inference requests the model receives (the operator's key ask —
"the queries the model gets", like a single-box server's dev log). One event
per request:
```json
{"ts": 0, "method": "POST", "endpoint": "/v1/chat/completions",
 "model": "<model-id>", "prompt_tokens": 0, "completion_tokens": 0,
 "tok_s": 0.0, "ttft_ms": 0, "latency_ms": 0, "status": 200}
```
- **Security:** metrics always shown; **prompt/response content is OFF by
  default** — auth-gated, truncated, behind an explicit toggle. Content is never
  written to disk without opt-in. Prompts are sensitive; the log leads with
  timings, not text.

### `POST /api/chat` — built-in playground
A small chat in the panel to talk to the served model and smoke-test the running
cluster without leaving the page. The gateway **proxies** to the engine's
`/v1/chat/completions` (authenticated; the browser never reaches the engine
directly) and streams the reply back. Read-only test surface, same auth as the
rest of `/api/*`.

## 4. The network metric is LATENCY, not bandwidth

A design decision confirmed from an operator network graph showing the cluster
link near-idle during a run: **pipeline-parallel decode is latency-bound.** Each
token ships one small activation tensor per stage boundary (~10 KB), so the link
sits at a fraction of a percent of capacity — bandwidth always looks idle and
**misleads**. The cost is the fixed per-boundary latency (the partitioner's
`boundary_overhead_ms`), paid serially per token.

Therefore:
- The panel **leads with `barrier_ms` / `ms per hop` and `wait_pct` per stage**;
  `rx/tx` bandwidth is a secondary detail (tooltip), never the headline.
- The agent should measure `net` on the **RPC-carrying NIC**, and — where
  possible — expose per-link RTT.
- Idle bandwidth = **headroom for batching**: continuous batching sends N
  sequences per hop, amortizing the per-hop latency. So the levers are
  **batching + fewer hops (quantize to fit in fewer nodes)**, not more bandwidth.

## 5. Config contract (zero hardcoded infra)

The panel is generic; the cluster is data. Nodes, endpoints, and the auth token
come from sofmat's config:
- Public repo ships `config.example.yaml` with generic `node-a/b/c` and
  `*.example.local` placeholders.
- Real hosts/IPs/GPU models live only in `config.local.yaml` (git-ignored;
  `leak-guard` blocks it by name).
- `SOFMAT_AUTH_TOKEN` from env/`config.local`, never in code or the bundle.
- **Node display names are user-editable** in the panel (inline rename, ✎),
  persisted to `config.local` as `nodes[].display_name`, defaulting to the
  logical label. Custom names never ship to the repo; the public example keeps
  `node-a/b/c`. The gateway exposes a small authenticated `PATCH` to set it.

## 6. Hero screen (for the operator's visual OK)

Interactive mock (illustrative data, anonymous labels), both themes:
**the published `sofmat panel` artifact.** ASCII reference in
[`web-panel-frontend.md`](web-panel-frontend.md). Layout: status bar → hero
(served model + latency-bound pipeline with the bottleneck and N−1 fallback
visible) → heterogeneous node grid → live request log with content off.

## 7. Deliverables once the visual is approved

- `sofmat-agent/` — agent + own lockfile (`psutil`, `pynvml`), pip-audit, tests.
- `web/` gateway routes (`/api/status`, `/api/stream`, `/api/requests`) + auth.
- `web/ui/` (Vite+React+TS): `PipelineBand`, `NodeCard`, `RequestsFeed`, SSE
  client with reconnect; static build served by the gateway.
- `docker-compose.yml`, component tests, a `/api/status` fixture (fake data),
  README with light + dark screenshots.

## 8. Open questions for the operator

1. Visual direction of the hero screen — approve, or adjust palette / density /
   which metric leads.
2. Default poll cadence for `/api/status` (proposed: 1 s).
3. Request-log retention in the UI (proposed: last N in memory, no disk).
