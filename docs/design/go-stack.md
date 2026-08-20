# sofmat — Go mono-stack (structure of record)

> Decision (3/4 + confirmed): the production stack is a **single Go binary**
> end-to-end — data-plane, control-plane and the engine binding via cgo to
> `libllama`. One runtime, one build chain, one debug mode. The Python modules
> stay as a **validated prototype + executable spec** (their tests are the
> behavior contract); they are ported 1:1, not discarded. Erlang is reserved for
> a future "thousands of persistent-session tenants + hot-reload" scenario,
> re-evaluated only on data.

## Layout

```
sofmat/
  go.mod                        module github.com/Custok/sofmat
  cmd/sofmat/main.go            CLI entrypoint (sofmat serve)
  internal/
    transport/                  data-plane: framed transport + KV handoff
    engine/                     cgo binding to libllama (state_seq_get/set)
    partitioner/                role/replica solver
    gateway/                    admission + routing (policy, hash-ring)
    coordinator/                choreography / wave-verify-rollback / glue
  Makefile                      build · test · vet · tidy
  .github/workflows/go-ci.yml   go vet + go test + pure-Go build
  docs/design/                  specs (this file, kv-handoff, low-latency, ...)
  # Python prototype kept as reference/spec (tests = contract):
  transport/ · partitioner/solver.py · gateway-v0/ · common/
```

Private packages live under `internal/` so nothing leaks into an importable
public API before it is stable. Real infra (endpoints, hosts) lives only in
`config.local.*` — never committed; the repo uses anonymous node labels.

## Porting map (Python prototype → Go package, by lane)

| Lane | Python prototype (contract = its tests) | Go package | Notes |
|---|---|---|---|
| transport (transport-lane) | `transport/kv_handoff.py` (6/6) | `internal/transport` | bulk chunking, sha256 integrity, auth/anti-DoS |
| engine (transport-lane) | — (new) | `internal/engine` | **cgo** `llama_state_seq_get_data`/`set_data`; behind a build tag |
| partitioner (partitioner-lane) | `partitioner/solver.py` (34/34) | `internal/partitioner` | `SolveRoles` / replicas; isolation by construction, fail-closed |
| gateway (gateway-lane) | `gateway-v0/admission.py` (63) | `internal/gateway` | admission threshold + LRU prefixes; fail-soft to decode-direct |
| coordinator (coordinator-lane) | — (synthesis) | `internal/coordinator` | wires the planes; owns wave/verify/rollback |

**Rule:** each port must make the Go tests reproduce the Python tests 1:1 before
the Python reference is retired. `go test ./...` is the gate.

## Build & CI

- `make build` / `make test` / `make vet` — local.
- `make build-nocgo` — the pure-Go planes build without the native engine
  (transport/gateway/solver), so CI stays green before the cgo binding lands.
- CI (`go-ci.yml`): `go vet` + `go test -count=1` + the pure-Go build on every
  push/PR to `main`.
- The engine's cgo backend arrives behind a build tag; the interface
  (`engine.KVCodec`) lets the other planes compile and test without it.

## Status

Skeleton up: module, entrypoint, five packages (doc + interface + stub +
guard test), Makefile, CI. Lanes port their prototype into their package with
the tests as the contract. Leak-guard runs at the publication boundary as usual.
