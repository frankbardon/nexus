#!/usr/bin/env bash
# .claude/flow/gates/pre-commit.sh — runs before every flow story commit; non-zero blocks it.
set -euo pipefail
make fmt-check
make vet
