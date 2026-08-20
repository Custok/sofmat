"""sofmat tools — derive a partitioner ModelSpec from a GGUF header.

Reads ONLY the GGUF metadata (a few KB): never loads tensors, never touches
GPU. Usage:

    python3 tools/gguf_modelspec.py /path/to/model.gguf [max_context] [kv_bytes]

Prints n_layers, weights_gb (file size) and kv_cache_gb computed as
``n_layers * 2 (K+V) * n_kv_heads * head_dim * kv_bytes * max_context``
(kv_bytes: 2 = f16 KV, 1 = q8 KV). The output feeds solver.ModelSpec directly.
"""

from __future__ import annotations

import os
import re
import struct
import sys


class GGUFError(Exception):
    pass


def _parse_header(path: str, max_array: int = 32) -> tuple[dict, list, int]:
    """Parse the GGUF v2/v3 header sequentially.

    Returns ``(metadata, tensor_infos, data_start)`` where ``tensor_infos``
    is a list of ``(name, rel_offset)`` in file order and ``data_start`` is
    the absolute file offset where the tensor data section begins. Large
    metadata arrays are read (to keep the stream aligned) but not stored.
    """
    with open(path, "rb") as f:
        if f.read(4) != b"GGUF":
            raise GGUFError(f"{path}: not a GGUF file")
        (version,) = struct.unpack("<I", f.read(4))
        if version < 2:
            raise GGUFError(f"unsupported GGUF version {version}")
        (n_tensors,) = struct.unpack("<Q", f.read(8))
        (n_kv,) = struct.unpack("<Q", f.read(8))

        def rd_str() -> str:
            (n,) = struct.unpack("<Q", f.read(8))
            return f.read(n).decode("utf-8", "replace")

        def rd_val(t: int):
            scalar = {0: "<B", 1: "<b", 2: "<H", 3: "<h", 4: "<I", 5: "<i",
                      6: "<f", 7: "<?", 10: "<Q", 11: "<q", 12: "<d"}
            if t == 8:
                return rd_str()
            if t == 9:
                (et,) = struct.unpack("<I", f.read(4))
                (n,) = struct.unpack("<Q", f.read(8))
                return [rd_val(et) for _ in range(n)]
            fmt = scalar.get(t)
            if fmt is None:
                raise GGUFError(f"unknown GGUF value type {t}")
            (v,) = struct.unpack(fmt, f.read(struct.calcsize(fmt)))
            return v

        meta: dict = {}
        for _ in range(n_kv):
            key = rd_str()
            (t,) = struct.unpack("<I", f.read(4))
            val = rd_val(t)
            if not (isinstance(val, list) and len(val) > max_array):
                meta[key] = val

        infos: list = []
        for _ in range(n_tensors):
            name = rd_str()
            (n_dims,) = struct.unpack("<I", f.read(4))
            f.read(8 * n_dims)  # dims (unused: sizes come from offset deltas)
            f.read(4)  # ggml type (unused for the same reason)
            (rel_offset,) = struct.unpack("<Q", f.read(8))
            infos.append((name, rel_offset))

        alignment = meta.get("general.alignment", 32)
        pos = f.tell()
        data_start = (pos + alignment - 1) // alignment * alignment
        return meta, infos, data_start


def read_metadata(path: str, max_array: int = 32) -> dict:
    """Parse the GGUF header and return only the key-value metadata."""
    meta, _, _ = _parse_header(path, max_array)
    return meta


def tensor_table(path: str) -> list[dict]:
    """Tensor layout of one GGUF file: ``{name, offset, nbytes}`` per tensor,
    with ``offset`` ABSOLUTE in the file (ready for seek / HTTP Range).

    ``nbytes`` is derived from consecutive data offsets (exact for every
    quantization, no per-type size tables): tensors are laid out in offset
    order, and the last one ends at EOF.
    """
    _, infos, data_start = _parse_header(path)
    if not infos:
        raise GGUFError(f"{path}: no tensors in header")
    ordered = sorted(infos, key=lambda t: t[1])
    file_size = os.path.getsize(path)
    table = []
    for (name, rel), (_, rel_next) in zip(ordered, ordered[1:] + [("", None)]):
        end = data_start + rel_next if rel_next is not None else file_size
        start = data_start + rel
        if end <= start:
            raise GGUFError(f"{path}: non-monotonic tensor offsets at {name}")
        table.append({"name": name, "offset": start, "nbytes": end - start})
    return table


def _weights_bytes(path: str) -> int:
    """Total weight bytes: the file itself, plus its sibling shards when the
    model is split (``model-00001-of-00004.gguf`` style). Pass ANY shard."""
    m = re.match(r"^(.*)-(\d{5})-of-(\d{5})\.gguf$", os.path.basename(path))
    if not m:
        return os.path.getsize(path)
    stem, _, n_shards = m.groups()
    folder = os.path.dirname(os.path.abspath(path))
    total = 0
    for i in range(1, int(n_shards) + 1):
        shard = os.path.join(folder, f"{stem}-{i:05d}-of-{n_shards}.gguf")
        if not os.path.exists(shard):
            raise GGUFError(f"sharded model incomplete: missing {shard}")
        total += os.path.getsize(shard)
    return total


def model_spec(path: str, max_context: int = 8192, kv_bytes: int = 2) -> dict:
    meta = read_metadata(path)
    arch = meta.get("general.architecture")
    if not arch:
        raise GGUFError("general.architecture missing from header")

    def g(key: str, required: bool = True):
        val = meta.get(f"{arch}.{key}")
        if val is None and required:
            raise GGUFError(f"{arch}.{key} missing from header")
        return val

    n_layers = g("block_count")
    head_dim = g("attention.key_length", required=False)
    if head_dim is None:
        head_dim = g("embedding_length") // g("attention.head_count")
    n_kv_heads = g("attention.head_count_kv", required=False) or g(
        "attention.head_count"
    )
    kv_gb = n_layers * 2 * n_kv_heads * head_dim * kv_bytes * max_context / 1e9
    return {
        "architecture": arch,
        "n_layers": n_layers,
        "weights_gb": round(_weights_bytes(path) / 1e9, 1),
        "kv_cache_gb": round(kv_gb, 1),
        "max_context_trained": g("context_length", required=False),
        "kv_assumptions": f"ctx={max_context}, kv_bytes={kv_bytes}",
    }


if __name__ == "__main__":
    args = [a for a in sys.argv[1:] if a != "--tensors"]
    if not args:
        print(__doc__)
        raise SystemExit(1)
    if "--tensors" in sys.argv:
        import json

        print(json.dumps(tensor_table(args[0]), indent=1))
    else:
        ctx = int(args[1]) if len(args) > 1 else 8192
        kvb = int(args[2]) if len(args) > 2 else 2
        for key, val in model_spec(args[0], ctx, kvb).items():
            print(f"{key}: {val}")
