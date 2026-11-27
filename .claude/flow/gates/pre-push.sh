#!/usr/bin/env bash
# .claude/flow/gates/pre-push.sh — runs before each flow test-group checkpoint and the full verify.
# FLOW_GATE_SCOPE=changed (checkpoint) | full (before ready).
set -euo pipefail
if [ "${FLOW_GATE_SCOPE:-full}" = changed ]; then
  base="${FLOW_BASE_REF:-origin/main}"
  # A go.mod/go.sum change can move any package, root or submodule: run the full sweep.
  if git diff --name-only "$base"...HEAD | grep -qE '(^|/)go\.(mod|sum)$'; then
    make build test check-modules
  else
    pkgs="$(~/.claude/skills/flow-shared/scripts/go-affected-pkgs.sh 2>/dev/null || echo ./...)"
    [ -z "$pkgs" ] || go test -race $pkgs
  fi
else
  make build lint test check-modules
fi
