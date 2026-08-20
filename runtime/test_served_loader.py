"""Tests for the served-model loader. Pure stdlib unittest, no GPU, no real GGUF.

A synthetic tensor index (contiguous byte layout) stands in for a parsed GGUF,
so the fetch PLANNER and the byte slicing are verified without a model file.

Run:  python3 -m unittest test_served_loader   (from the runtime/ directory)
"""

from __future__ import annotations

import os
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(__file__))

from served_loader import (  # noqa: E402
    TensorInfo, ByteRange, LoaderError, plan_stage_fetch, _coalesce,
    LocalFileSource, fetch_stage_weights, tensor_index_from_tables,
)

N_LAYERS = 32
EMBED = 4096
ATTN = 1000
FFN = 3000
HEAD = 4096


def _synthetic_index() -> tuple[list[TensorInfo], int]:
    """Contiguous layout: token_embd | blk.0.attn blk.0.ffn | ... | norm | output.
    Returns (index, total_bytes).
    """
    idx: list[TensorInfo] = []
    off = 0
    idx.append(TensorInfo("token_embd.weight", off, EMBED)); off += EMBED
    for n in range(N_LAYERS):
        idx.append(TensorInfo(f"blk.{n}.attn.weight", off, ATTN)); off += ATTN
        idx.append(TensorInfo(f"blk.{n}.ffn.weight", off, FFN)); off += FFN
    idx.append(TensorInfo("output_norm.weight", off, HEAD)); off += HEAD
    idx.append(TensorInfo("output.weight", off, HEAD)); off += HEAD
    return idx, off


class Planner(unittest.TestCase):
    def setUp(self):
        self.index, self.total = _synthetic_index()

    def _names(self, plan):
        return {t.name for t in plan.tensors}

    def test_first_stage_gets_embedding_not_head(self):
        p = plan_stage_fetch(self.index, 0, 8, N_LAYERS)
        names = self._names(p)
        self.assertIn("token_embd.weight", names)
        self.assertIn("blk.0.attn.weight", names)
        self.assertIn("blk.7.ffn.weight", names)
        self.assertNotIn("blk.8.attn.weight", names)
        self.assertNotIn("output.weight", names)

    def test_middle_stage_only_its_layers(self):
        p = plan_stage_fetch(self.index, 8, 16, N_LAYERS)
        names = self._names(p)
        self.assertNotIn("token_embd.weight", names)
        self.assertNotIn("output.weight", names)
        self.assertIn("blk.8.attn.weight", names)
        self.assertIn("blk.23.ffn.weight", names)
        self.assertNotIn("blk.24.attn.weight", names)

    def test_last_stage_gets_head_not_embedding(self):
        p = plan_stage_fetch(self.index, 24, 8, N_LAYERS)
        names = self._names(p)
        self.assertIn("blk.31.ffn.weight", names)
        self.assertIn("output_norm.weight", names)
        self.assertIn("output.weight", names)
        self.assertNotIn("token_embd.weight", names)

    def test_full_partition_covers_everything_once(self):
        # 3 contiguous stages covering [0,32) must fetch every tensor exactly
        # once (no gaps, no double-count) — the correctness property of the split.
        plans = [
            plan_stage_fetch(self.index, 0, 12, N_LAYERS),
            plan_stage_fetch(self.index, 12, 12, N_LAYERS),
            plan_stage_fetch(self.index, 24, 8, N_LAYERS),
        ]
        seen = [t.name for p in plans for t in p.tensors]
        self.assertEqual(len(seen), len(set(seen)), "a tensor was fetched twice")
        self.assertEqual(set(seen), {t.name for t in self.index})
        self.assertEqual(sum(p.total_bytes for p in plans), self.total)

    def test_contiguous_stage_coalesces_to_one_range(self):
        # A contiguous layout means one stage's tensors merge into a single
        # request (minimal round trips against the served copy).
        p = plan_stage_fetch(self.index, 8, 8, N_LAYERS)
        self.assertEqual(len(p.ranges), 1)

    def test_bad_range_rejected(self):
        with self.assertRaises(LoaderError):
            plan_stage_fetch(self.index, 0, 0, N_LAYERS)

    def test_wrong_total_layers_detected(self):
        # If the caller passes n_total_layers too small, the "last stage" logic
        # still works, but a stage past the real end matches nothing -> error.
        with self.assertRaises(LoaderError):
            plan_stage_fetch(self.index, 40, 4, N_LAYERS)


class Coalesce(unittest.TestCase):
    def test_merges_touching_and_splits_gaps(self):
        ts = [
            TensorInfo("a", 0, 100),
            TensorInfo("b", 100, 100),     # touches a
            TensorInfo("c", 500, 100),     # gap -> separate
        ]
        ranges = _coalesce(ts)
        self.assertEqual(ranges, [ByteRange(0, 200), ByteRange(500, 100)])

    def test_gap_tolerance_merges_small_holes(self):
        ts = [TensorInfo("a", 0, 100), TensorInfo("b", 108, 100)]  # 8-byte hole
        self.assertEqual(len(_coalesce(ts, gap=8)), 1)
        self.assertEqual(len(_coalesce(ts, gap=0)), 2)


class Fetch(unittest.TestCase):
    def test_local_source_slices_correct_bytes(self):
        index, total = _synthetic_index()
        # Materialise a fake model file whose every byte encodes its position.
        blob = bytes((i % 251) for i in range(total))
        with tempfile.NamedTemporaryFile(delete=False) as fh:
            fh.write(blob)
            path = fh.name
        try:
            plan = plan_stage_fetch(index, 8, 8, N_LAYERS)
            weights = fetch_stage_weights(LocalFileSource(path), plan)
            # Every fetched tensor's bytes must equal the file's bytes at its offset.
            by_name = {t.name: t for t in index}
            for name, data in weights.items():
                t = by_name[name]
                self.assertEqual(data, blob[t.offset:t.end], f"bad slice for {name}")
        finally:
            os.unlink(path)


class Sharded(unittest.TestCase):
    """Two-shard model (like the Qwen BF16 BF16/*-00001/00002): layers 0-15 in
    shard A, 16-31 in shard B, each with its OWN offset space.
    """

    def _index(self):
        idx = []
        # shard A: token_embd + blk.0..15
        off = 0
        idx.append(TensorInfo("token_embd.weight", off, EMBED, "A")); off += EMBED
        for n in range(16):
            idx.append(TensorInfo(f"blk.{n}.attn.weight", off, ATTN, "A")); off += ATTN
            idx.append(TensorInfo(f"blk.{n}.ffn.weight", off, FFN, "A")); off += FFN
        # shard B: blk.16..31 + norm + output, offsets restart in B's own space
        off = 0
        for n in range(16, 32):
            idx.append(TensorInfo(f"blk.{n}.attn.weight", off, ATTN, "B")); off += ATTN
            idx.append(TensorInfo(f"blk.{n}.ffn.weight", off, FFN, "B")); off += FFN
        idx.append(TensorInfo("output_norm.weight", off, HEAD, "B")); off += HEAD
        idx.append(TensorInfo("output.weight", off, HEAD, "B")); off += HEAD
        return idx

    def test_stage_spanning_shards(self):
        idx = self._index()
        # stage [12,20) crosses the A/B boundary: layers 12-15 in A, 16-19 in B.
        p = plan_stage_fetch(idx, 12, 8, N_LAYERS)
        shards = {r.shard for r in p.ranges}
        self.assertEqual(shards, {"A", "B"})
        names = {t.name for t in p.tensors}
        self.assertIn("blk.12.attn.weight", names)
        self.assertIn("blk.19.ffn.weight", names)
        self.assertNotIn("blk.11.attn.weight", names)
        self.assertNotIn("blk.20.attn.weight", names)
        # each range is tagged with the shard its tensors live in
        for r in p.ranges:
            self.assertIn(r.shard, ("A", "B"))

    def test_ranges_never_cross_shards(self):
        idx = self._index()
        p = plan_stage_fetch(idx, 0, 32, N_LAYERS)  # whole model
        for r in p.ranges:
            covered = [t for t in p.tensors
                       if t.shard == r.shard and r.offset <= t.offset and t.end <= r.end]
            self.assertTrue(covered, f"range {r} covers nothing in its shard")

    def test_fetch_with_per_shard_sources(self):
        idx = self._index()
        # Materialise two shard files whose bytes encode position.
        paths = {}
        try:
            for shard in ("A", "B"):
                size = max(t.end for t in idx if t.shard == shard)
                blob = bytes((i % 251) for i in range(size))
                with tempfile.NamedTemporaryFile(delete=False) as fh:
                    fh.write(blob)
                    paths[shard] = (fh.name, blob)
            sources = {s: LocalFileSource(paths[s][0]) for s in paths}
            plan = plan_stage_fetch(idx, 12, 8, N_LAYERS)
            weights = fetch_stage_weights(sources, plan)
            by_name = {t.name: t for t in idx}
            for name, data in weights.items():
                t = by_name[name]
                _, blob = paths[t.shard]
                self.assertEqual(data, blob[t.offset:t.end], f"bad slice for {name}")
        finally:
            for _fname, _ in paths.values():
                pass
            import os as _os
            for fname, _ in paths.values():
                _os.unlink(fname)


class FromTables(unittest.TestCase):
    def test_builds_index_from_parser_output(self):
        tables = {
            "s1": [{"name": "blk.0.attn.weight", "offset": 100, "nbytes": 50}],
            "s2": [{"name": "blk.1.attn.weight", "offset": 0, "nbytes": 70}],
        }
        idx = tensor_index_from_tables(tables)
        self.assertEqual(len(idx), 2)
        by_shard = {t.shard: t for t in idx}
        self.assertEqual(by_shard["s1"].nbytes, 50)
        self.assertEqual(by_shard["s2"].offset, 0)


if __name__ == "__main__":
    unittest.main()
