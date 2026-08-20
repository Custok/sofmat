# sofmat transport (node-c)

Inter-host activation channel for pipeline-parallel inference. The coordinator
(`node-a`) and stage workers (`node-b/c/d`) exchange pipeline activations
through this interface and nothing else:

```python
import auth, transport, framing

token = auth.load_token()                       # SOFMAT_TRANSPORT_TOKEN (env/config.local)

# master side (dial a worker)
t = transport.connect("node-c.example.local", 50051, token, max_activation_mb=8)
t.send_activation(header, payload)              # push my stage output downstream
h, view = t.recv_activation()                   # pull previous stage output (zero-copy view)

# worker side (accept an authenticated peer)
t = transport.accept(sock, token, max_activation_mb=8)
```

## Design (see docs/research/03 and README invariants)
- **Interface first (`Transport` ABC).** v0 backend is raw stdlib `socket` +
  a binary frame; gRPC and a future RDMA backend slot in behind the same
  interface, so the coordinator never changes when the wire does (invariant 1).
- **No `pickle` on the wire (OWASP A08).** `framing.py` is a fixed binary
  header + a contiguous tensor buffer; the payload is returned as a
  `memoryview` and rebuilt zero-copy with `torch.frombuffer(...).reshape(...)`.
- **Auth from v0 (OWASP A01).** `auth.py` does an HMAC-SHA256 challenge-response
  with a shared token (`SOFMAT_TRANSPORT_TOKEN`), constant-time verify; the
  token never crosses the wire. No open port that accepts tensors from the LAN.
- **Bounded / fail-closed (A04, DoS).** Every frame is validated before the
  payload reaches the GPU (magic, version, `ndim`, dims, and
  `prod(shape)·dtype_size == n_bytes`); the length prefix is capped by
  `max_activation_mb`.
- **Measured overhead.** `transport.probe_boundary_overhead_ms()` yields the
  per-link `boundary_overhead_ms` the partitioner's cost model consumes.

## Files
| file | what |
|---|---|
| `framing.py` | binary wire format (encode/decode activation + control frames) |
| `transport.py` | `Transport` interface + `TcpTransport` (connect/accept, bounded framed IO) + stabilized `probe_boundary_overhead_ms` (warmup + median) |
| `pipelined.py` | `BufferedSender` — overlapped (double-buffered) send so comms hide behind compute (Prima.cpp PRP); FIFO-preserving, errors re-raised, interface unchanged |
| `test_transport.py` | 15 unit tests, pure stdlib, loopback only (no infra) |

> Auth is **not** in this directory: it lives in the shared **`../common/auth.py`**
> (one auth module for the transport handshake AND the served-weights endpoint).
> `transport.py` imports it as `auth` via the same sibling-path bootstrap the
> coordinator uses.

## Overlapped send (double buffer)
```python
sender = pipelined.BufferedSender(t, depth=2)   # wraps any Transport; same send_activation()
sender.send_activation(header, payload)          # returns once queued; worker writes while you compute N+1
sender.flush(); sender.close()                   # a send error is re-raised here, never swallowed
```
`BufferedSender` is a drop-in wrapper — the coordinator integrates against the
unchanged `Transport` interface whether or not it wraps for overlap.

## Test
```
cd transport && python3 -m unittest test_transport -v   # 15 tests, no dependencies
```

## Done / pending
- ✅ v0 framing + auth + bounded TCP · ✅ **double-buffer overlapped send** · ✅ **stable boundary-overhead probe**.
- Pending: gRPC/RDMA backends behind the interface · size-gated compression on
  prefill + KV-migration paths (NOT decode hot-path) · integration with the
  coordinator's per-stage healthcheck · `pip-audit` once `torch` lands.
