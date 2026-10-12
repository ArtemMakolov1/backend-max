#!/usr/bin/env bash
set -euo pipefail

# Test the exact image digest before it can replace a production pin. This
# isolated fixture has no production credentials and no outbound connectivity.
: "${GRAFANA_IMAGE:?An immutable Grafana image is required}"
: "${PROMETHEUS_IMAGE:?An immutable Prometheus image is required}"
for image in "$GRAFANA_IMAGE" "$PROMETHEUS_IMAGE"; do
  [[ "$image" =~ @sha256:[0-9a-f]{64}$ ]] || {
    echo 'Monitoring smoke tests require immutable image digests' >&2
    exit 1
  }
done

fixture=$(mktemp -d)
network="maxposty-monitoring-test-${RANDOM}-${RANDOM}"
grafana="${network}-grafana"
prometheus="${network}-prometheus"
cleanup() {
  local result=$?
  if (( result != 0 )); then
    docker logs --tail 100 "$grafana" 2>&1 || true
    docker logs --tail 50 "$prometheus" 2>&1 || true
  fi
  docker rm -f "$grafana" "$prometheus" >/dev/null 2>&1 || true
  docker network rm "$network" >/dev/null 2>&1 || true
  rm -rf "$fixture"
  exit "$result"
}
trap cleanup EXIT
mkdir -p "$fixture/provisioning/datasources"
chmod 0755 "$fixture" "$fixture/provisioning" "$fixture/provisioning/datasources"
cat >"$fixture/prometheus.yml" <<'YAML'
global:
  scrape_interval: 15s
scrape_configs: []
YAML
cat >"$fixture/provisioning/datasources/prometheus.yml" <<'YAML'
apiVersion: 1
datasources:
  - name: Prometheus
    uid: maxposty-prometheus
    type: prometheus
    access: proxy
    url: http://prometheus:9090
    isDefault: true
    editable: false
    jsonData:
      httpMethod: POST
      prometheusType: Prometheus
      prometheusVersion: 3.15.0
      timeInterval: 15s
YAML
chmod 0644 "$fixture/prometheus.yml" "$fixture/provisioning/datasources/prometheus.yml"
docker pull --platform linux/amd64 "$GRAFANA_IMAGE"
docker pull --platform linux/amd64 "$PROMETHEUS_IMAGE"
docker network create --internal "$network" >/dev/null
docker run --rm --platform linux/amd64 --network "$network" \
  --entrypoint /bin/promtool \
  --mount "type=bind,src=$fixture/prometheus.yml,dst=/etc/prometheus/prometheus.yml,readonly" \
  "$PROMETHEUS_IMAGE" check config /etc/prometheus/prometheus.yml
docker run -d --name "$prometheus" --platform linux/amd64 \
  --network "$network" --network-alias prometheus \
  --read-only --cap-drop ALL --security-opt no-new-privileges \
  --tmpfs /prometheus:rw,nosuid,nodev,uid=65534,gid=65534 \
  --mount "type=bind,src=$fixture/prometheus.yml,dst=/etc/prometheus/prometheus.yml,readonly" \
  "$PROMETHEUS_IMAGE" >/dev/null
docker run -d --name "$grafana" --platform linux/amd64 \
  --network "$network" --read-only --cap-drop ALL --security-opt no-new-privileges \
  --tmpfs /var/lib/grafana:rw,nosuid,nodev,uid=472,gid=0 \
  --tmpfs /tmp:rw,nosuid,nodev,uid=472,gid=0 \
  --mount "type=bind,src=$fixture/provisioning,dst=/etc/grafana/provisioning,readonly" \
  --env GF_SECURITY_ADMIN_USER=smoke-admin \
  --env GF_SECURITY_ADMIN_PASSWORD=monitoring-isolated-fixture-password \
  --env GF_USERS_ALLOW_SIGN_UP=false \
  --env GF_ANALYTICS_REPORTING_ENABLED=false \
  --env GF_ANALYTICS_CHECK_FOR_UPDATES=false \
  --env GF_ANALYTICS_CHECK_FOR_PLUGIN_UPDATES=false \
  "$GRAFANA_IMAGE" >/dev/null
# Internal Docker networks do not provide host port mappings on every engine.
# Curl is part of the pinned Grafana base; query its real HTTP listener inside
# the same hardened container without exposing either fixture to the host.
base='http://127.0.0.1:3000'
ready=false
for (( attempt=0; attempt<60; attempt++ )); do
  if docker exec "$grafana" curl --silent --fail --max-time 3 "$base/api/health" >"$fixture/health.json" &&
    jq -e '.database == "ok"' "$fixture/health.json" >/dev/null; then
    ready=true
    break
  fi
  sleep 2
done
[[ "$ready" == true ]] || { echo 'Grafana did not become healthy' >&2; exit 1; }

api() {
  docker exec "$grafana" curl --silent --show-error --fail --max-time 30 \
    --user smoke-admin:monitoring-isolated-fixture-password "$@"
}
api "$base/api/plugins/prometheus/settings" >"$fixture/plugin.json"
jq -e '.id == "prometheus" and .signature == "internal" and
  .module == "public/plugins/prometheus/module.js"' "$fixture/plugin.json" >/dev/null
api "$base/api/plugins?type=datasource&core=1" >"$fixture/core-plugins.json"
jq -e 'any(.[]; .id == "prometheus" and .signature == "internal")' \
  "$fixture/core-plugins.json" >/dev/null
api --dump-header /tmp/maxposty-module.headers "$base/public/plugins/prometheus/module.js" \
  >"$fixture/module.js"
docker exec "$grafana" cat /tmp/maxposty-module.headers >"$fixture/module.headers"
test -s "$fixture/module.js"
grep -Eiq '^content-type: (application|text)/(javascript|x-javascript)' "$fixture/module.headers"
api "$base/api/datasources/uid/maxposty-prometheus/health" >"$fixture/datasource-health.json"
jq -e '.status == "OK"' "$fixture/datasource-health.json" >/dev/null
api --header 'Content-Type: application/json' --data \
  '{"from":"now-5m","to":"now","queries":[{"refId":"A","datasource":{"type":"prometheus","uid":"maxposty-prometheus"},"expr":"vector(2)","instant":true,"range":false,"format":"time_series","intervalMs":15000,"maxDataPoints":100}]}' \
  "$base/api/ds/query" >"$fixture/query.json"
jq -e '.results.A.error == null and any(.results.A.frames[]?;
  . as $frame | any(range(0; (.schema.fields | length));
    . as $index | $frame.schema.fields[$index].type == "number" and
    any($frame.data.values[$index][]?; . == 2)))' "$fixture/query.json" >/dev/null
echo 'Grafana internal Prometheus registration, frontend module and real backend query passed'
