#!/usr/bin/env bash
# Install the sofmat git hooks into this clone. Idempotent.
set -euo pipefail

repo_root="$(git rev-parse --show-toplevel)"
hooks_dir="${repo_root}/.git/hooks"
install -m 0755 "${repo_root}/scripts/pre-commit" "${hooks_dir}/pre-commit"
echo "leak-guard pre-commit hook installed at ${hooks_dir}/pre-commit"
echo "Tip: keep a local denylist at leak-guard/denylist.local.txt (git-ignored)"
echo "     with your real hostnames / model names, one per line."
