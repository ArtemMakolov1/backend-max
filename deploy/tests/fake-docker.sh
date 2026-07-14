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
    *'.Labels'*) printf 'maxposty\n' ;;
  esac
  exit 0
fi

if [[ ${1:-} == "inspect" ]]; then
  container_id=${!#}
  if [[ "$container_id" == "new-backend-id" && "$TEST_SCENARIO" == "initial-health-fail" ]]; then
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
