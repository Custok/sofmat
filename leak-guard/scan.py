#!/usr/bin/env python3
"""sofmat leak-guard — block private infra / secrets before a public push.

The engine that keeps this repository publishable. It runs in two places:

  * pre-commit hook  -> scans STAGED content, aborts the commit on a hit.
  * CI (GitHub Actions) -> scans the whole tree, fails the job on a hit.

Design (important — the scanner itself must not leak):

  * PUBLIC patterns are STRUCTURAL, not enumerations. Private IPv4 ranges,
    token shapes (``lst_...``, ``ghp_...``, Bearer blobs), absolute home
    paths, dynamic-DNS domains — all safe to publish because they describe a
    *shape*, never one of our real values.
  * SPECIFIC real names (our hostnames, model / avatar / client names) live in
    an OPTIONAL local denylist file (``leak-guard/denylist.local.txt``) that is
    git-ignored and never leaves the lab. The scanner loads it when present as
    extra literal patterns; the published scanner reveals nothing about us.

Anonymous cluster labels ``node-a/b/c/d`` and ``*.example.local`` placeholders
are always allowed — that is the whole point of the anonymisation convention.

Pure standard library on purpose (matches the partitioner): no install step,
runs the same on every host and in CI.
"""

from __future__ import annotations

import argparse
import os
import re
import subprocess
import sys
from dataclasses import dataclass

DENYLIST_LOCAL = os.path.join(os.path.dirname(__file__), "denylist.local.txt")

# Extensions we never scan as text (binary / weights). Weights are gitignored
# anyway, but a stray sample must not blow up the scanner.
SKIP_EXT = {
    ".gguf", ".safetensors", ".bin", ".pt", ".pth", ".onnx",
    ".png", ".jpg", ".jpeg", ".webp", ".gif", ".ico",
    ".wav", ".mp3", ".m4a", ".flac", ".ogg", ".mp4", ".mov",
    ".zip", ".gz", ".tar", ".7z", ".pdf", ".woff", ".woff2",
}

# Paths never CONTENT-scanned: the scanner's own engine (necessarily embeds
# pattern fragments), its self-test (deliberate fakes) and the git-ignored
# denylist. Same convention gitleaks uses for its own config.
SKIP_PATH_RE = re.compile(
    r"(^|/)(\.git/|node_modules/|\.venv/|venv/|__pycache__/"
    r"|leak-guard/scan\.py$|leak-guard/test_scan\.py$"
    r"|leak-guard/denylist\.local\.txt$)"
)

# Files that must NEVER be committed, whatever their content. Defence in depth:
# .gitignore is the first line, but if someone force-adds one of these, the
# guard blocks it by NAME (the denylist is content-skipped above, so without
# this it could slip through). A leak-guard that skips a file must still refuse
# to let that file be committed.
FORBIDDEN_BASENAME = re.compile(
    r"(^|/)(denylist\.local\.txt"
    r"|config\.local\.(ya?ml|json|toml)"
    r"|nodes\.local\.[^/]+"
    r"|\.env(\.[^/]+)?)$"
)


@dataclass(frozen=True)
class Rule:
    name: str
    pattern: re.Pattern[str]
    hint: str


# Lines that legitimately contain a shape-match are waved through when they
# also carry an allow marker. Keeps the example config / docs committable.
ALLOW_RE = re.compile(
    r"node-[a-z]\b"
    r"|\.example\.(local|com|org)\b"
    r"|REPLACE|PLACEHOLDER|example\.yaml"
    r"|leak-guard-allow",  # explicit escape hatch for a reviewed false positive
    re.IGNORECASE,
)


def _structural_rules() -> list[Rule]:
    return [
        Rule(
            "private-ipv4",
            # 10.x, 192.168.x, 172.16-31.x — private ranges that would pin our LAN.
            re.compile(
                r"\b(?:10(?:\.\d{1,3}){3}"
                r"|192\.168(?:\.\d{1,3}){2}"
                r"|172\.(?:1[6-9]|2\d|3[01])(?:\.\d{1,3}){2})\b"
            ),
            "private IPv4 address — use node-x logical labels, real IPs go in config.local.yaml",
        ),
        Rule(
            "ip-octet-port",
            # A bare last-octet with a port — how a host leaks WITHOUT the full
            # 10.0.0.x prefix (e.g. ".51:50052"). Requires the ":port" so plain
            # decimals like "0.51" don't false-positive. (Gap flagged by node-c.)
            re.compile(r"(?<!\d)\.\d{1,3}:\d{2,5}\b"),
            "host as .octet:port — use a node-x label; real endpoints in config.local.yaml",
        ),
        Rule(
            "service-token",
            re.compile(r"\b(?:lst_|ghp_|gho_|ghs_|xox[baprs]-)[A-Za-z0-9_\-]{8,}"),
            "looks like a service/API token",
        ),
        Rule(
            "bearer-token",
            # "Bearer <blob>" / "Token <blob>" — space-separated, no colon.
            re.compile(r"(?i)\b(?:bearer|token)\s+[A-Za-z0-9._\-]{16,}"),
            "hardcoded bearer/token — load from env / config.local.yaml",
        ),
        Rule(
            "credential-assign",
            # key: value / key=value form (authorization, api_key, secret, ...).
            re.compile(r"(?i)\b(?:authorization|api[_-]?key|secret|password|passwd)\b\s*[:=]\s*['\"]?[A-Za-z0-9._\-]{12,}"),
            "hardcoded credential — load from env / config.local.yaml",
        ),
        Rule(
            "abs-home-path",
            re.compile(r"(?:/home/[A-Za-z0-9._-]+|/Users/[A-Za-z0-9._-]+|[Cc]:\\Users\\[A-Za-z0-9._-]+|~/\.lmstudio)\b"),
            "absolute local path — describes our lab; use a config-driven path",
        ),
        Rule(
            "dyndns-host",
            re.compile(r"\b[A-Za-z0-9_-]+\.(?:mynetgear\.com|dyndns\.\w+|no-ip\.\w+)\b"),
            "dynamic-DNS hostname of our network",
        ),
        # --- security-gate (David 2026-08-20: OWASP Top 10, público) ---
        # A08 insecure deserialization: sofmat moves tensors between hosts over
        # the network. pickle/torch.load/yaml.load on untrusted input = RCE.
        Rule(
            "unsafe-deser",
            re.compile(
                r"\b(?:pickle\.loads?|cPickle|_pickle|marshal\.loads?"
                r"|torch\.load\s*\((?![^)]*weights_only\s*=\s*True)"
                r"|yaml\.load\s*\((?![^)]*Safe))"
            ),
            "unsafe deserialization (OWASP A08) — use a binary framing / safe_load / weights_only=True; never on network input",
        ),
        # NOTE on the anonymisation convention (added after a real leak: dev
        # handles + infra descriptions slipped into docs because they are
        # neither IPs nor tokens). The SPECIFIC private terms — collaborator
        # handles, our hardware, product/client names — live in the denylist
        # (local file + SOFMAT_LEAKGUARD_DENYLIST CI secret), NOT as public
        # regexes here: naming them in a published rule would itself leak, and a
        # generic "<x>-dev" rule false-positives on package names
        # (e.g. libssl-dev). See _local_rules.
    ]


def _local_rules() -> list[Rule]:
    """Literal denylist of SPECIFIC private terms, from two sources:

    * ``leak-guard/denylist.local.txt`` — git-ignored local file (developer
      machines). Real hostnames, model / avatar / client names, exotic hardware.
    * ``SOFMAT_LEAKGUARD_DENYLIST`` env var — newline-separated, so CI can inject
      the same list as a repository SECRET. This is what makes CI catch our
      private terms WITHOUT ever committing them: the terms live in a GitHub
      secret, not in the repo. (The gap that let dev-handles / infra descriptions
      through was exactly this: CI ran only the structural rules.)

    One entry per line (``# comment`` and blanks ignored); each is a
    case-insensitive literal pattern.
    """
    terms: list[str] = []
    try:
        with open(DENYLIST_LOCAL, encoding="utf-8") as fh:
            terms.extend(fh.read().splitlines())
    except FileNotFoundError:
        pass
    terms.extend((os.environ.get("SOFMAT_LEAKGUARD_DENYLIST") or "").splitlines())

    rules: list[Rule] = []
    for raw in terms:
        term = raw.strip()
        if not term or term.startswith("#"):
            continue
        rules.append(
            Rule(
                "private-term",
                # Whole-word match: a short name like "Ada" must NOT fire inside
                # "metadata" / "validated". \b anchors on the term's own edges.
                re.compile(r"\b" + re.escape(term) + r"\b", re.IGNORECASE),
                "matches a private term (denylist.local.txt / SOFMAT_LEAKGUARD_DENYLIST)",
            )
        )
    return rules


def _iter_staged_files() -> list[str]:
    out = subprocess.run(
        ["git", "diff", "--cached", "--name-only", "--diff-filter=ACM"],
        capture_output=True, text=True, check=True,
    ).stdout
    return [p for p in out.splitlines() if p]


def _iter_tree_files(root: str) -> list[str]:
    try:
        out = subprocess.run(
            ["git", "-C", root, "ls-files"],
            capture_output=True, text=True, check=True,
        ).stdout
        return [os.path.join(root, p) for p in out.splitlines() if p]
    except (subprocess.CalledProcessError, FileNotFoundError):
        files = []
        for dirpath, _dirs, names in os.walk(root):
            for n in names:
                files.append(os.path.join(dirpath, n))
        return files


def _read_text(path: str) -> str | None:
    _, ext = os.path.splitext(path)
    if ext.lower() in SKIP_EXT:
        return None
    try:
        with open(path, "rb") as fh:
            blob = fh.read()
        if b"\x00" in blob[:4096]:  # binary sniff
            return None
        return blob.decode("utf-8", errors="replace")
    except (OSError, IsADirectoryError):
        return None


@dataclass(frozen=True)
class Finding:
    path: str
    line: int
    rule: str
    hint: str
    excerpt: str


def scan_paths(paths: list[str], rules: list[Rule]) -> list[Finding]:
    findings: list[Finding] = []
    for path in paths:
        norm = path.replace("\\", "/")
        if FORBIDDEN_BASENAME.search(norm):
            findings.append(Finding(
                path, 0, "forbidden-file",
                "this file must never be committed (local secrets / real infra) — it belongs only on disk, git-ignored",
                os.path.basename(norm),
            ))
            continue
        if SKIP_PATH_RE.search(norm):
            continue
        text = _read_text(path)
        if text is None:
            continue
        for lineno, line in enumerate(text.splitlines(), start=1):
            if ALLOW_RE.search(line):
                continue
            for rule in rules:
                m = rule.pattern.search(line)
                if m:
                    excerpt = line.strip()
                    if len(excerpt) > 120:
                        excerpt = excerpt[:117] + "..."
                    findings.append(Finding(path, lineno, rule.name, rule.hint, excerpt))
                    break  # one finding per line is enough to block
    return findings


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description="sofmat leak-guard scanner")
    g = ap.add_mutually_exclusive_group()
    g.add_argument("--staged", action="store_true", help="scan git-staged content (pre-commit)")
    g.add_argument("--all", action="store_true", help="scan the whole tree (CI)")
    ap.add_argument("--root", default=".", help="repo root for --all")
    ap.add_argument("paths", nargs="*", help="explicit files to scan")
    args = ap.parse_args(argv)

    if args.staged:
        paths = _iter_staged_files()
    elif args.paths:
        paths = args.paths
    else:
        paths = _iter_tree_files(args.root)

    rules = _structural_rules() + _local_rules()
    findings = scan_paths(paths, rules)

    if not findings:
        print(f"leak-guard: clean ({len(paths)} file(s) scanned, "
              f"{len(rules)} rule(s)).")
        return 0

    print(f"\n🚫 leak-guard BLOCKED — {len(findings)} potential leak(s):\n", file=sys.stderr)
    for f in findings:
        print(f"  {f.path}:{f.line}  [{f.rule}] {f.hint}", file=sys.stderr)
        print(f"      > {f.excerpt}", file=sys.stderr)
    print(
        "\nFix: move the value to config.local.yaml / env, use a node-x label, "
        "or (reviewed false positive) append 'leak-guard-allow' to the line.\n",
        file=sys.stderr,
    )
    return 1


if __name__ == "__main__":
    raise SystemExit(main())
