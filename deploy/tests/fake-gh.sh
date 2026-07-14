#!/usr/bin/env bash
set -euo pipefail

: "${MOCK_GATE_SHA:?MOCK_GATE_SHA is required}"

case "$*" in
  *ci.yml*) workflow=ci ;;
  *security.yml*) workflow=security ;;
  *) echo "Unexpected fake GitHub API request: $*" >&2; exit 64 ;;
esac

conclusion=${MOCK_GATE_CONCLUSION:-success}
if [[ "$workflow" == "security" && -n "${MOCK_SECURITY_CONCLUSION:-}" ]]; then
  conclusion=$MOCK_SECURITY_CONCLUSION
fi

printf '{"workflow_runs":[{"head_sha":"%s","head_branch":"main","event":"push","run_number":7,"run_attempt":1,"status":"completed","conclusion":"%s","html_url":"https://example.invalid/%s"}]}\n' \
  "$MOCK_GATE_SHA" "$conclusion" "$workflow"
