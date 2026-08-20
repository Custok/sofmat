"""End-to-end integration test for the sofmat coordinator.

Runs a real 2-stage pipeline on loopback with the REAL partitioner and the REAL
authenticated binary transport, using the dependency-free MockBackend. Proves
the whole wiring — partition -> stage workers -> transport -> reassembly — works
on one machine with no torch, no GPU and no model. This is what a "street user"
runs to confirm the install; it must stay green in CI.
"""

import os
import sys

_ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
for _p in (_ROOT, os.path.join(_ROOT, "transport"), os.path.join(_ROOT, "partitioner"), os.path.join(_ROOT, "common")):
    if _p not in sys.path:
        sys.path.insert(0, _p)

import array          # noqa: E402
import socket         # noqa: E402
import threading      # noqa: E402
import time           # noqa: E402
import unittest       # noqa: E402

import solver         # noqa: E402  partitioner/solver.py
from coordinator import Coordinator, MockBackend, StageWorker  # noqa: E402


def _free_port() -> int:
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    port = s.getsockname()[1]
    s.close()
    return port


class TestPipelineEndToEnd(unittest.TestCase):
    def test_two_stage_mock_pipeline(self):
        HID = 8
        TOKEN = b"0123456789abcdef"  # 16-byte shared secret

        # A 4-layer model whose caps force a 2+2 split across two nodes:
        # 0.5 GB/layer; each node caps 1.2 GB -> holds exactly 2 layers.
        nodes = [
            solver.NodeProfile("n0", model_mem_cap_gb=1.2, ms_per_layer=1.0),
            solver.NodeProfile("n1", model_mem_cap_gb=1.2, ms_per_layer=1.0),
        ]
        model = solver.ModelSpec(n_layers=4, weights_gb=2.0, kv_cache_gb=0.0)
        plan = solver.solve(nodes, model, boundary_overhead_ms=0.1).plan
        self.assertEqual(len(plan.stages), 2, "caps should force a 2-stage split")

        # One worker per stage on loopback.
        endpoints, threads = {}, []
        for st in plan.stages:
            port = _free_port()
            endpoints[st.node_id] = ("127.0.0.1", port)
            worker = StageWorker(MockBackend(hidden_size=HID))
            th = threading.Thread(
                target=worker.serve,
                args=("127.0.0.1", port, TOKEN, st.first_layer, st.n_layers),
                daemon=True,
            )
            th.start()
            threads.append(th)
        time.sleep(0.4)  # let workers bind + listen

        hidden = array.array("f", [0.0] * HID).tobytes()
        with Coordinator(plan, endpoints, TOKEN) as coord:
            out, metrics = coord.forward_token(hidden, (HID,), token_index=0)

        got = array.array("f")
        got.frombytes(out)
        # Each layer adds 1.0; 4 layers total, regardless of how they split.
        self.assertEqual(list(got), [4.0] * HID)
        self.assertGreaterEqual(metrics.total_ms, 0.0)

    def test_auth_rejects_wrong_token(self):
        port = _free_port()
        worker = StageWorker(MockBackend(hidden_size=4))
        th = threading.Thread(
            target=worker.serve,
            args=("127.0.0.1", port, b"the-right-token!!", 0, 1),
            daemon=True,
        )
        th.start()
        time.sleep(0.3)

        nodes = [solver.NodeProfile("n0", model_mem_cap_gb=100.0, ms_per_layer=1.0)]
        model = solver.ModelSpec(n_layers=1, weights_gb=1.0, kv_cache_gb=0.0)
        plan = solver.solve(nodes, model, boundary_overhead_ms=0.1).plan

        coord = Coordinator(plan, {"n0": ("127.0.0.1", port)}, b"the-WRONG-token!!")
        with self.assertRaises(Exception):
            coord.connect()          # handshake must fail with the wrong token
        coord.close()


if __name__ == "__main__":
    unittest.main(verbosity=2)
