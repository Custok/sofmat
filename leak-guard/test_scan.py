#!/usr/bin/env python3
"""Self-test for the leak-guard scanner. Pure stdlib, no pytest needed.

Runs a set of MUST-BLOCK fixtures (each has to produce >=1 finding) and a set
of MUST-PASS fixtures (anonymised / placeholder content that has to stay
clean). Exit 0 only if every case behaves. This file is itself skipped by the
scanner (SKIP_PATH_RE) precisely because it embeds deliberate fakes.

Run: python3 leak-guard/test_scan.py
"""

from __future__ import annotations

import sys
import tempfile
import os

sys.path.insert(0, os.path.dirname(__file__))
from scan import scan_paths, _structural_rules  # noqa: E402

RULES = _structural_rules()  # local denylist not needed for the structural tests

MUST_BLOCK = {
    "private-ip": "coordinator = 10.0.0.30",
    "private-ip-192": "gateway: 192.168.1.1",
    "service-token": 'TOKEN = "lst_nMPp3xQ72zaBcd1234"',
    "bearer": 'Authorization: Bearer abcDEF1234567890xyz',
    "api-key-assign": 'api_key = "sk-9f8e7d6c5b4a3210"',
    "home-path": "weights at /home/someuser/models/big.gguf",
    "lmstudio-path": "load ~/.lmstudio/models/foo",
    "dyndns": "reach it at mybox.mynetgear.com",
    "pickle-loads": "act = pickle.loads(frame)",
    "torch-load-unsafe": "state = torch.load(path)",
    "yaml-load-unsafe": "cfg = yaml.load(open(p))",
}

MUST_PASS = {
    "anon-label": "master: node-a  # logical label",
    "example-host": "host: node-b.example.local",
    "placeholder-path": "path: /REPLACE/WITH/LOCAL/MODEL/PATH",
    "public-ip-doc": "example public DNS 8.8.8.8 in a comment",  # not a private range
    "port-number": "port: 50051",
    "allow-marker": "note: 10.0.0.0/24 is our lab range  # leak-guard-allow",
    "plain-prose": "The runtime measures ms per layer and reports it.",
    "torch-load-safe": "state = torch.load(path, weights_only=True)",
    "yaml-safe": "cfg = yaml.safe_load(open(p))",
}


def _scan_snippet(text: str) -> int:
    with tempfile.NamedTemporaryFile("w", suffix=".txt", delete=False) as fh:
        fh.write(text + "\n")
        name = fh.name
    try:
        return len(scan_paths([name], RULES))
    finally:
        os.unlink(name)


# Files blocked by NAME regardless of content (defence in depth). scan_paths
# flags these before reading, so the path need not exist on disk.
MUST_BLOCK_PATHS = [
    "leak-guard/denylist.local.txt",
    "config.local.yaml",
    "some/dir/config.local.json",
    ".env",
    ".env.production",
    "nodes.local.map",
]
MUST_PASS_PATHS = [
    "leak-guard/denylist.local.example.txt",  # the committable example
    "config.example.yaml",
    "runtime/worker.py",
]


def main() -> int:
    failures = []
    for label, snippet in MUST_BLOCK.items():
        if _scan_snippet(snippet) == 0:
            failures.append(f"MUST-BLOCK '{label}' was NOT caught: {snippet!r}")
    for label, snippet in MUST_PASS.items():
        n = _scan_snippet(snippet)
        if n != 0:
            failures.append(f"MUST-PASS '{label}' false-positived ({n}): {snippet!r}")
    for path in MUST_BLOCK_PATHS:
        if len(scan_paths([path], RULES)) == 0:
            failures.append(f"MUST-BLOCK-PATH not caught: {path}")
    for path in MUST_PASS_PATHS:
        # These don't exist on disk; a clean pass means 0 findings (name ok,
        # content unread). Use a name-only assertion via scan_paths.
        n = len(scan_paths([path], RULES))
        if n != 0:
            failures.append(f"MUST-PASS-PATH false-positived ({n}): {path}")

    total = (len(MUST_BLOCK) + len(MUST_PASS)
             + len(MUST_BLOCK_PATHS) + len(MUST_PASS_PATHS))
    if failures:
        print(f"leak-guard self-test: {len(failures)}/{total} FAILED", file=sys.stderr)
        for f in failures:
            print(f"  - {f}", file=sys.stderr)
        return 1
    print(f"leak-guard self-test: {total}/{total} OK "
          f"({len(MUST_BLOCK)} content-block, {len(MUST_PASS)} content-pass, "
          f"{len(MUST_BLOCK_PATHS)} name-block, {len(MUST_PASS_PATHS)} name-pass)")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
