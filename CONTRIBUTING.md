# Contributing to sofmat

Thanks for looking. sofmat is meant to be **forked and continued** — the design
is documented in [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md), the direction
in [`docs/ROADMAP.md`](docs/ROADMAP.md), and the reasoning behind the choices in
[`docs/research/`](docs/research/).

## Run it first
Everything runs on the standard library, no GPU required — see **Quickstart** in
the [README](README.md). If the tests are green on a fresh clone, you're set up.

## The bar for code
This is a public project; the code is the first thing anyone sees, so we hold a
high bar. Before you open a PR:

- **Clear names, real docstrings, type hints.** A reader should understand a
  module from its top docstring and a function from its signature.
- **No dead code, no un-commented cleverness.** If a line is subtle, say why.
- **Tests for behaviour you add or change.** Keep them fast and deterministic;
  the mock path means most things are testable without a GPU.
- **Idiomatic, PEP 8, small functions.** Match the style already in the tree.
- **Helpful errors.** Fail closed with a message that says what and why.

## Non-negotiables

### Security
- **Never deserialize anything off the wire with `pickle`, `yaml.load`, or `torch.load(weights_only=False)`.** Activations use the binary frame in `transport/framing.py`; everything received is validated before use. <!-- leak-guard-allow: this line documents the forbidden calls, it does not use them -->

- **Validate everything received before it is used** (shapes, sizes, types).
- **Authenticated transport only** — no open port that accepts tensors from the
  LAN. Secrets come from the environment / `config.local`, never from code.

### No private infrastructure in the repo
sofmat is infra-agnostic on purpose. The repository must never contain a real IP,
hostname, token, model path, or cluster topology. In code, docs and tests use the
**anonymous labels** `node-a` / `node-b` / … and `*.example.local`; the mapping to
real machines lives only in `config.local.yaml`, which is git-ignored.

The **leak-guard** enforces this. Install the hook once and it blocks a commit
that would leak:

```bash
bash scripts/install-hooks.sh
python leak-guard/scan.py --all      # what CI runs; must be clean
```

CI runs the same scan on every push and cannot be skipped with `--no-verify`.

## Pull requests
1. Fork, branch, make the change with tests.
2. `python <module>/test_*.py` green, and `python leak-guard/scan.py --all` clean.
3. Keep the PR focused; describe what and why. A reviewer should be able to read
   it top to bottom without archaeology.

## Layout
`config.example.yaml` · `partitioner/` · `transport/` · `coordinator/` ·
`leak-guard/` · `docs/`. Each module is self-contained and independently
testable — start with the one you care about.
