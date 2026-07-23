#!/usr/bin/env bash
set -euo pipefail

: "${TEST_LOG:?TEST_LOG is required}"
: "${TEST_SCENARIO:?TEST_SCENARIO is required}"
: "${TEST_EMPTY_DIR:?TEST_EMPTY_DIR is required}"
: "${TEST_OLD_SHA:?TEST_OLD_SHA is required}"

printf '%s\n' "$*" >>"$TEST_LOG"

if [[ ${1:-} == "compose" && ${2:-} == "version" ]]; then
  exit 0
fi

if [[ ${1:-} == "network" && ${2:-} == "inspect" ]]; then
  case "$*" in
    *'.Driver'*) printf 'bridge\n' ;;
    *'.Scope'*) printf 'local\n' ;;
    *'.Labels'*'maxposty-monitoring-edge'*) printf 'monitoring-edge\n' ;;
    *'.IPAM.Config'*'maxposty-monitoring-edge'*) printf '172.29.42.0/24\n' ;;
    *'.Labels'*) printf 'maxposty\n' ;;
  esac
  exit 0
fi

if [[ ${1:-} == "inspect" ]]; then
  container_id=${!#}
  if [[ "$container_id" == "new-backend-id" && \
    ("$TEST_SCENARIO" == "initial-health-fail" || "$TEST_SCENARIO" == "new-backend-health-fail") ]]; then
    printf 'unhealthy\n'
  elif [[ "$container_id" == "new-grafana-id" && "$TEST_SCENARIO" == "monitoring-fail" ]]; then
    printf 'unhealthy\n'
  else
    printf 'healthy\n'
  fi
  exit 0
fi

if [[ ${1:-} != "compose" ]]; then
  echo "Unexpected fake Docker command: $*" >&2
  exit 64
fi

compose_file=''
arguments=("$@")
for ((index = 0; index < ${#arguments[@]}; index++)); do
  case "${arguments[$index]}" in
    --env-file)
      index=$((index + 1))
      env_file=${arguments[$index]:-}
      if [[ ! -f "$env_file" ]]; then
        echo "missing-env:$env_file" >>"$TEST_LOG"
        exit 91
      fi
      ;;
    --file)
      index=$((index + 1))
      compose_file=${arguments[$index]:-}
      ;;
  esac
done

bundle_kind=new
if [[ -n "$compose_file" && "$compose_file" == *"/$TEST_OLD_SHA/"* ]]; then
  bundle_kind=old
fi

if [[ "$*" == *" ps -q "* ]]; then
  service=${!#}
  printf '%s-%s-id\n' "$bundle_kind" "$service"
  exit 0
fi

if [[ "$*" == *" exec -T postgres-exporter "*"/metrics"* ]]; then
  # Real exporter responses are much larger than a pipe buffer. Keep the
  # connection metric first so a quiet grep reproduces the production SIGPIPE.
  awk 'BEGIN {
    print "pg_up 1"
    for (i = 0; i < 8192; i++) print "# maxposty postgres exporter metrics padding"
  }'
  exit 0
fi

if [[ "$*" == *" exec -T pgbouncer-exporter "*"/metrics"* ]]; then
  awk 'BEGIN {
    print "pgbouncer_up 1"
    for (i = 0; i < 8192; i++) print "# maxposty pgbouncer exporter metrics padding"
  }'
  exit 0
fi

if [[ "$*" == *" exec -T prometheus "*"/api/v1/query"* ]]; then
  printf '{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,"1"]}]}}\n'
  exit 0
fi

if [[ "$*" == *" exec -T prometheus "*"/api/v1/alertmanagers"* ]]; then
  printf '{"status":"success","data":{"activeAlertmanagers":[{"url":"http://alertmanager:9093/api/v2/alerts"}]}}\n'
  exit 0
fi

if [[ "$*" == *" exec -T grafana "*"/monitoring/api/datasources/uid/maxposty-prometheus"* ]]; then
  printf '{"uid":"maxposty-prometheus"}\n'
  exit 0
fi

if [[ "$*" == *" exec -T grafana "*"/monitoring/api/dashboards/uid/maxposty-overview"* ]]; then
  printf '{"dashboard":{"uid":"maxposty-overview"}}\n'
  exit 0
fi

if [[ "$*" == *" exec -T grafana "*"/monitoring/api/dashboards/uid/maxposty-application"* ]]; then
  printf '{"dashboard":{"uid":"maxposty-application"}}\n'
  exit 0
fi

if [[ "$*" == *" exec -T grafana "*"/monitoring/api/dashboards/uid/maxposty-infrastructure"* ]]; then
  printf '{"dashboard":{"uid":"maxposty-infrastructure"}}\n'
  exit 0
fi

if [[ "$*" == *"pg_dump"* ]]; then
  if [[ "$TEST_SCENARIO" == "backup-fail" ]]; then
    exit 41
  fi
  printf 'mock-custom-dump\n'
  exit 0
fi

if [[ "$*" == *"media-backup"* ]]; then
  tar -czf - -C "$TEST_EMPTY_DIR" .
  exit 0
fi

exit 0
