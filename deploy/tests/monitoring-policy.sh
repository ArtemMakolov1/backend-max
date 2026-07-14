#!/usr/bin/env bash
set -euo pipefail

repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
production_compose="$repo_root/deploy/compose.production.yaml"
local_compose="$repo_root/compose.yaml"
prometheus_image='prom/prometheus:v3.13.1@sha256:3c42b892cf723fa54d2f262c37a0e1f80aa8c8ddb1da7b9b0df9455a35a7f893'

if [[ $(grep -c '^    ports:$' "$production_compose") -ne 1 ]] ||
  ! grep -F '127.0.0.1:19090:9090' "$production_compose" >/dev/null; then
  echo "Production may publish only the SSH-only loopback Prometheus port" >&2
  exit 1
fi

for image in \
  'prom/prometheus:v3.13.1@sha256:' \
  'grafana/grafana:nightly-slim@sha256:' \
  'ghcr.io/artemmakolov1/maxposty-postgres-exporter:v0.20.1-go1.26.5.1@sha256:' \
  'ghcr.io/artemmakolov1/maxposty-pgbouncer-exporter:v0.12.1-go1.26.5.1@sha256:' \
  'prom/node-exporter:v1.12.1@sha256:'; do
  grep -F "$image" "$production_compose" >/dev/null || {
    echo "Monitoring image is not pinned by digest: $image" >&2
    exit 1
  }
done

grep -F -- '--storage.tsdb.retention.time=30d' "$production_compose" >/dev/null
grep -F -- '--storage.tsdb.retention.size=5GB' "$production_compose" >/dev/null
if grep -E -- '--web\.(enable-admin-api|enable-lifecycle)=(true|false)' \
  "$production_compose" "$local_compose" >/dev/null; then
  echo "Prometheus boolean switches must not be passed as --flag=true/false arguments" >&2
  exit 1
fi
if grep -F -- '--web.enable-admin-api' "$production_compose" "$local_compose" >/dev/null; then
  echo "Prometheus admin API must remain disabled by its secure default" >&2
  exit 1
fi
grep -F 'GF_AUTH_PROXY_ENABLED: "true"' "$production_compose" >/dev/null
grep -F 'GF_AUTH_BASIC_ENABLED: "false"' "$production_compose" >/dev/null
grep -F 'GF_AUTH_ANONYMOUS_ENABLED: "false"' "$production_compose" >/dev/null
grep -F 'GF_AUTH_PROXY_HEADERS: Role:X-WEBAUTH-ROLE' "$production_compose" >/dev/null
grep -F 'GF_AUTH_PROXY_WHITELIST: 172.29.42.10,127.0.0.1' "$production_compose" >/dev/null
grep -F 'GF_SECURITY_DATA_SOURCE_PROXY_WHITELIST: prometheus:9090' "$production_compose" >/dev/null
grep -F 'ipv4_address: 172.29.42.20' "$production_compose" >/dev/null
grep -F 'name: maxposty-monitoring-edge' "$production_compose" >/dev/null
grep -F '127.0.0.1:${GRAFANA_PORT:-3000}:3000' "$local_compose" >/dev/null
grep -F "grep -Eq '^pg_up 1" "$repo_root/deploy/deploy-production.sh" >/dev/null
grep -F "grep -Eq '^pgbouncer_up 1" "$repo_root/deploy/deploy-production.sh" >/dev/null
grep -F 'wait_for_prometheus_target' "$repo_root/deploy/deploy-production.sh" >/dev/null
grep -F 'wait_for_grafana_provisioning' "$repo_root/deploy/deploy-production.sh" >/dev/null

for service in postgres-exporter pgbouncer-exporter prometheus grafana; do
  service_block=$(awk -v service="$service" '
    $0 == "  " service ":" { inside=1; next }
    inside && $0 ~ /^  [a-zA-Z0-9_-]+:$/ { exit }
    inside { print }
  ' "$production_compose")
  if grep -q '^      edge:' <<<"$service_block"; then
    echo "$service must not join the public edge network" >&2
    exit 1
  fi
done

grafana_block=$(awk '
  $0 == "  grafana:" { inside=1; next }
  inside && $0 ~ /^  [a-zA-Z0-9_-]+:$/ { exit }
  inside { print }
' "$production_compose")
if grep -q '^      edge:' <<<"$grafana_block"; then
  echo "Grafana must not join the shared application edge network" >&2
  exit 1
fi

for dashboard in "$repo_root"/monitoring/grafana/dashboards/*.json; do
  jq -e '(.uid | length > 0) and (.panels | length > 0)' "$dashboard" >/dev/null
  if jq -r '.. | objects | .expr? // empty' "$dashboard" | grep -E '(^|[^a-z])by[[:space:]]*\([^)]*\).*or vector\(0\)' >/dev/null; then
    echo "Labeled dashboard query adds a misleading unlabeled zero series in $dashboard" >&2
    exit 1
  fi
done

grep -F 'pg_long_running_transactions_oldest_timestamp_seconds' \
  "$repo_root/monitoring/prometheus/rules/maxposty-alerts.yml" >/dev/null

if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
  docker run --rm \
    --entrypoint=/bin/promtool \
    -v "$repo_root/monitoring/prometheus/prometheus.yml:/etc/prometheus/prometheus.yml:ro" \
    -v "$repo_root/monitoring/prometheus/rules:/etc/prometheus/rules:ro" \
    "$prometheus_image" \
    check config /etc/prometheus/prometheus.yml >/dev/null
else
  echo "Docker daemon unavailable; skipped promtool validation" >&2
fi

echo "Monitoring policy tests passed."
