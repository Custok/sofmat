"""sofmat coordinator (node-a's module): the master that ties the pool together.

Public surface:
  * ``StageBackend`` / ``MockBackend`` — the per-stage forward-pass interface
    and a dependency-free stand-in for tests.
  * ``Coordinator`` — the master: runs the partitioner's plan across the stage
    workers over the authenticated binary transport, token by token.
  * ``StageWorker`` — the per-node runner that serves one ``StageBackend``.
  * ``StageFailure`` / ``TokenMetrics`` — failure signal for re-sharding and the
    per-token KPI (network vs compute).
"""

from .backend import MockBackend, StageBackend
from .pipeline import Coordinator, StageFailure, StageWorker, TokenMetrics

__all__ = [
    "StageBackend",
    "MockBackend",
    "Coordinator",
    "StageWorker",
    "StageFailure",
    "TokenMetrics",
]
