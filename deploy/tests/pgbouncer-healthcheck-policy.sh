#!/usr/bin/env bash
set -euo pipefail

repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
expected='test: ["CMD", "bash", "-ec", "exec 3<>/dev/tcp/127.0.0.1/6432"]'

for compose_file in "$repo_root/compose.yaml" "$repo_root/deploy/compose.production.yaml"; do
  matches=$(grep -Fc -- "$expected" "$compose_file" || true)
  if [[ "$matches" -ne 1 ]]; then
    echo "PgBouncer must use the Bash TCP healthcheck exactly once in $compose_file" >&2
    exit 1
  fi
done

echo "PgBouncer healthcheck policy tests passed."
