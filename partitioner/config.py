"""sofmat partitioner — strict, fail-closed config loading (OWASP A05).

Turns the user's cluster config (the ``config.example.yaml`` schema) into
validated ``NodeProfile`` objects + solver parameters. Design rules:

  * FAIL-CLOSED: unknown keys, wrong types, out-of-range values or missing
    required fields raise ``ConfigError`` — the solver never runs on a
    half-understood config.
  * No secrets in code or logs: ``${ENV_VAR}`` references are resolved from
    the environment at load time and their VALUES never appear in error
    messages (A02). This module never logs resolved values.
  * YAML is parsed with ``yaml.safe_load`` ONLY (A08 — never ``yaml.load``);
    JSON configs are accepted as a stdlib-only fallback.
  * Node ids must be anonymous logical labels (``node-a`` style): the public
    schema never carries hostnames with meaning; real mapping lives in the
    user's gitignored ``config.local.yaml``.
"""

from __future__ import annotations

import json
import os
import re
from dataclasses import dataclass

from solver import NodeProfile

_ID_RE = re.compile(r"^[a-z0-9][a-z0-9-]{0,31}$")
_ENV_RE = re.compile(r"^\$\{([A-Z][A-Z0-9_]{0,63})\}$")

_ALLOWED_NODE_KEYS = {
    "id", "host", "gpus", "vram_gb", "ram_gb", "role", "elastic",
    "unified_memory", "mem_bandwidth_gbps", "unstable_power",
    "model_mem_cap_gb", "ms_per_layer", "present",
}
_ALLOWED_TRANSPORT_KEYS = {"backend", "port", "max_activation_mb", "auth_token"}
_ALLOWED_PARTITIONER_KEYS = {
    "objective", "host_parsimony", "network_time_budget", "min_usable_tokens_s",
}


class ConfigError(Exception):
    """Raised on any config that fails strict validation."""


@dataclass(frozen=True)
class PartitionerSettings:
    network_time_budget: float
    min_usable_tokens_s: float | None  # None = pure speed objective


@dataclass(frozen=True)
class ClusterConfig:
    master: str
    nodes: tuple[NodeProfile, ...]
    settings: PartitionerSettings
    # host/port/auth stay with transport & coordinator; the partitioner only
    # needs profiles + settings, so nothing network-identifying is kept here.


def _require(cond: bool, msg: str) -> None:
    if not cond:
        raise ConfigError(msg)


def _number(section: str, obj: dict, key: str, lo: float, hi: float,
            required=True, default=None):
    if key not in obj:
        _require(not required, f"{section}: missing required field '{key}'")
        return default
    val = obj[key]
    _require(
        isinstance(val, (int, float)) and not isinstance(val, bool),
        f"{section}.{key}: expected a number, got {type(val).__name__}",
    )
    _require(lo <= val <= hi, f"{section}.{key}: {val} out of range [{lo}, {hi}]")
    return float(val)


def resolve_env_ref(section: str, value: str) -> str:
    """Resolve a ``${VAR}`` reference. The resolved VALUE never appears in
    errors (A02) — only the variable NAME does."""
    m = _ENV_RE.match(value)
    if not m:
        return value
    var = m.group(1)
    resolved = os.environ.get(var)
    _require(bool(resolved), f"{section}: env var '{var}' is not set (fail-closed)")
    return resolved  # caller must never log this


def load_cluster_config(text: str, fmt: str = "yaml") -> ClusterConfig:
    """Parse + validate a config document (YAML via safe_load, or JSON)."""
    if fmt == "json":
        try:
            raw = json.loads(text)
        except json.JSONDecodeError as e:
            raise ConfigError(f"invalid JSON: {e}") from None
    elif fmt == "yaml":
        try:
            import yaml  # optional dep; stdlib-only installs can use JSON
        except ImportError:
            raise ConfigError(
                "PyYAML not installed: install it or provide JSON (fmt='json')"
            ) from None
        try:
            raw = yaml.safe_load(text)  # A08: never yaml.load
        except yaml.YAMLError as e:
            raise ConfigError(f"invalid YAML: {e}") from None
    else:
        raise ConfigError(f"unknown config format '{fmt}'")

    _require(isinstance(raw, dict), "config root must be a mapping")
    cluster = raw.get("cluster")
    _require(isinstance(cluster, dict), "missing 'cluster' section")

    master = cluster.get("master")
    _require(isinstance(master, str) and bool(_ID_RE.match(master or "")),
             "cluster.master: must be a logical node id (e.g. 'node-a')")

    nodes_raw = cluster.get("nodes")
    _require(isinstance(nodes_raw, list) and len(nodes_raw) >= 1,
             "cluster.nodes: need at least one node")

    profiles: list[NodeProfile] = []
    seen: set[str] = set()
    for i, node in enumerate(nodes_raw):
        sec = f"cluster.nodes[{i}]"
        _require(isinstance(node, dict), f"{sec}: must be a mapping")
        unknown = set(node) - _ALLOWED_NODE_KEYS
        _require(not unknown, f"{sec}: unknown keys {sorted(unknown)} (fail-closed)")
        node_id = node.get("id")
        _require(isinstance(node_id, str) and bool(_ID_RE.match(node_id or "")),
                 f"{sec}.id: must match {_ID_RE.pattern} (anonymous label)")
        _require(node_id not in seen, f"{sec}.id: duplicate id '{node_id}'")
        seen.add(node_id)

        vram = _number(sec, node, "vram_gb", 0.1, 100_000)
        cap = _number(sec, node, "model_mem_cap_gb", 0.1, 100_000,
                      required=False, default=vram)
        _require(cap <= vram, f"{sec}.model_mem_cap_gb: cap {cap} > vram_gb {vram}")
        bw = _number(sec, node, "mem_bandwidth_gbps", 0.1, 100_000,
                     required=False, default=None)
        ms = _number(sec, node, "ms_per_layer", 0.000001, 100_000,
                     required=False, default=None)
        present = node.get("present", True)
        _require(isinstance(present, bool), f"{sec}.present: must be boolean")

        profiles.append(NodeProfile(
            id=node_id,
            model_mem_cap_gb=cap,
            ms_per_layer=ms,
            mem_bandwidth_gbps=bw,
            present=present,
        ))

    _require(master in seen, f"cluster.master: '{master}' is not a declared node")

    transport = cluster.get("transport", {})
    _require(isinstance(transport, dict), "cluster.transport: must be a mapping")
    unknown = set(transport) - _ALLOWED_TRANSPORT_KEYS
    _require(not unknown,
             f"cluster.transport: unknown keys {sorted(unknown)} (fail-closed)")
    if "backend" in transport:
        _require(transport["backend"] in ("tcp", "rdma"),
                 "cluster.transport.backend: must be 'tcp' or 'rdma'")
    if "port" in transport:
        _number("cluster.transport", transport, "port", 1, 65535)
    if "auth_token" in transport:
        token = transport["auth_token"]
        _require(isinstance(token, str) and len(token) > 0,
                 "cluster.transport.auth_token: must be a non-empty string")
        # Resolve to prove it exists; the value is discarded here on purpose
        # (the transport module re-resolves it — the partitioner never holds
        # secrets).
        resolve_env_ref("cluster.transport.auth_token", token)

    part = raw.get("partitioner", {})
    _require(isinstance(part, dict), "partitioner: must be a mapping")
    unknown = set(part) - _ALLOWED_PARTITIONER_KEYS
    _require(not unknown, f"partitioner: unknown keys {sorted(unknown)} (fail-closed)")
    budget = _number("partitioner", part, "network_time_budget", 0.0, 1.0,
                     required=False, default=0.15)
    if "min_usable_tokens_s" in part:
        floor = _number("partitioner", part, "min_usable_tokens_s", 0.0, 1e9)
    else:
        floor = None  # pure speed objective (decision A default)
    objective = part.get("objective", "speed")
    _require(objective in ("speed", "capacity"),
             "partitioner.objective: must be 'speed' or 'capacity'")
    if objective == "capacity" and floor is None:
        floor = 0.0  # legacy parsimony mode

    return ClusterConfig(
        master=master,
        nodes=tuple(profiles),
        settings=PartitionerSettings(
            network_time_budget=budget,
            min_usable_tokens_s=floor,
        ),
    )
