#!/usr/bin/env bash
set -euo pipefail

env_file=${1:-}
output=${2:-}
if [[ -z "$env_file" || ! -f "$env_file" || -z "$output" ]]; then
  echo "Usage: $0 ENV_FILE OUTPUT_FILE" >&2
  exit 2
fi

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
"$script_dir/validate-production-env.sh" "$env_file"

env_value() {
  local requested_key=$1
  awk -F= -v requested_key="$requested_key" '$1 == requested_key { sub(/^[^=]*=/, ""); print; exit }' "$env_file"
}

bootstrap_mode=$(env_value AUTH_BOOTSTRAP_MODE)
webhook_url=$(env_value ALERTMANAGER_WEBHOOK_URL)
mkdir -p "$(dirname -- "$output")"
umask 077
temporary=$(mktemp "${output}.tmp.XXXXXX")
trap 'rm -f "$temporary"' EXIT

if [[ "$bootstrap_mode" == "true" ]]; then
  cat >"$temporary" <<'YAML'
global:
  resolve_timeout: 5m
route:
  receiver: operator-disabled
  group_by: [alertname, job, severity]
  group_wait: 30s
  group_interval: 5m
  repeat_interval: 4h
receivers:
  - name: operator-disabled
YAML
else
  # URL validation is performed by validate-production-env.sh. YAML single
  # quoting still needs embedded apostrophes escaped without exposing the URL.
  escaped_url=${webhook_url//\'/\'\'}
  cat >"$temporary" <<YAML
global:
  resolve_timeout: 5m
route:
  receiver: operator-webhook
  group_by: [alertname, job, severity]
  group_wait: 30s
  group_interval: 5m
  repeat_interval: 4h
receivers:
  - name: operator-webhook
    webhook_configs:
      - url: '$escaped_url'
        send_resolved: true
YAML
fi

chmod 600 "$temporary"
mv -f "$temporary" "$output"
trap - EXIT
