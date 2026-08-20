# runtime — per-host stage worker

Owner: `node-b`. A worker runs **one pipeline stage**: a contiguous range of
transformer layers plus its KV-cache. The master (coordinator) drives the
autoregressive loop; the worker receives an activation, runs its layers, and
passes the result on.

## Run it now (no GPU, no dependencies)

```bash
python3 runtime/demo.py                 # see it run end-to-end
python3 -m unittest test_runtime test_reference   # from runtime/, 26 tests
```

`demo.py` runs a toy model two ways — single host, and split across three
pipeline stages with the activation serialised to bytes at each boundary like
the real transport — and shows they produce the **identical** result. That
equality is pipeline-parallel capacity pooling, demonstrated with zero GPUs.

## Pieces

- **`worker.py`** — `StageWorker` + `StageSpec` + `Activation` + `StepTelemetry`.
  Validates every incoming activation (rank / hidden dim / seq_len / dtype)
  **before** compute, times compute vs network vs wait per step, and never
  loads weights from anything but the config-declared local path.
- **`reference_backend.py`** — the CPU reference backend + `run_partition`. A
  deterministic per-layer transform of a hidden vector: applying layers
  `[0..L)` in one worker equals applying them split across stages, so a correct
  partition reproduces single-host exactly and a broken pipeline does not. Lets
  the WHOLE system run end-to-end on any machine. Serialises to bytes (no
  pickle, A08) for the transport.
- **`microbench.py`** — measures this node's real **`ms_per_layer`** and emits
  an anonymous profile the partitioner consumes (`NodeProfile.ms_per_layer`,
  measured > declared). Invariant 3: measured, not catalogue.
- **`test_runtime.py`** (14) + **`test_reference.py`** (12) — stdlib unittest,
  deterministic (fake clock, no sleeps), no GPU.

## Backends (the `LayerExecutor` protocol)

- `RefCpuExecutor` — real, deterministic, GPU-free: the end-to-end reference.
- `StubLayerExecutor` — structural pass-through for the plumbing/telemetry
  tests (timing comes from an injected clock, not the stub).
- Real torch backend (to land): same protocol, loads the stage's layers with
  `weights_only=True` from `StageSpec.model_path`, keeps the KV-cache. Plugs in
  without touching the worker, the tests, or the coordinator.

## Security (OWASP, this module's share)

- **A08** — weights load only from the validated config path; activations
  crossing the wire are metadata-validated here and byte-validated by the
  transport's framing (no pickle, ever).
- **A04** — a malformed activation raises `WorkerError` and drops the step; it
  never reaches the GPU and never crashes the worker.

## Interfaces

- **← transport (`node-c`)**: hands the worker a received `Activation`
  (zero-copy buffer) plus the measured `recv_ms`; the worker's `send` callback
  is the transport's `send_activation`.
- **→ partitioner (`node-d`)**: `microbench` output feeds `ms_per_layer`.
- **→ coordinator (`node-a`)**: `StepTelemetry` (compute / network / wait) is
  the raw material of the network-transparency KPI.

## Node-b note

This worker is written to run first on the most unusual kind of node — one with
unified CPU/GPU memory and a memory bandwidth far below a discrete GPU — so the
partitioner has a real heterogeneous stress case from day one. The
unified-memory cap (model budget < total memory) is a config field, not a
special case.
