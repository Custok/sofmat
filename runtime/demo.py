#!/usr/bin/env python3
"""sofmat runtime — end-to-end demo on the reference CPU backend (no GPU).

What a newcomer runs to SEE sofmat work after cloning:

    python3 runtime/demo.py

It runs a small toy "model" two ways — as a single host, and split across three
pipeline stages (as if on three machines) with the activation serialised to
bytes at each boundary exactly like the real transport — and shows they produce
the identical result. That equality is the whole idea of pipeline-parallel
capacity pooling, demonstrated with zero GPUs and zero dependencies.
"""

from __future__ import annotations

import os
import sys

sys.path.insert(0, os.path.dirname(__file__))

from reference_backend import run_reference_model, run_partition, hidden_to_bytes
from worker import StageSpec


def _checksum(hidden) -> str:
    import hashlib
    return hashlib.sha256(hidden_to_bytes(hidden)).hexdigest()[:16]


def main() -> int:
    hidden_dim, n_layers, seed = 16, 32, 1

    # 1) ground truth: all layers on one host.
    truth = run_reference_model(hidden_dim, n_layers, seed=seed)

    # 2) split across three logical hosts (uneven, like a real heterogeneous
    #    pool: a small stage, a big one, a small one), bytes on every boundary.
    cuts = [0, 4, 28, 32]
    specs = [
        StageSpec(node_id=f"node-{chr(ord('a') + i)}",
                  first_layer=cuts[i], n_layers=cuts[i + 1] - cuts[i],
                  hidden_dim=hidden_dim)
        for i in range(len(cuts) - 1)
    ]
    distributed = run_partition(specs, seed=seed, through_bytes=True)

    print("sofmat runtime — end-to-end reference demo (no GPU)\n")
    print(f"  model: {n_layers} layers, hidden_dim={hidden_dim}")
    print(f"  single host        -> checksum {_checksum(truth)}")
    print("  split 3 stages     -> " + " | ".join(
        f"{s.node_id}[{s.first_layer}:{s.last_layer + 1}]" for s in specs))
    print(f"  distributed result -> checksum {_checksum(distributed)}")

    match = _checksum(truth) == _checksum(distributed)
    print("\n  " + ("✅ MATCH — the split pipeline reproduces single-host exactly."
                    if match else
                    "❌ MISMATCH — pipeline is broken."))
    return 0 if match else 1


if __name__ == "__main__":
    raise SystemExit(main())
