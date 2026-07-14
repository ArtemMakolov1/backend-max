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

backup_temporary=''
media_temporary=''
stack_mutated=false
writes_frozen=false
restore_previous_release() {
  local exit_status=$?
  local restore_failed=false
  [[ -z "$backup_temporary" ]] || rm -f "$backup_temporary"
  [[ -z "$media_temporary" ]] || rm -f "$media_temporary"
  if ((exit_status == 0)); then
    return 0
  fi

  set +e
  if [[ -n "$current_dir" && ("$stack_mutated" == "true" || "$writes_frozen" == "true") ]]; then
    echo "Deployment failed; restoring the previous versioned backend bundle" >&2
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
    # Always attempt to start the previous backend, even if an infrastructure
    # dependency needs manual recovery, so the trap never deliberately leaves
    # the accepted application container in a stopped state.
    if ! compose "$current_dir" "$current_env" "$current_release" up -d --no-deps --force-recreate backend; then
      restore_failed=true
    fi
    if ! wait_for_health "$current_dir" "$current_env" "$current_release" backend 40; then
      restore_failed=true
    fi
    if [[ "$restore_failed" == "true" ]]; then
      echo "Previous bundle did not recover fully; manual recovery is required" >&2
    fi
  elif [[ -z "$current_dir" && "$stack_mutated" == "true" ]]; then
    echo "Initial deployment failed; removing unaccepted containers while preserving data volumes" >&2
    compose "$release_dir" "$next_env" "$next_release" down --remove-orphans >/dev/null 2>&1 || true
  fi

  # Rollback commands above still need the staged files, so clean them only
  # after the previous stack or initial-deploy containers have been handled.
  rm -f "$next_env" "$next_release"
  if [[ "$current_dir" != "$release_dir" ]]; then
    rm -f "$release_dir/.env.production" "$release_dir/.release"
  fi
  return "$exit_status"
}
trap restore_previous_release EXIT

compose "$release_dir" "$next_env" "$next_release" config >/dev/null
compose "$release_dir" "$next_env" "$next_release" pull backend migrate

stack_mutated=true
compose "$release_dir" "$next_env" "$next_release" up -d postgres
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

# Freeze application writes while the database and media volume are captured,
# copied off-host (in production), and the additive migration is applied. This
# intentionally trades a bounded maintenance window for a coherent restore point.
if [[ -n "$current_dir" ]]; then
  writes_frozen=true
  compose "$current_dir" "$current_env" "$current_release" stop backend
fi

backup_dir="$installation_dir/backups"
mkdir -p "$backup_dir"
chmod 700 "$backup_dir"
backup_timestamp=$(date -u +%Y%m%dT%H%M%SZ)
backup_temporary="$backup_dir/postgres-${backup_timestamp}.dump.tmp"
backup_final="$backup_dir/postgres-${backup_timestamp}.dump"
media_temporary="$backup_dir/media-${backup_timestamp}.tar.gz.tmp"
media_final="$backup_dir/media-${backup_timestamp}.tar.gz"
manifest_final="$backup_dir/backup-${backup_timestamp}.sha256"

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

compose "$release_dir" "$next_env" "$next_release" --profile ops run --rm --no-deps --quiet-pull -T media-backup \
  -C /source -czf - . >"$media_temporary"
if [[ ! -s "$media_temporary" ]]; then
  echo "Pre-migration media backup is empty or could not be created" >&2
  exit 1
fi
if ! tar -tzf "$media_temporary" >/dev/null; then
  echo "Pre-migration media backup did not pass archive validation" >&2
  exit 1
fi

chmod 600 "$backup_temporary" "$media_temporary"
mv "$backup_temporary" "$backup_final"
mv "$media_temporary" "$media_final"
backup_temporary=''
media_temporary=''
(
  cd "$backup_dir"
  sha256sum "$(basename "$backup_final")" "$(basename "$media_final")" >"$(basename "$manifest_final")"
)
chmod 600 "$manifest_final"

backup_hook="$installation_dir/hooks/after-backup"
bootstrap_mode=$(env_value "$next_env" AUTH_BOOTSTRAP_MODE)
if [[ "$bootstrap_mode" == "false" ]]; then
  if [[ ! -x "$backup_hook" ]]; then
    echo "Production requires executable offsite backup hook: $backup_hook" >&2
    exit 1
  fi
  snapshot_image=$image
  snapshot_source_sha=$release_id
  if [[ -n "$current_dir" ]]; then
    snapshot_image=$(env_value "$current_release" BACKEND_IMAGE)
    snapshot_source_sha=$(basename -- "$current_dir")
  fi
  MAXPOSTY_BACKUP_SNAPSHOT_IMAGE="$snapshot_image" \
    MAXPOSTY_BACKUP_SNAPSHOT_SOURCE_SHA="$snapshot_source_sha" \
    "$backup_hook" "$backup_final" "$media_final" "$manifest_final" "$image"
else
  echo "Bootstrap warning: encrypted offsite publication is disabled; local snapshots only" >&2
fi

retention_days=$(env_value "$next_env" BACKUP_RETENTION_DAYS)
find "$backup_dir" -type f \( -name 'postgres-*.dump' -o -name 'media-*.tar.gz' -o -name 'backup-*.sha256' \) \
  -mtime "+$retention_days" -delete

compose "$release_dir" "$next_env" "$next_release" run --rm --no-deps migrate
compose "$release_dir" "$next_env" "$next_release" up -d --no-deps --force-recreate backend

if ! wait_for_health "$release_dir" "$next_env" "$next_release" backend 40; then
  compose "$release_dir" "$next_env" "$next_release" logs --tail=120 --no-color backend >&2 || true
  echo "New backend did not become healthy; database remains on the additive schema and the previous bundle will be restored" >&2
  exit 1
fi

mv -f "$next_env" "$release_dir/.env.production"
mv -f "$next_release" "$release_dir/.release"
chmod 600 "$release_dir/.env.production" "$release_dir/.release"
temporary_link="$installation_dir/.current-${release_id}"
rm -f "$temporary_link"
ln -s "$release_dir" "$temporary_link"
mv -Tf "$temporary_link" "$current_link"

trap - EXIT
echo "Backend deployment is healthy at immutable digest: $image"
