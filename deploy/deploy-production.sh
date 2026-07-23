#!/usr/bin/env bash
set -euo pipefail

image=${1:-}
if [[ ! "$image" =~ ^ghcr\.io/[a-z0-9._-]+/[a-z0-9._/-]+@sha256:[0-9a-f]{64}$ ]]; then
  echo "Deployment image must be an immutable lowercase GHCR digest reference" >&2
  exit 2
fi

for command_name in awk date docker find flock ln mv readlink sha256sum tar; do
  if ! command -v "$command_name" >/dev/null 2>&1; then
    echo "Required deployment command is missing: $command_name" >&2
    exit 1
  fi
done
if ! docker compose version >/dev/null 2>&1; then
  echo "Docker Compose v2 is required" >&2
  exit 1
fi

release_dir=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
release_id=$(basename "$release_dir")
releases_dir=$(dirname "$release_dir")
installation_dir=$(dirname "$releases_dir")
if [[ "$(basename "$releases_dir")" != "releases" || ! "$release_id" =~ ^[0-9a-f]{40}$ ]]; then
  echo "Deployment bundle must run from INSTALL_DIR/releases/40_CHARACTER_COMMIT_SHA" >&2
  exit 1
fi
cd "$release_dir"

exec 9>"$installation_dir/.deploy.lock"
if ! flock -n 9; then
  echo "Another backend deployment is already running" >&2
  exit 1
fi

next_env="$release_dir/.env.production.next"
next_release="$release_dir/.release.next"
validator="$release_dir/deploy/validate-production-env.sh"
alertmanager_renderer="$release_dir/deploy/render-alertmanager-config.sh"
alertmanager_config="$release_dir/.alertmanager.yml"
current_link="$installation_dir/current"
current_dir=''
current_env=''
current_release=''

if [[ ! -f "$next_env" ]]; then
  echo "The deployment did not provide .env.production.next" >&2
  exit 1
fi
chmod 600 "$next_env"
"$validator" "$next_env"

env_value() {
  local file=$1
  local requested_key=$2
  awk -F= -v requested_key="$requested_key" '$1 == requested_key { sub(/^[^=]*=/, ""); print; exit }' "$file"
}

if [[ -L "$current_link" ]]; then
  current_dir=$(readlink -f "$current_link")
  case "$current_dir/" in
    "$releases_dir/"*) ;;
    *)
      echo "Current release symlink points outside $releases_dir" >&2
      exit 1
      ;;
  esac
  current_env="$current_dir/.env.production"
  current_release="$current_dir/.release"
  if [[ ! -f "$current_env" || ! -f "$current_release" || ! -x "$current_dir/deploy/validate-production-env.sh" ]]; then
    echo "Current release is incomplete and cannot be rolled back safely" >&2
    exit 1
  fi
  "$current_dir/deploy/validate-production-env.sh" "$current_env"
  for key in POSTGRES_OWNER_PASSWORD POSTGRES_APP_PASSWORD; do
    if [[ "$(env_value "$current_env" "$key")" != "$(env_value "$next_env" "$key")" ]]; then
      echo "$key cannot be rotated by an application deployment; use the documented database credential rotation procedure" >&2
      exit 1
    fi
  done
  for key in POSTGRES_MONITOR_PASSWORD GRAFANA_SECRET_KEY; do
    if grep -q "^${key}=" "$current_env" && [[ "$(env_value "$current_env" "$key")" != "$(env_value "$next_env" "$key")" ]]; then
      echo "$key cannot be rotated by an application deployment; use the documented monitoring credential rotation procedure" >&2
      exit 1
    fi
  done
elif [[ -e "$current_link" ]]; then
  echo "$current_link must be a symlink to a versioned release" >&2
  exit 1
fi

ensure_edge_network() {
  local network_name=maxposty-edge
  local infrastructure_dir network_driver network_scope network_label
  infrastructure_dir=$(dirname "$installation_dir")

  exec 8>"$infrastructure_dir/.maxposty-edge-network.lock"
  if ! flock -w 30 8; then
    echo "Timed out waiting to configure the shared $network_name Docker network" >&2
    return 1
  fi
  if ! docker network inspect "$network_name" >/dev/null 2>&1; then
    docker network create \
      --driver bridge \
      --attachable \
      --label com.maxposty.stack=maxposty \
      "$network_name" >/dev/null 2>&1 || true
  fi

  if ! docker network inspect "$network_name" >/dev/null 2>&1; then
    echo "Could not create the shared $network_name Docker network" >&2
    return 1
  fi
  network_driver=$(docker network inspect --format '{{.Driver}}' "$network_name")
  network_scope=$(docker network inspect --format '{{.Scope}}' "$network_name")
  network_label=$(docker network inspect --format '{{index .Labels "com.maxposty.stack"}}' "$network_name")
  if [[ "$network_driver" != "bridge" || "$network_scope" != "local" || "$network_label" != "maxposty" ]]; then
    echo "Existing $network_name network does not match the expected local MaxPosty bridge" >&2
    return 1
  fi
  flock -u 8
  exec 8>&-
}

ensure_edge_network

ensure_monitoring_edge_network() {
  local network_name=maxposty-monitoring-edge
  local infrastructure_dir network_driver network_scope network_label network_subnet
  infrastructure_dir=$(dirname "$installation_dir")

  exec 7>"$infrastructure_dir/.maxposty-monitoring-edge-network.lock"
  if ! flock -w 30 7; then
    echo "Timed out waiting to configure the shared $network_name Docker network" >&2
    return 1
  fi
  if ! docker network inspect "$network_name" >/dev/null 2>&1; then
    docker network create \
      --driver bridge \
      --attachable \
      --subnet 172.29.42.0/24 \
      --label com.maxposty.stack=maxposty \
      --label com.maxposty.purpose=monitoring-edge \
      "$network_name" >/dev/null 2>&1 || true
  fi

  if ! docker network inspect "$network_name" >/dev/null 2>&1; then
    echo "Could not create the shared $network_name Docker network" >&2
    return 1
  fi
  network_driver=$(docker network inspect --format '{{.Driver}}' "$network_name")
  network_scope=$(docker network inspect --format '{{.Scope}}' "$network_name")
  network_label=$(docker network inspect --format '{{index .Labels "com.maxposty.purpose"}}' "$network_name")
  network_subnet=$(docker network inspect --format '{{(index .IPAM.Config 0).Subnet}}' "$network_name")
  if [[ "$network_driver" != "bridge" || "$network_scope" != "local" || \
    "$network_label" != "monitoring-edge" || "$network_subnet" != "172.29.42.0/24" ]]; then
    echo "Existing $network_name network does not match the isolated monitoring contract" >&2
    return 1
  fi
  flock -u 7
  exec 7>&-
}

ensure_monitoring_edge_network

printf 'BACKEND_IMAGE=%s\n' "$image" >"$next_release"
chmod 600 "$next_release"

compose() {
  local bundle_dir=$1
  local environment_file=$2
  local release_file=$3
  shift 3
  docker compose \
    --project-name maxposty-backend \
    --env-file "$environment_file" \
    --env-file "$release_file" \
    --file "$bundle_dir/deploy/compose.production.yaml" \
    "$@"
}

wait_for_health() {
  local bundle_dir=$1
  local environment_file=$2
  local release_file=$3
  local service=$4
  local attempts=${5:-40}
  local container_id status
  for ((attempt = 1; attempt <= attempts; attempt++)); do
    container_id=$(compose "$bundle_dir" "$environment_file" "$release_file" ps -q "$service")
    if [[ -n "$container_id" ]]; then
      status=$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$container_id" 2>/dev/null || true)
      case "$status" in
        healthy|running)
          return 0
          ;;
        unhealthy|exited|dead)
          return 1
          ;;
      esac
    fi
    sleep 3
  done
  return 1
}

wait_for_prometheus_target() {
  local bundle_dir=$1
  local environment_file=$2
  local release_file=$3
  local job=$4
  local encoded_job response
  # Job names are fixed by monitoring/prometheus/prometheus.yml. Keep the URL
  # encoding explicit so no deployment input is interpolated into PromQL.
  case "$job" in
    maxposty-backend) encoded_job=maxposty-backend ;;
    postgres) encoded_job=postgres ;;
    pgbouncer) encoded_job=pgbouncer ;;
    node) encoded_job=node ;;
    alertmanager) encoded_job=alertmanager ;;
    *) return 2 ;;
  esac
  for ((attempt = 1; attempt <= 20; attempt++)); do
    response=$(compose "$bundle_dir" "$environment_file" "$release_file" exec -T prometheus \
      wget -q -O - "http://127.0.0.1:9090/api/v1/query?query=up%7Bjob%3D%22${encoded_job}%22%7D" 2>/dev/null || true)
    if grep -Eq '"value":\[[^]]*,"1"\]' <<<"$response"; then
      return 0
    fi
    sleep 3
  done
  return 1
}

wait_for_prometheus_alertmanager() {
  local bundle_dir=$1
  local environment_file=$2
  local release_file=$3
  local response
  for ((attempt = 1; attempt <= 20; attempt++)); do
    response=$(compose "$bundle_dir" "$environment_file" "$release_file" exec -T prometheus \
      wget -q -O - http://127.0.0.1:9090/api/v1/alertmanagers 2>/dev/null || true)
    if grep -Fq 'http://alertmanager:9093/api/v2/alerts' <<<"$response"; then
      return 0
    fi
    sleep 3
  done
  return 1
}

wait_for_grafana_provisioning() {
  local bundle_dir=$1
  local environment_file=$2
  local release_file=$3
  local resource response
  local resources=(
    'datasources/uid/maxposty-prometheus|maxposty-prometheus'
    'dashboards/uid/maxposty-overview|maxposty-overview'
    'dashboards/uid/maxposty-application|maxposty-application'
    'dashboards/uid/maxposty-infrastructure|maxposty-infrastructure'
  )

  for ((attempt = 1; attempt <= 20; attempt++)); do
    local all_ready=true
    for resource in "${resources[@]}"; do
      local api_path=${resource%%|*}
      local expected_uid=${resource##*|}
      response=$(compose "$bundle_dir" "$environment_file" "$release_file" exec -T grafana \
        wget -q -O - \
        --header='X-WEBAUTH-USER: maxposty-deploy-check' \
        --header='X-WEBAUTH-ROLE: Viewer' \
        "http://127.0.0.1:3000/monitoring/api/${api_path}" 2>/dev/null || true)
      if ! grep -Fq "\"uid\":\"${expected_uid}\"" <<<"$response"; then
        all_ready=false
        break
      fi
    done
    if [[ "$all_ready" == "true" ]]; then
      return 0
    fi
    sleep 3
  done
  return 1
}

has_monitoring_stack() {
  local bundle_dir=$1
  grep -q '^  prometheus:$' "$bundle_dir/deploy/compose.production.yaml"
}

has_alertmanager_stack() {
  local bundle_dir=$1
  grep -q '^  alertmanager:$' "$bundle_dir/deploy/compose.production.yaml"
}

start_monitoring() {
  local bundle_dir=$1
  local environment_file=$2
  local release_file=$3
  local service

  if ! compose "$bundle_dir" "$environment_file" "$release_file" up -d --no-deps --force-recreate \
    postgres-exporter pgbouncer-exporter node-exporter; then
    echo "Monitoring exporters could not be recreated" >&2
    return 1
  fi
  for service in postgres-exporter pgbouncer-exporter node-exporter; do
    if ! wait_for_health "$bundle_dir" "$environment_file" "$release_file" "$service" 30; then
      compose "$bundle_dir" "$environment_file" "$release_file" logs --tail=120 --no-color "$service" >&2 || true
      echo "$service did not become healthy" >&2
      return 1
    fi
  done

  # An exporter HTTP endpoint can be healthy while its database connection is
  # broken. Gate the release on the exporters' own connection metrics instead
  # of accepting a stale or disconnected monitoring stack. Do not use grep -q
  # here: with pipefail it closes a real, long metrics stream early and turns
  # the producer's expected SIGPIPE into a false deployment failure.
  if ! compose "$bundle_dir" "$environment_file" "$release_file" exec -T postgres-exporter \
    wget -q -O - http://127.0.0.1:9187/metrics | grep -E '^pg_up 1(\.0)?$' >/dev/null; then
    compose "$bundle_dir" "$environment_file" "$release_file" logs --tail=120 --no-color postgres-exporter >&2 || true
    echo "PostgreSQL exporter is healthy but cannot query PostgreSQL" >&2
    return 1
  fi
  if ! compose "$bundle_dir" "$environment_file" "$release_file" exec -T pgbouncer-exporter \
    wget -q -O - http://127.0.0.1:9127/metrics | grep -E '^pgbouncer_up 1(\.0)?$' >/dev/null; then
    compose "$bundle_dir" "$environment_file" "$release_file" logs --tail=120 --no-color pgbouncer-exporter >&2 || true
    echo "PgBouncer exporter is healthy but cannot query PgBouncer" >&2
    return 1
  fi

  if has_alertmanager_stack "$bundle_dir"; then
    if ! compose "$bundle_dir" "$environment_file" "$release_file" up -d --no-deps --force-recreate alertmanager; then
      echo "Alertmanager could not be recreated" >&2
      return 1
    fi
    if ! wait_for_health "$bundle_dir" "$environment_file" "$release_file" alertmanager 30; then
      compose "$bundle_dir" "$environment_file" "$release_file" logs --tail=120 --no-color alertmanager >&2 || true
      echo "Alertmanager did not become healthy" >&2
      return 1
    fi
  fi

  if ! compose "$bundle_dir" "$environment_file" "$release_file" up -d --no-deps --force-recreate prometheus; then
    echo "Prometheus could not be recreated" >&2
    return 1
  fi
  if ! wait_for_health "$bundle_dir" "$environment_file" "$release_file" prometheus 30; then
    compose "$bundle_dir" "$environment_file" "$release_file" logs --tail=120 --no-color prometheus >&2 || true
    echo "Prometheus did not become healthy" >&2
    return 1
  fi
  for service in maxposty-backend postgres pgbouncer node; do
    if ! wait_for_prometheus_target "$bundle_dir" "$environment_file" "$release_file" "$service"; then
      compose "$bundle_dir" "$environment_file" "$release_file" logs --tail=120 --no-color prometheus >&2 || true
      echo "Prometheus target is missing or down: $service" >&2
      return 1
    fi
  done
  if has_alertmanager_stack "$bundle_dir"; then
    if ! wait_for_prometheus_target "$bundle_dir" "$environment_file" "$release_file" alertmanager; then
      compose "$bundle_dir" "$environment_file" "$release_file" logs --tail=120 --no-color prometheus >&2 || true
      echo "Prometheus target is missing or down: alertmanager" >&2
      return 1
    fi
    if ! wait_for_prometheus_alertmanager "$bundle_dir" "$environment_file" "$release_file"; then
      compose "$bundle_dir" "$environment_file" "$release_file" logs --tail=120 --no-color prometheus >&2 || true
      echo "Prometheus has not activated the internal Alertmanager endpoint" >&2
      return 1
    fi
  fi

  if ! compose "$bundle_dir" "$environment_file" "$release_file" up -d --no-deps --force-recreate grafana; then
    echo "Grafana could not be recreated" >&2
    return 1
  fi
  if ! wait_for_health "$bundle_dir" "$environment_file" "$release_file" grafana 30; then
    compose "$bundle_dir" "$environment_file" "$release_file" logs --tail=120 --no-color grafana >&2 || true
    echo "Grafana did not become healthy" >&2
    return 1
  fi
  if ! wait_for_grafana_provisioning "$bundle_dir" "$environment_file" "$release_file"; then
    compose "$bundle_dir" "$environment_file" "$release_file" logs --tail=200 --no-color grafana >&2 || true
    echo "Grafana is healthy but required dashboards or the Prometheus datasource were not provisioned" >&2
    return 1
  fi
}

backup_temporary=''
stack_mutated=false
writes_frozen=false
schema_advanced=false
new_backend_healthy=false
release_activated=false

activate_release_bundle() {
  if [[ "$release_activated" == "true" ]]; then
    return 0
  fi
  if [[ -f "$next_env" ]]; then
    mv -f "$next_env" "$release_dir/.env.production"
  elif [[ ! -f "$release_dir/.env.production" ]]; then
    echo "Release activation is missing its production environment" >&2
    return 1
  fi
  if [[ -f "$next_release" ]]; then
    mv -f "$next_release" "$release_dir/.release"
  elif [[ ! -f "$release_dir/.release" ]]; then
    echo "Release activation is missing its immutable image metadata" >&2
    return 1
  fi
  chmod 600 "$release_dir/.env.production" "$release_dir/.release"
  local temporary_link="$installation_dir/.current-${release_id}"
  rm -f "$temporary_link"
  ln -s "$release_dir" "$temporary_link"
  mv -Tf "$temporary_link" "$current_link"
  release_activated=true
}

restore_previous_release() {
  local exit_status=$?
  local restore_failed=false
  [[ -z "$backup_temporary" ]] || rm -f "$backup_temporary"
  if ((exit_status == 0)); then
    return 0
  fi

  set +e
  if [[ -n "$current_dir" && ("$stack_mutated" == "true" || "$writes_frozen" == "true") ]]; then
    if has_monitoring_stack "$release_dir"; then
      compose "$release_dir" "$next_env" "$next_release" stop grafana prometheus alertmanager postgres-exporter pgbouncer-exporter node-exporter >/dev/null 2>&1 || true
    fi

    if [[ "$schema_advanced" == "true" ]]; then
      echo "Deployment failed after schema advancement; refusing to start the retired backend against the new schema" >&2
      if [[ "$new_backend_healthy" == "true" ]]; then
        # The application has already passed its health gate. Keep it and the
        # new S3/schema semantics active, then fall back only the monitoring
        # services. This is a roll-forward application recovery, not rollback.
        if ! activate_release_bundle; then
          restore_failed=true
        fi
        if has_monitoring_stack "$current_dir"; then
          if ! start_monitoring "$current_dir" "$current_env" "$current_release"; then
            restore_failed=true
          fi
        fi
        if [[ "$restore_failed" == "true" ]]; then
          echo "The new backend remains required, but monitoring or release activation needs manual recovery" >&2
        else
          echo "The healthy new backend remains active; previous monitoring was restored for operator recovery" >&2
        fi
      else
        # Migrations are applied transaction-by-transaction. Even when the
        # migrator exits non-zero, an earlier cutover migration may already be
        # committed, so starting the old local-media binary is never safe.
        compose "$release_dir" "$next_env" "$next_release" stop backend >/dev/null 2>&1 || true
        echo "Backend is fail-closed; complete a roll-forward deployment from the preserved staged bundle" >&2
      fi
    else
      echo "Deployment failed before schema advancement; restoring the previous versioned backend bundle" >&2
      # Restore the accepted release's private Alertmanager configuration before
      # its monitoring containers are recreated. This also makes secret rotation
      # rollback deterministic instead of leaving the rejected config in the
      # shared volume.
      if has_alertmanager_stack "$current_dir"; then
        if ! compose "$current_dir" "$current_env" "$current_release" up --no-deps --force-recreate runtime-storage-init; then
          restore_failed=true
        fi
      fi
      if ! compose "$current_dir" "$current_env" "$current_release" up -d --no-deps --force-recreate postgres; then
        restore_failed=true
      elif ! wait_for_health "$current_dir" "$current_env" "$current_release" postgres 40; then
        restore_failed=true
      fi
      if ! compose "$current_dir" "$current_env" "$current_release" up -d --no-deps --force-recreate pgbouncer; then
        restore_failed=true
      elif ! wait_for_health "$current_dir" "$current_env" "$current_release" pgbouncer 30; then
        restore_failed=true
      fi
      # The previous backend is safe only because no migration has started.
      if ! compose "$current_dir" "$current_env" "$current_release" up -d --no-deps --force-recreate backend; then
        restore_failed=true
      fi
      if ! wait_for_health "$current_dir" "$current_env" "$current_release" backend 40; then
        restore_failed=true
      fi
      if has_monitoring_stack "$current_dir"; then
        if ! start_monitoring "$current_dir" "$current_env" "$current_release"; then
          restore_failed=true
        fi
      fi
      if [[ "$restore_failed" == "true" ]]; then
        echo "Previous bundle did not recover fully; manual recovery is required" >&2
      fi
    fi
  elif [[ -z "$current_dir" && "$stack_mutated" == "true" ]]; then
    if [[ "$schema_advanced" == "true" ]]; then
      echo "Initial deployment failed after schema advancement; preserving the staged bundle for roll-forward recovery" >&2
      compose "$release_dir" "$next_env" "$next_release" stop backend grafana prometheus alertmanager postgres-exporter pgbouncer-exporter node-exporter >/dev/null 2>&1 || true
    else
      echo "Initial deployment failed; removing unaccepted containers while preserving data volumes" >&2
      compose "$release_dir" "$next_env" "$next_release" down --remove-orphans >/dev/null 2>&1 || true
    fi
  fi

  # Once migration starts, staged metadata is the recovery input for the only
  # safe direction: roll-forward. Before that point it is safe to discard.
  if [[ "$schema_advanced" != "true" || "$new_backend_healthy" == "true" ]]; then
    rm -f "$next_env" "$next_release"
    if [[ "$release_activated" != "true" && "$current_dir" != "$release_dir" ]]; then
      rm -f "$release_dir/.env.production" "$release_dir/.release" "$alertmanager_config"
    fi
  fi
  return "$exit_status"
}
trap restore_previous_release EXIT

install -d -m 755 "$installation_dir/runtime" "$installation_dir/runtime/metrics"
"$alertmanager_renderer" "$next_env" "$alertmanager_config"
compose "$release_dir" "$next_env" "$next_release" config >/dev/null
compose "$release_dir" "$next_env" "$next_release" pull \
  backend migrate runtime-storage-init postgres pgbouncer postgres-exporter pgbouncer-exporter node-exporter alertmanager prometheus grafana

stack_mutated=true

# Freeze the old backend before Compose is allowed to recreate PostgreSQL or
# PgBouncer. This avoids serving requests through a database restart during an
# observability/configuration rollout.
if [[ -n "$current_dir" ]]; then
  writes_frozen=true
  compose "$current_dir" "$current_env" "$current_release" stop backend
fi

if ! compose "$release_dir" "$next_env" "$next_release" up -d postgres; then
  compose "$release_dir" "$next_env" "$next_release" logs --tail=120 --no-color runtime-storage-init postgres >&2 || true
  echo "PostgreSQL startup prerequisites failed" >&2
  exit 1
fi
if ! wait_for_health "$release_dir" "$next_env" "$next_release" postgres 40; then
  compose "$release_dir" "$next_env" "$next_release" logs --tail=120 --no-color postgres >&2 || true
  echo "PostgreSQL did not become healthy" >&2
  exit 1
fi
compose "$release_dir" "$next_env" "$next_release" up -d pgbouncer
if ! wait_for_health "$release_dir" "$next_env" "$next_release" pgbouncer 30; then
  compose "$release_dir" "$next_env" "$next_release" logs --tail=120 --no-color pgbouncer >&2 || true
  echo "PgBouncer did not become healthy" >&2
  exit 1
fi

# Application writes stay frozen while a validated local database snapshot is
# created and the additive migration runs. Media already lives in S3 and is not
# copied during a deploy; encrypted offsite database/media backups are handled
# by the independent scheduled backup workflow.

backup_dir="$installation_dir/backups"
mkdir -p "$backup_dir"
chmod 700 "$backup_dir"
backup_timestamp=$(date -u +%Y%m%dT%H%M%SZ)
backup_temporary="$backup_dir/postgres-${backup_timestamp}.dump.tmp"
backup_final="$backup_dir/postgres-${backup_timestamp}.dump"

# The variables in this command are intentionally expanded inside PostgreSQL's
# container, not by this host-side script.
# shellcheck disable=SC2016
compose "$release_dir" "$next_env" "$next_release" exec -T postgres sh -ec \
  'PGPASSWORD="$POSTGRES_PASSWORD" pg_dump --format=custom --no-password --username="$POSTGRES_USER" --dbname="$POSTGRES_DB"' \
  >"$backup_temporary"
if [[ ! -s "$backup_temporary" ]]; then
  echo "Pre-migration PostgreSQL backup is empty" >&2
  exit 1
fi
if ! compose "$release_dir" "$next_env" "$next_release" exec -T postgres \
  pg_restore --list <"$backup_temporary" >/dev/null; then
  echo "Pre-migration PostgreSQL backup did not pass pg_restore validation" >&2
  exit 1
fi

chmod 600 "$backup_temporary"
mv "$backup_temporary" "$backup_final"
backup_temporary=''

retention_days=$(env_value "$next_env" BACKUP_RETENTION_DAYS)
find "$backup_dir" -type f -name 'postgres-*.dump' \
  -mtime "+$retention_days" -delete

schema_advanced=true
compose "$release_dir" "$next_env" "$next_release" run --rm --no-deps migrate
compose "$release_dir" "$next_env" "$next_release" up -d --no-deps --force-recreate backend

if ! wait_for_health "$release_dir" "$next_env" "$next_release" backend 40; then
  compose "$release_dir" "$next_env" "$next_release" logs --tail=120 --no-color backend >&2 || true
  echo "New backend did not become healthy after schema advancement; automatic backend rollback is disabled" >&2
  exit 1
fi
new_backend_healthy=true

if ! start_monitoring "$release_dir" "$next_env" "$next_release"; then
  echo "The monitoring stack did not become healthy; the new backend will remain active and monitoring will fall back" >&2
  exit 1
fi

activate_release_bundle

trap - EXIT
echo "Backend deployment is healthy at immutable digest: $image"
