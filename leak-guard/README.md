# leak-guard

The gate that keeps **sofmat** publishable. It scans for private infrastructure
and secrets, and **blocks** them in two places:

- **pre-commit** — aborts a commit whose staged content has a hit.
- **CI** (GitHub Actions) — fails the job on any tracked file; cannot be skipped
  with `--no-verify`.

Pure Python standard library — no install, runs the same everywhere.

## Why two layers of patterns

The scanner must never leak *by existing*. So:

- **Structural rules (public):** private IPv4 ranges, service/API token shapes,
  bearer tokens, credential assignments, absolute home-directory paths (including
  LM-tool cache dirs), and dynamic-DNS hostnames. Each describes a *shape*,
  never one of our real values — safe to publish. (See `scan.py` for the exact
  regexes.)
- **Denylist (private):** the *specific* real terms the structural rules can't
  know — collaborator handles (`x-dev`), real hostnames, exotic hardware,
  model / avatar / client / codenames. Matched as whole words — a short term is
  anchored on its own edges, so it never fires inside a longer word. Two sources,
  neither committed:
  - `leak-guard/denylist.local.txt` — git-ignored, for developer machines.
  - **`SOFMAT_LEAKGUARD_DENYLIST` env var** — newline-separated, injected in CI
    as a repository **secret**. This is what lets CI catch our private terms
    WITHOUT publishing them. (The gap that once leaked dev-handles + infra
    descriptions was exactly this: CI ran only the structural rules.)

The structural rules also cover a bare `.octet:port` host (how an endpoint
leaks without the full `10.0.0.x` prefix). Bare last-octets in prose without a
port are left to review — they are indistinguishable from decimals by regex.

Anonymous labels `node-a/b/c/d` and `*.example.local` placeholders are always
allowed — that is the anonymisation convention the whole repo relies on.

## Use

```bash
bash scripts/install-hooks.sh          # install the pre-commit hook
cp leak-guard/denylist.local.example.txt leak-guard/denylist.local.txt
# ...add your real names to denylist.local.txt (git-ignored)

python3 leak-guard/scan.py --staged    # what the hook runs
python3 leak-guard/scan.py --all       # what CI runs (whole tree)
python3 leak-guard/test_scan.py        # self-test: must-block + must-pass fixtures
```

## Escape hatch

A reviewed false positive can be waved through by appending `leak-guard-allow`
to that line. Use sparingly, and never on a real value.
