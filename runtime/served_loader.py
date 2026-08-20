"""sofmat runtime — served-model loader (one copy, each worker fetches its slice).

Design decision (David, 2026-08-20): the model is deployed ONCE (on a serving
node / shared FS / object store), not copied whole to every node. Each stage
worker pulls ONLY the bytes of the tensors for ITS layer range — so a
heterogeneous cluster where some nodes have little disk still works, and adding
a node costs no extra full copy.

This module is the disk-and-network-efficient half of that: given a model's
tensor index (name -> byte offset + size) and a stage's layer range, it plans
the EXACT set of byte ranges that node must fetch, coalesces adjacent ones into
as few requests as possible, and fetches them through a pluggable ``Source``
(local file seek, or HTTP range requests against the single served copy).

Parsing the GGUF binary into a ``TensorInfo`` list is a shared concern (the
partitioner's ``tools/gguf_modelspec.py`` already reads GGUF metadata) — this
module takes the index as input so there is ONE GGUF parser in the repo, not
two. Everything here is pure stdlib and unit-testable with a synthetic index,
no GPU and no real weights.
"""

from __future__ import annotations

import re
from dataclasses import dataclass


# A transformer block's per-layer tensors are named ``blk.<N>.<...>`` in GGUF.
_BLOCK_RE = re.compile(r"^blk\.(\d+)\.")

# Tensors that belong to the whole model, not a single block. The FIRST stage
# needs the input embedding; the LAST stage needs the final norm + output head.
# Names cover the common GGUF conventions.
_EMBED_NAMES = ("token_embd.weight",)
_HEAD_NAMES = ("output_norm.weight", "output.weight", "lm_head.weight")


class LoaderError(Exception):
    """Raised on an inconsistent index or an impossible fetch plan."""


@dataclass(frozen=True)
class TensorInfo:
    """One tensor's location in the served model.

    ``shard`` names the file the tensor lives in — ``None`` for a single-file
    model, or e.g. ``"00001-of-00002"`` for a sharded GGUF. ``offset`` is
    absolute WITHIN that shard (each shard has its own header + data section),
    so byte ranges never cross a shard boundary.
    """

    name: str
    offset: int
    nbytes: int
    shard: str | None = None

    @property
    def end(self) -> int:
        return self.offset + self.nbytes

    def layer(self) -> int | None:
        """The block index if this is a per-layer tensor, else None."""
        m = _BLOCK_RE.match(self.name)
        return int(m.group(1)) if m else None


@dataclass(frozen=True)
class ByteRange:
    offset: int
    nbytes: int
    shard: str | None = None   # which shard file this range is read from

    @property
    def end(self) -> int:
        return self.offset + self.nbytes


@dataclass(frozen=True)
class FetchPlan:
    """What one stage must pull from the served copy."""

    stage_first_layer: int
    stage_n_layers: int
    tensors: tuple[TensorInfo, ...]   # the tensors this stage owns
    ranges: tuple[ByteRange, ...]     # coalesced byte ranges (each tagged with its shard)

    @property
    def total_bytes(self) -> int:
        return sum(r.nbytes for r in self.ranges)


def tensor_index_from_tables(tables: dict[str | None, list[dict]]) -> list[TensorInfo]:
    """Build a combined index from the partitioner's GGUF ``tensor_table`` output.

    ``tables`` maps a shard label (``None`` for a single file) to that shard's
    ``[{name, offset, nbytes}, ...]`` list. One GGUF parser feeds this — the
    loader never parses the binary itself.
    """
    index: list[TensorInfo] = []
    for shard, rows in tables.items():
        for r in rows:
            index.append(TensorInfo(r["name"], int(r["offset"]), int(r["nbytes"]), shard))
    return index


def _coalesce(tensors: list[TensorInfo], *, gap: int = 0) -> list[ByteRange]:
    """Merge tensors whose byte ranges touch (or sit within ``gap`` bytes) into
    single requests — fewer round trips. Coalescing happens WITHIN each shard;
    ranges never span two files.
    """
    by_shard: dict[str | None, list[TensorInfo]] = {}
    for t in tensors:
        by_shard.setdefault(t.shard, []).append(t)

    ranges: list[ByteRange] = []
    for shard, group in by_shard.items():
        ordered = sorted(group, key=lambda t: t.offset)
        cur_start, cur_end = ordered[0].offset, ordered[0].end
        for t in ordered[1:]:
            if t.offset <= cur_end + gap:
                cur_end = max(cur_end, t.end)
            else:
                ranges.append(ByteRange(cur_start, cur_end - cur_start, shard))
                cur_start, cur_end = t.offset, t.end
        ranges.append(ByteRange(cur_start, cur_end - cur_start, shard))
    return ranges


def plan_stage_fetch(
    index: list[TensorInfo],
    first_layer: int,
    n_layers: int,
    n_total_layers: int,
    *,
    coalesce_gap: int = 0,
) -> FetchPlan:
    """Byte ranges a stage owning ``[first_layer, first_layer+n_layers)`` must
    fetch from the served model.

    Includes: every ``blk.N.*`` tensor with N in the stage's range; the input
    embedding IF this is the first stage (first_layer == 0); the final norm +
    output head IF this is the last stage (covers the last model layer).
    """
    if n_layers < 1 or first_layer < 0:
        raise LoaderError(f"bad stage range first={first_layer} n={n_layers}")
    last_layer = first_layer + n_layers - 1
    is_first = first_layer == 0
    is_last = last_layer == n_total_layers - 1

    picked: list[TensorInfo] = []
    for t in index:
        li = t.layer()
        if li is not None:
            if first_layer <= li <= last_layer:
                picked.append(t)
        elif t.name in _EMBED_NAMES:
            if is_first:
                picked.append(t)
        elif t.name in _HEAD_NAMES:
            if is_last:
                picked.append(t)
        # Any other non-layer tensor (e.g. rope freqs) is tiny/shared; a real
        # deployment replicates those cheaply. Kept out of the per-stage plan
        # on purpose so the plan stays "this stage's layers + its endpoints".

    if not picked:
        raise LoaderError(
            f"stage [{first_layer},{last_layer}] matched no tensors — "
            f"check n_total_layers ({n_total_layers}) vs the index"
        )
    ranges = _coalesce(picked, gap=coalesce_gap)
    return FetchPlan(
        stage_first_layer=first_layer,
        stage_n_layers=n_layers,
        tensors=tuple(sorted(picked, key=lambda t: t.offset)),
        ranges=tuple(ranges),
    )


# --- pluggable source: where the single served copy lives -------------------

class Source:
    """Fetches a byte range of the served model. Implemented for local files
    and HTTP range requests; an object-store backend fits the same shape.
    """

    def fetch(self, offset: int, nbytes: int) -> bytes:  # pragma: no cover
        raise NotImplementedError


class LocalFileSource(Source):
    """The served copy is a local (or shared-FS) file: plain seek + read."""

    def __init__(self, path: str):
        self._path = path

    def fetch(self, offset: int, nbytes: int) -> bytes:
        with open(self._path, "rb") as fh:
            fh.seek(offset)
            data = fh.read(nbytes)
        if len(data) != nbytes:
            raise LoaderError(
                f"short read at {offset}: got {len(data)} of {nbytes} bytes"
            )
        return data


class HttpRangeSource(Source):
    """The served copy is one HTTP object; fetch each range with a Range header.

    stdlib only (urllib). The server must honour byte ranges (206 Partial
    Content). One serving node covers the whole cluster; each worker pulls only
    its slice.

    SECURITY (OWASP A01, flagged by node-c/transport): the weight-serving
    endpoint MUST authenticate — an open endpoint lets anyone on the LAN pull
    the whole model. Pass ``auth_headers``: a zero-arg callable returning the
    per-request auth headers, wired by the deployment to
    ``common.auth.request_headers(common.auth.load_token())`` (HMAC over a fresh
    nonce; the token lives only in the environment, never in code — A02). Kept
    as a callable so this module has no hard dependency on ``common`` and stays
    unit-testable on its own. Omitting it works ONLY against a local trusted
    mount, never a shared LAN endpoint (``weight_server`` refuses unauthed).
    """

    def __init__(self, url: str, *, auth_headers=None, timeout: float = 60.0):
        self._url = url
        self._auth_headers = auth_headers
        self._timeout = timeout

    def fetch(self, offset: int, nbytes: int) -> bytes:
        import urllib.request

        end = offset + nbytes - 1
        headers = {"Range": f"bytes={offset}-{end}"}
        if self._auth_headers is not None:
            headers.update(self._auth_headers())
        req = urllib.request.Request(self._url, headers=headers)
        with urllib.request.urlopen(req, timeout=self._timeout) as resp:
            if resp.status not in (200, 206):
                raise LoaderError(f"served source HTTP {resp.status} for range")
            data = resp.read()
        if len(data) != nbytes:
            raise LoaderError(
                f"served source returned {len(data)} of {nbytes} bytes "
                f"(does it honour Range requests?)"
            )
        return data


def fetch_stage_weights(sources, plan: FetchPlan) -> dict[str, bytes]:
    """Pull a stage's tensors from the served copy and slice each out of the
    coalesced ranges. Returns ``{tensor_name: raw_bytes}`` for the executor to
    upload to the device (torch backend, Fase 0).

    ``sources`` is either a single :class:`Source` (single-file model) or a
    ``{shard_label: Source}`` mapping (sharded model — each shard is its own
    file/URL). Ranges and tensors are matched within their shard, so a sharded
    model just works.
    """
    def source_for(shard: str | None) -> Source:
        if isinstance(sources, Source):
            return sources
        try:
            return sources[shard]
        except KeyError:
            raise LoaderError(f"no source configured for shard {shard!r}")

    blobs: dict[ByteRange, bytes] = {
        r: source_for(r.shard).fetch(r.offset, r.nbytes) for r in plan.ranges
    }
    out: dict[str, bytes] = {}
    for t in plan.tensors:
        for r, blob in blobs.items():
            if r.shard == t.shard and r.offset <= t.offset and t.end <= r.end:
                start = t.offset - r.offset
                out[t.name] = blob[start:start + t.nbytes]
                break
        else:  # pragma: no cover - defensive; the plan guarantees coverage
            raise LoaderError(f"tensor {t.name} not covered by any fetched range")
    return out
