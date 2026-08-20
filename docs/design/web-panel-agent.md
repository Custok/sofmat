# Web panel — `sofmat-agent` (per-node telemetry)

> Design-of-record section for the node agent. Owner lane: transport/security.
> Integrates into `docs/design/web-panel.md`. Public repo — anonymous node
> labels only, no host/infra values in any payload or example.

## Role
One small program runs on **every node** of the cluster (master and workers).
It exposes that node's live telemetry over an authenticated HTTP endpoint. The
gateway (control plane) polls each agent and aggregates the results into
`/api/status` for the UI. The agent knows nothing about other nodes — it only
reports *itself*. This keeps the panel's node view honest (each card is a real
per-node reading, not an inference) and the failure model simple (an
unreachable agent = that node's card greys out).

## Endpoint
```
GET /metrics
```
- Bind host/port from config (default port `50060`); the real bind address
  lives only in `config.local.yaml`.
- **Authenticated with `common/auth`** (OWASP A01): the caller sends
  `common/auth.request_headers(token)`; the agent verifies with
  `common/auth.verify_request(token, headers)`. **401 on missing/invalid auth**
  — never an open telemetry port on the LAN (a metrics feed still reveals the
  cluster shape). Token from `SOFMAT_AUTH_TOKEN` (env/`config.local`), never in
  code. Same shared secret as the transport and the weight endpoint.
- Returns `200 application/json` with the payload below.

## Payload (v1)
Logical node label only; no hostnames/IPs. `null` for a field the OS/driver
does not expose.
```json
{
  "node": "node-c",
  "ts": 1700000000,
  "gpus": [
    {"idx": 0, "name": "GPU model", "vram_used_mb": 0, "vram_total_mb": 0,
     "vram_free_mb": 0, "util_pct": 0, "temp_c": 0, "power_w": 0}
  ],
  "cpu":  {"util_pct": 0, "cores": 0, "load1": 0.0, "freq_mhz": 0},
  "ram":  {"used_mb": 0, "total_mb": 0, "free_mb": 0},
  "disk": {"used_gb": 0, "total_gb": 0},
  "net":  {"iface": "<rpc-nic>", "rx_mbps": 0.0, "tx_mbps": 0.0, "rtt_ms": {"<peer>": 0.0}},
  "stage": {"rpc_port": 50052, "connected": false, "layers": [0, 0]}
}
```
- `gpus[]` — one entry per GPU on the node (a node may serve several).
- `net` — **measured on the interconnect NIC that carries the pipeline traffic,
  named in config** (`net.iface`), not an aggregate of all interfaces; a
  management/desktop NIC would drown the signal.
  - `rx/tx_mbps` — **sampled** (delta of byte counters over Δt; first poll after
    start is `null`). ⚠ **Secondary metric, and it will look near-idle by
    design:** pipeline-parallel *decode* sends ~10 KB per token per boundary, so
    at ~15 tok/s the link runs at single-digit **Mb/s — <0.1 % of 10 GbE**. The
    network is **latency-bound, not bandwidth-bound**; a bandwidth gauge here
    misleads ("the network isn't doing anything").
  - `rtt_ms` — optional lightweight per-peer round-trip (TCP-connect probe) so
    the UI can show hop latency, which is what actually matters. **The
    authoritative boundary-latency figure is `probe_boundary_overhead_ms` from
    the transport (coordinator-driven), not the agent** — the agent's `rtt_ms`
    is a cheap always-on hint, the probe is the bench number.
- `stage` — this node's role in the current run: the rpc port it serves,
  whether the master currently holds an RPC session (`connected`), and the
  `[first, last]` layer range from the partitioner's map. `connected:false`
  with the process up = idle/elastic node not currently in a run.

## Dependencies (agent-only)
The core (`transport`, `common`, `partitioner`, `coordinator`) stays **pure
stdlib, zero deps**. The agent is the one component that reads whole-node
telemetry across **heterogeneous OSes** (Windows master + Linux workers), where
hand-rolled `/proc` vs WMI parsing is fragile and reviewer-unfriendly. So the
agent — and only the agent — takes two well-known deps:
- **`psutil`** — cross-platform CPU/RAM/disk/net counters.
- **`pynvml`** (NVML) — GPU telemetry without parsing `nvidia-smi` text.

Governance: these live in the agent's **own lockfile** (`sofmat-agent/requirements.lock`),
covered by `pip-audit` in CI **against that lockfile**, not the core. The core's
zero-dep security posture is unchanged. If a deployer cannot install them, the
agent degrades: absent `pynvml` → `gpus: []`; absent `psutil` → `cpu/ram/net`
fields `null`. It never crashes the node.

## Non-goals (v1)
No control actions (read-only telemetry). No cross-node awareness. No history
(the gateway/UI does retention). No content of requests (that is the gateway's
request-log, with content OFF by default).

## Tests
Pure-stdlib unit tests for the auth gate (200 vs 401), the JSON schema/shape,
the net-rate sampling (first poll `null`, second poll a rate), and graceful
degradation when psutil/pynvml are absent (mocked). Loopback only, no infra.
