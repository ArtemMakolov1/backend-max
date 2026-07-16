#!/usr/bin/env bash
set -euo pipefail

repository=${1:-}
if [[ ! "$repository" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]]; then
  echo "Usage: $0 OWNER/REPOSITORY (GitHub token on standard input)" >&2
  exit 2
fi

for command_name in awk date docker find flock install mktemp openssl readlink sha256sum tar; do
  command -v "$command_name" >/dev/null 2>&1 || {
    echo "Required backup command is missing: $command_name" >&2
    exit 1
  }
done
docker compose version >/dev/null 2>&1 || { echo "Docker Compose v2 is required" >&2; exit 1; }

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
release_dir=$(CDPATH='' cd -- "$script_dir/../.." && pwd)
releases_dir=$(dirname -- "$release_dir")
installation_dir=$(dirname -- "$releases_dir")
current_link="$installation_dir/current"
current_dir=$(readlink -f "$current_link" 2>/dev/null || true)
if [[ "$(basename -- "$releases_dir")" != releases || "$current_dir" != "$release_dir" || ! "$(basename -- "$release_dir")" =~ ^[0-9a-f]{40}$ ]]; then
  echo "Scheduled backup must run from the currently accepted versioned release" >&2
  exit 1
fi

environment_file="$release_dir/.env.production"
release_file="$release_dir/.release"
"$release_dir/deploy/validate-production-env.sh" "$environment_file"
env_value() {
  local file=$1
  local requested_key=$2
  awk -F= -v requested_key="$requested_key" '$1 == requested_key { sub(/^[^=]*=/, ""); print; exit }' "$file"
}
if [[ "$(env_value "$environment_file" AUTH_BOOTSTRAP_MODE)" != false ]]; then
  echo "Scheduled encrypted backups are production-only" >&2
  exit 1
fi
image=$(env_value "$release_file" BACKEND_IMAGE)
source_sha=$(basename -- "$release_dir")
[[ "$image" =~ ^ghcr\.io/[a-z0-9._-]+/[a-z0-9._/-]+@sha256:[0-9a-f]{64}$ ]] || {
  echo "Accepted release does not contain an immutable backend image" >&2
  exit 1
}

# Share the deployment lock so an out-of-band deployment cannot restart the
# database halfway through a restore drill or physical base backup.
exec 9>"$installation_dir/.deploy.lock"
if ! flock -w 300 9; then
  echo "Timed out waiting for the production deployment lock" >&2
  exit 1
fi

token=''
IFS= read -r token || [[ -n "$token" ]]
if [[ ${#token} -lt 20 ]]; then
  echo "A short-lived GitHub Actions token must be provided on standard input" >&2
  exit 1
fi

compose() {
  docker compose \
    --project-name maxposty-backend \
    --env-file "$environment_file" \
    --env-file "$release_file" \
    --file "$release_dir/deploy/compose.production.yaml" \
    "$@"
}

metrics_dir="$installation_dir/runtime/metrics"
backup_dir="$installation_dir/backups"
install -d -m 755 "$installation_dir/runtime" "$metrics_dir"
install -d -m 700 "$backup_dir"
umask 077

write_attempt_metric() {
  local success=$1
  local now temporary
  now=$(date -u +%s)
  temporary=$(mktemp "$metrics_dir/.backup-attempt.prom.XXXXXX")
  cat >"$temporary" <<METRICS
# HELP maxposty_backup_last_attempt_timestamp_seconds Unix timestamp of the latest scheduled backup attempt.
# TYPE maxposty_backup_last_attempt_timestamp_seconds gauge
maxposty_backup_last_attempt_timestamp_seconds $now
# HELP maxposty_backup_last_attempt_success Whether the latest scheduled backup attempt completed successfully.
# TYPE maxposty_backup_last_attempt_success gauge
maxposty_backup_last_attempt_success $success
METRICS
  chmod 644 "$temporary"
  mv -f "$temporary" "$metrics_dir/backup-attempt.prom"
}

write_attempt_metric 0
auth_dir=$(mktemp -d "$release_dir/.backup-auth.XXXXXX")
chmod 700 "$auth_dir"
curl_config="$auth_dir/github-curl.config"
{
  printf 'header = "Authorization: Bearer %s"\n' "$token"
  printf 'header = "X-GitHub-Api-Version: 2026-03-10"\n'
  printf 'connect-timeout = 20\n'
} >"$curl_config"
chmod 600 "$curl_config"
unset token

timestamp=$(date -u +%Y%m%dT%H%M%SZ)
dump_temporary="$backup_dir/postgres-${timestamp}.dump.tmp"
dump_final="$backup_dir/postgres-${timestamp}.dump"
media_temporary="$backup_dir/media-${timestamp}.tar.gz.tmp"
media_final="$backup_dir/media-${timestamp}.tar.gz"
manifest_final="$backup_dir/backup-${timestamp}.sha256"
restore_database="maxposty_restore_${timestamp//[^0-9]/}"
restore_created=false
base_temporary=".base-${timestamp}.tmp"
completed=false

cleanup() {
  status=$?
  set +e
  if [[ "$restore_created" == true ]]; then
    compose exec -T postgres sh -ec \
      'PGPASSWORD="$POSTGRES_PASSWORD" dropdb --force --if-exists --no-password --username="$POSTGRES_USER" "$1"' \
      sh "$restore_database" >/dev/null 2>&1 || true
  fi
  compose exec -T postgres rm -rf "/var/lib/postgresql/base-backups/$base_temporary" >/dev/null 2>&1 || true
  rm -rf "$auth_dir"
  rm -f "$dump_temporary" "$media_temporary"
  if [[ "$completed" != true ]]; then
    write_attempt_metric 0 || true
  fi
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# Logical snapshot plus a real restore into a disposable database. Listing a
# dump alone cannot detect missing extensions, invalid ownership or SQL errors.
# shellcheck disable=SC2016
compose exec -T postgres sh -ec \
  'PGPASSWORD="$POSTGRES_PASSWORD" pg_dump --format=custom --no-password --username="$POSTGRES_USER" --dbname="$POSTGRES_DB"' \
  >"$dump_temporary"
[[ -s "$dump_temporary" ]] || { echo "Scheduled PostgreSQL backup is empty" >&2; exit 1; }
compose exec -T postgres pg_restore --list <"$dump_temporary" >/dev/null
compose exec -T postgres sh -ec \
  'PGPASSWORD="$POSTGRES_PASSWORD" createdb --no-password --username="$POSTGRES_USER" "$1"' \
  sh "$restore_database"
restore_created=true
compose exec -T postgres sh -ec \
  'PGPASSWORD="$POSTGRES_PASSWORD" pg_restore --exit-on-error --no-owner --no-privileges --no-password --username="$POSTGRES_USER" --dbname="$1"' \
  sh "$restore_database" <"$dump_temporary" >/dev/null
compose exec -T postgres sh -ec \
  'PGPASSWORD="$POSTGRES_PASSWORD" psql --no-password --username="$POSTGRES_USER" --dbname="$1" --tuples-only --command="SELECT 1"' \
  sh "$restore_database" | grep -Eq '^[[:space:]]*1[[:space:]]*$'
compose exec -T postgres sh -ec \
  'PGPASSWORD="$POSTGRES_PASSWORD" dropdb --force --no-password --username="$POSTGRES_USER" "$1"' \
  sh "$restore_database"
restore_created=false

# Keep the legacy media archive while media migration is in progress. The
# immutable encrypted asset remains self-contained for older releases.
compose --profile ops run --rm --no-deps --quiet-pull -T media-backup -C /source -czf - . >"$media_temporary"
[[ -s "$media_temporary" ]] && tar -tzf "$media_temporary" >/dev/null
chmod 600 "$dump_temporary" "$media_temporary"
mv -f "$dump_temporary" "$dump_final"
mv -f "$media_temporary" "$media_final"
(
  cd "$backup_dir"
  sha256sum "$(basename -- "$dump_final")" "$(basename -- "$media_final")" >"$(basename -- "$manifest_final")"
)
chmod 600 "$manifest_final"

# Force a WAL boundary and require PostgreSQL's archiver to acknowledge the
# exact segment before accepting a new physical recovery base.
archived_wal=$(compose exec -T postgres sh -ec \
  'PGPASSWORD="$POSTGRES_PASSWORD" psql --no-password --username="$POSTGRES_USER" --dbname="$POSTGRES_DB" --tuples-only --no-align --command="SELECT pg_walfile_name(pg_switch_wal())"' |
  tr -d '\r[:space:]')
[[ "$archived_wal" =~ ^[0-9A-F]{24}$ ]] || { echo "PostgreSQL did not return a WAL segment name" >&2; exit 1; }
wal_archived=false
for _ in {1..30}; do
  current_archived=$(compose exec -T postgres sh -ec \
    'PGPASSWORD="$POSTGRES_PASSWORD" psql --no-password --username="$POSTGRES_USER" --dbname="$POSTGRES_DB" --tuples-only --no-align --command="SELECT last_archived_wal FROM pg_stat_archiver"' |
    tr -d '\r[:space:]')
  if [[ "$current_archived" == "$archived_wal" ]]; then
    wal_archived=true
    break
  fi
  sleep 2
done
[[ "$wal_archived" == true ]] || { echo "PostgreSQL did not archive the forced WAL segment" >&2; exit 1; }

compose exec -T postgres sh -ec \
  'rm -rf "/var/lib/postgresql/base-backups/$1" && PGPASSWORD="$POSTGRES_PASSWORD" pg_basebackup --dbname="host=127.0.0.1 port=5432 user=$POSTGRES_USER dbname=$POSTGRES_DB" --no-password --pgdata="/var/lib/postgresql/base-backups/$1" --format=plain --wal-method=stream --checkpoint=fast --manifest-checksums=SHA256' \
  sh "$base_temporary"
compose exec -T postgres pg_verifybackup "/var/lib/postgresql/base-backups/$base_temporary" >/dev/null
compose exec -T postgres mv "/var/lib/postgresql/base-backups/$base_temporary" "/var/lib/postgresql/base-backups/base-$timestamp"

backup_hook="$installation_dir/hooks/after-backup"
[[ -x "$backup_hook" ]] || { echo "Production offsite backup hook is unavailable" >&2; exit 1; }
export MAXPOSTY_BACKUP_CURL_CONFIG=$curl_config
export MAXPOSTY_BACKUP_REPOSITORY=$repository
export MAXPOSTY_BACKUP_SOURCE_SHA=$source_sha
export MAXPOSTY_BACKUP_SNAPSHOT_IMAGE=$image
export MAXPOSTY_BACKUP_SNAPSHOT_SOURCE_SHA=$source_sha
export MAXPOSTY_BACKUP_RECIPIENT_CERT="$installation_dir/certs/backup-recipient.pem"
"$backup_hook" "$dump_final" "$media_final" "$manifest_final" "$image"

backup_retention_days=$(env_value "$environment_file" BACKUP_RETENTION_DAYS)
pitr_retention_days=$(env_value "$environment_file" PITR_RETENTION_DAYS)
find "$backup_dir" -type f \( -name 'postgres-*.dump' -o -name 'media-*.tar.gz' -o -name 'backup-*.sha256' \) \
  -mtime "+$backup_retention_days" -delete
compose exec -T postgres find /var/lib/postgresql/base-backups -mindepth 1 -maxdepth 1 -type d -name 'base-*' \
  -mtime "+$pitr_retention_days" -exec rm -rf -- '{}' +
compose exec -T postgres find /var/lib/postgresql/wal-archive -type f -mtime "+$pitr_retention_days" -delete

wal_failed_count=$(compose exec -T postgres sh -ec \
  'PGPASSWORD="$POSTGRES_PASSWORD" psql --no-password --username="$POSTGRES_USER" --dbname="$POSTGRES_DB" --tuples-only --no-align --command="SELECT failed_count FROM pg_stat_archiver"' |
  tr -d '\r[:space:]')
wal_last_archived_timestamp=$(compose exec -T postgres sh -ec \
  'PGPASSWORD="$POSTGRES_PASSWORD" psql --no-password --username="$POSTGRES_USER" --dbname="$POSTGRES_DB" --tuples-only --no-align --command="SELECT COALESCE(EXTRACT(EPOCH FROM last_archived_time)::bigint, 0) FROM pg_stat_archiver"' |
  tr -d '\r[:space:]')
[[ "$wal_failed_count" =~ ^[0-9]+$ && "$wal_last_archived_timestamp" =~ ^[0-9]+$ ]] || {
  echo "PostgreSQL archiver metrics are invalid" >&2
  exit 1
}
success_timestamp=$(date -u +%s)
success_metric_temporary=$(mktemp "$metrics_dir/.backup-success.prom.XXXXXX")
cat >"$success_metric_temporary" <<METRICS
# HELP maxposty_backup_last_success_timestamp_seconds Unix timestamp of the latest verified encrypted backup.
# TYPE maxposty_backup_last_success_timestamp_seconds gauge
maxposty_backup_last_success_timestamp_seconds $success_timestamp
# HELP maxposty_backup_restore_verification_last_success_timestamp_seconds Unix timestamp of the latest successful temporary-database restore drill.
# TYPE maxposty_backup_restore_verification_last_success_timestamp_seconds gauge
maxposty_backup_restore_verification_last_success_timestamp_seconds $success_timestamp
# HELP maxposty_pitr_base_backup_last_success_timestamp_seconds Unix timestamp of the latest pg_verifybackup-validated physical base backup.
# TYPE maxposty_pitr_base_backup_last_success_timestamp_seconds gauge
maxposty_pitr_base_backup_last_success_timestamp_seconds $success_timestamp
# HELP maxposty_postgresql_wal_archive_failed_total PostgreSQL WAL archive failures reported by pg_stat_archiver.
# TYPE maxposty_postgresql_wal_archive_failed_total counter
maxposty_postgresql_wal_archive_failed_total $wal_failed_count
# HELP maxposty_postgresql_wal_archive_last_success_timestamp_seconds Unix timestamp of PostgreSQL's latest archived WAL segment.
# TYPE maxposty_postgresql_wal_archive_last_success_timestamp_seconds gauge
maxposty_postgresql_wal_archive_last_success_timestamp_seconds $wal_last_archived_timestamp
METRICS
chmod 644 "$success_metric_temporary"
mv -f "$success_metric_temporary" "$metrics_dir/backup-success.prom"
write_attempt_metric 1
completed=true
echo "Scheduled encrypted backup, restore drill and PITR base verification completed"
