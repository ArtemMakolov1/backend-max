#!/usr/bin/env bash
set -euo pipefail

repository=${1:-}
source_sha=${2:-}
max_attempts=${3:-180}
retry_delay_seconds=${4:-10}

if [[ ! "$repository" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]]; then
  echo "Repository must be in OWNER/REPO format" >&2
  exit 2
fi
if [[ ! "$source_sha" =~ ^[0-9a-f]{40}$ ]]; then
  echo "Release gate requires a full lowercase commit SHA" >&2
  exit 2
fi
if [[ ! "$max_attempts" =~ ^[1-9][0-9]*$ ]] || ((max_attempts > 360)); then
  echo "Release gate attempts must be between 1 and 360" >&2
  exit 2
fi
if [[ ! "$retry_delay_seconds" =~ ^[0-9]+$ ]] || ((retry_delay_seconds > 60)); then
  echo "Release gate delay must be between 0 and 60 seconds" >&2
  exit 2
fi

for command_name in gh jq sleep; do
  if ! command -v "$command_name" >/dev/null 2>&1; then
    echo "Release gate command is missing: $command_name" >&2
    exit 1
  fi
done

required_workflows=(ci.yml security.yml)
for workflow_file in "${required_workflows[@]}"; do
  gate_passed=false
  for ((attempt = 1; attempt <= max_attempts; attempt++)); do
    response=$(gh api --method GET \
      "repos/$repository/actions/workflows/$workflow_file/runs" \
      -f branch=main \
      -f event=push \
      -f head_sha="$source_sha" \
      -f per_page=10)
    latest_run=$(jq -c --arg source_sha "$source_sha" '
      [.workflow_runs[]
        | select(.head_sha == $source_sha and .head_branch == "main" and .event == "push")]
      | sort_by(.run_number, .run_attempt)
      | last // null
    ' <<<"$response")

    if [[ "$latest_run" != "null" ]]; then
      status=$(jq -r '.status' <<<"$latest_run")
      conclusion=$(jq -r '.conclusion // ""' <<<"$latest_run")
      run_url=$(jq -r '.html_url // ""' <<<"$latest_run")
      if [[ "$status" == "completed" && "$conclusion" == "success" ]]; then
        echo "Release gate passed: $workflow_file at $source_sha ($run_url)"
        gate_passed=true
        break
      fi
      if [[ "$status" == "completed" ]]; then
        echo "Release gate failed: $workflow_file concluded $conclusion at $source_sha ($run_url)" >&2
        exit 1
      fi
      echo "Waiting for $workflow_file at $source_sha: status=$status (attempt $attempt/$max_attempts)"
    else
      echo "Waiting for $workflow_file at $source_sha: no trusted main push run yet (attempt $attempt/$max_attempts)"
    fi

    if ((attempt < max_attempts)); then
      sleep "$retry_delay_seconds"
    fi
  done

  if [[ "$gate_passed" != "true" ]]; then
    echo "Timed out waiting for successful $workflow_file at $source_sha" >&2
    exit 1
  fi
done

echo "All release workflows passed for exact commit $source_sha"
