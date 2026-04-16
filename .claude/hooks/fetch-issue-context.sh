#!/usr/bin/env bash
# Claude Code hook: user-prompt-submit
# Detects #NNN patterns in user prompt, fetches GitHub issue context via gh CLI.
# Outputs structured issue context to stdout (injected into conversation).

set -euo pipefail

# Read prompt from stdin JSON (Claude Code hook contract)
PROMPT=$(jq -r '.prompt // empty' 2>/dev/null) || PROMPT=""

# Extract all #NNN references (deduplicated)
ISSUES=$(echo "$PROMPT" | grep -oE '#[0-9]+' | sort -u | sed 's/#//') || true

if [ -z "$ISSUES" ]; then
  exit 0
fi

# Auto-detect repo
REPO=$(gh repo view --json nameWithOwner -q .nameWithOwner 2>/dev/null) || {
  echo "⚠ Could not detect GitHub repo. Ensure gh CLI is authenticated." >&2
  exit 0
}

for ISSUE_NUM in $ISSUES; do
  # Fetch issue details
  ISSUE_JSON=$(gh issue view "$ISSUE_NUM" --repo "$REPO" --json \
    number,title,state,body,labels,milestone,assignees,comments,projectItems,closedAt,createdAt,updatedAt \
    2>/dev/null) || {
    echo "⚠ Could not fetch issue #${ISSUE_NUM}" >&2
    continue
  }

  # Fetch linked PRs
  LINKED_PRS=$(gh api "repos/${REPO}/issues/${ISSUE_NUM}/timeline" \
    --jq '[.[] | select(.event == "cross-referenced" and .source.issue.pull_request != null) | {number: .source.issue.number, title: .source.issue.title, state: .source.issue.state, url: .source.issue.html_url}] | unique_by(.number)' \
    2>/dev/null) || LINKED_PRS="[]"

  # Format output
  TITLE=$(echo "$ISSUE_JSON" | jq -r '.title')
  STATE=$(echo "$ISSUE_JSON" | jq -r '.state')
  BODY=$(echo "$ISSUE_JSON" | jq -r '.body // "No description"')
  LABELS=$(echo "$ISSUE_JSON" | jq -r '[.labels[].name] | join(", ") // "none"')
  MILESTONE=$(echo "$ISSUE_JSON" | jq -r '.milestone.title // "none"')
  ASSIGNEES=$(echo "$ISSUE_JSON" | jq -r '[.assignees[].login] | join(", ") // "unassigned"')
  CREATED=$(echo "$ISSUE_JSON" | jq -r '.createdAt')
  UPDATED=$(echo "$ISSUE_JSON" | jq -r '.updatedAt')
  PROJECT_STATUS=$(echo "$ISSUE_JSON" | jq -r '[.projectItems[]? | "\(.project.title): \(.status.name // "no status")"] | join(", ") // "none"')

  cat <<EOF
--- GitHub Issue #${ISSUE_NUM} ---
Title: ${TITLE}
State: ${STATE}
Labels: ${LABELS}
Milestone: ${MILESTONE}
Assignees: ${ASSIGNEES}
Project: ${PROJECT_STATUS}
Created: ${CREATED}
Updated: ${UPDATED}

## Description
${BODY}

EOF

  # Comments
  COMMENT_COUNT=$(echo "$ISSUE_JSON" | jq '.comments | length')
  if [ "$COMMENT_COUNT" -gt 0 ]; then
    echo "## Comments (${COMMENT_COUNT})"
    echo "$ISSUE_JSON" | jq -r '.comments[] | "### \(.author.login) (\(.createdAt))\n\(.body)\n"'
  fi

  # Linked PRs
  PR_COUNT=$(echo "$LINKED_PRS" | jq 'length')
  if [ "$PR_COUNT" -gt 0 ]; then
    echo "## Linked PRs"
    echo "$LINKED_PRS" | jq -r '.[] | "- #\(.number) \(.title) [\(.state)] \(.url)"'
    echo ""
  fi

  echo "--- End Issue #${ISSUE_NUM} ---"
  echo ""
done
