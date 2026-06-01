#!/usr/bin/env bash
# Link audit: reject obviously broken Markdown links in the assembled draft.
#
# Usage by the ICM plugin: invoked with the candidate artifact's path as
# argv[1]. Exit 0 = pass; non-zero exit + stdout message = fail with the
# stdout used as feedback for the next iteration.
#
# This is a deliberately cheap mechanical check. Anything semantic
# belongs in the LLM rubric.

set -u

artifact="${1:-}"
if [[ -z "${artifact}" || ! -f "${artifact}" ]]; then
  echo "link_audit: artifact path missing or not a file: ${artifact:-<empty>}"
  exit 2
fi

problems=0
issues=""

# Markdown links of the form [text](url). Capture url group with grep -oE.
# We accept http(s) and relative/anchor links; everything else is suspect.
while IFS= read -r line; do
  url="$line"
  # Strip the brackets and the trailing paren.
  url="${url#*\](}"
  url="${url%)*}"

  case "${url}" in
    http://*|https://*)
      # ok shape; we don't fetch — that would be a side effect this
      # predicate has no business making.
      ;;
    \#*|./*|/*)
      # ok shape; relative or anchor.
      ;;
    "")
      issues+=$'\n  empty url in link\n'
      problems=$((problems+1))
      ;;
    TODO|FIXME|tk|TK)
      issues+=$'\n  placeholder url ('"${url}"$') still present\n'
      problems=$((problems+1))
      ;;
    *)
      # Reject anything that is not a recognized URL shape — typically
      # "javascript:" or a missing scheme.
      issues+=$'\n  suspicious url shape: '"${url}"$'\n'
      problems=$((problems+1))
      ;;
  esac
done < <(grep -oE '\][^]]*\([^)]+\)' "${artifact}" || true)

if (( problems > 0 )); then
  echo "link_audit: ${problems} suspicious link(s) found:${issues}"
  exit 1
fi

exit 0
