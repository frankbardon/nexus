#!/usr/bin/env bash
# check-event-versions.sh — guard pkg/events/*.go against silent breaking
# changes. Wraps internal/cmd/check-event-versions, supplying a sane
# default base rev for both local and CI use.
#
# Usage:
#   scripts/check-event-versions.sh                  # diff against HEAD~1
#   scripts/check-event-versions.sh origin/main      # diff against named rev
#   CHECK_EVENTS_BASE=origin/main scripts/check-event-versions.sh
#
# The check passes when every removed/renamed/type-changed field on a
# struct in pkg/events/ was accompanied by a bump of its <Name>Version
# constant. New fields with sensible zero defaults pass without a bump
# (they are forward-compatible). See docs/src/architecture/events.md.

set -euo pipefail

base="${1:-${CHECK_EVENTS_BASE:-}}"

# When invoked outside a CI environment with no explicit base, default
# to HEAD~1 so the check is meaningful on the most recent commit. CI
# pipelines should set CHECK_EVENTS_BASE=origin/main (or the PR target
# branch) to compare the whole branch against the merge target.
if [[ -z "$base" ]]; then
    if git rev-parse --verify HEAD~1 >/dev/null 2>&1; then
        base="HEAD~1"
    else
        echo "check-event-versions: not enough history to diff (no HEAD~1); skipping" >&2
        exit 0
    fi
fi

# Ensure the base rev actually exists so a typo fails loudly rather than
# silently passing.
if ! git rev-parse --verify "$base" >/dev/null 2>&1; then
    echo "check-event-versions: base revision $base not found" >&2
    exit 1
fi

cd "$(git rev-parse --show-toplevel)"

exec go run ./internal/cmd/check-event-versions -base "$base" "$@"
