#!/usr/bin/env bash
set -euo pipefail

repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
compose_file="$repo_root/deploy/compose.production.yaml"
backup_script="$repo_root/deploy/backup/run-scheduled-backup.sh"
backup_hook="$repo_root/deploy/backup/after-backup-github-release.sh"
backup_workflow="$repo_root/.github/workflows/backup.yml"
deploy_workflow="$repo_root/.github/workflows/deploy.yml"
deploy_script="$repo_root/deploy/deploy-production.sh"
renderer="$repo_root/deploy/render-alertmanager-config.sh"

for required_file in "$backup_script" "$backup_hook" "$backup_workflow" "$renderer"; do
  [[ -f "$required_file" ]] || { echo "Missing operations file: $required_file" >&2; exit 1; }
done
[[ -x "$backup_script" && -x "$renderer" ]] || {
  echo "Operations scripts must be executable" >&2
  exit 1
}

# The daily job is independent from deploys but shares the same concurrency and
# server lock, so PostgreSQL cannot be restarted during a restore drill.
grep -F 'cron: "17 1 * * *"' "$backup_workflow" >/dev/null
grep -F 'group: maxposty-backend-production' "$backup_workflow" >/dev/null
grep -F 'contents: write' "$backup_workflow" >/dev/null
grep -F '/opt/maxposty/backend/current/deploy/backup/run-scheduled-backup.sh' "$backup_workflow" >/dev/null
grep -F 'printf '\''%s\n'\'' "$BACKUP_TOKEN" | ssh' "$backup_workflow" >/dev/null
if grep -Eq 'ssh[^\n]*(BACKUP_TOKEN|github\.token)' "$backup_workflow"; then
  echo "The backup token must be sent on standard input, not as an SSH argument" >&2
  exit 1
fi
grep -F 'flock -w 300' "$backup_script" >/dev/null
grep -F 'current_dir" != "$release_dir' "$backup_script" >/dev/null
for signal_script in "$backup_script" "$backup_hook"; do
  grep -F 'trap cleanup EXIT' "$signal_script" >/dev/null
  grep -F "trap 'exit 130' INT" "$signal_script" >/dev/null
  grep -F "trap 'exit 143' TERM" "$signal_script" >/dev/null
  if grep -F 'trap cleanup EXIT INT TERM' "$signal_script" >/dev/null; then
    echo "Interrupted backup scripts must not report success: $signal_script" >&2
    exit 1
  fi
done

# A verified dump means a real disposable-database restore, not just listing an
# archive. A physical recovery base and an acknowledged WAL boundary are also
# required before the run is marked successful.
for contract in \
  'pg_dump --format=custom' \
  'createdb --no-password' \
  'pg_restore --exit-on-error' \
  'dropdb --force' \
  'SELECT pg_walfile_name(pg_switch_wal())' \
  'SELECT last_archived_wal FROM pg_stat_archiver' \
  'pg_basebackup' \
  'pg_verifybackup' \
  'PITR_RETENTION_DAYS' \
  'maxposty_backup_restore_verification_last_success_timestamp_seconds' \
  'maxposty_pitr_base_backup_last_success_timestamp_seconds' \
  'maxposty_postgresql_wal_archive_failed_total'; do
  grep -F "$contract" "$backup_script" >/dev/null || {
    echo "Scheduled backup is missing contract: $contract" >&2
    exit 1
  }
done

for contract in \
  'archive_mode=on' \
  'archive_timeout=300s' \
  'archive_command=test ! -f /var/lib/postgresql/wal-archive/%f' \
  'postgres-wal-archive:/var/lib/postgresql/wal-archive' \
  'postgres-base-backups:/var/lib/postgresql/base-backups' \
  'alertmanager-config:/etc/alertmanager:ro' \
  'network_mode: none'; do
  grep -F "$contract" "$compose_file" >/dev/null || {
    echo "Production compose is missing operations contract: $contract" >&2
    exit 1
  }
done

# Alert delivery is rendered on the server into a private file. The actual
# receiver URL must stay exclusively in the protected production secret.
grep -F 'ALERTMANAGER_WEBHOOK_URL: ${{ secrets.ALERTMANAGER_WEBHOOK_URL }}' "$deploy_workflow" >/dev/null
grep -F 'render-alertmanager-config.sh' "$deploy_script" >/dev/null
grep -F 'chmod 600 "$temporary"' "$renderer" >/dev/null
grep -F 'send_resolved: true' "$renderer" >/dev/null
if grep -E 'https://[^[:space:]'\'']+/(hooks|webhook|api/webhooks)/[A-Za-z0-9_-]+' "$renderer" "$compose_file"; then
  echo "Alert receiver credentials must not be hardcoded" >&2
  exit 1
fi

sandbox=$(mktemp -d "$repo_root/.operations-policy.XXXXXX")
trap 'rm -rf "$sandbox"' EXIT
production_env="$sandbox/production.env"
env \
  DEPLOY_STAGE=production \
  POSTGRES_OWNER_PASSWORD=owner_password_0123456789abcdef0123456789abcdef \
  POSTGRES_APP_PASSWORD=app_password_0123456789abcdef0123456789abcdef \
  POSTGRES_MONITOR_PASSWORD=monitor_password_0123456789abcdef0123456789abcdef \
  GRAFANA_ADMIN_PASSWORD=grafana_admin_0123456789abcdef0123456789abcdef \
  GRAFANA_SECRET_KEY=grafana_secret_0123456789abcdef0123456789abcdef \
  ALERTMANAGER_WEBHOOK_URL=https://alerts.example.test/private-receiver \
  YANDEX_CLIENT_ID=yandex-client-id \
  YANDEX_CLIENT_SECRET=yandex-client-secret \
  OBSERVABILITY_ADMIN_USERS=makolov99 \
  MAX_BOT_TOKEN=max-bot-token \
  MAX_WEBHOOK_SECRET=0123456789abcdef0123456789abcdef \
  S3_HOST=https://s3.example.test \
  S3_ACCESS_KEY=test-access-key \
  S3_SECRET_KEY=test-secret-key+/= \
  "$repo_root/deploy/render-production-env.sh" "$production_env"
production_alertmanager="$sandbox/alertmanager.yml"
"$renderer" "$production_env" "$production_alertmanager"
grep -F "url: 'https://alerts.example.test/private-receiver'" "$production_alertmanager" >/dev/null
grep -F 'send_resolved: true' "$production_alertmanager" >/dev/null
[[ $(stat -c '%a' "$production_alertmanager" 2>/dev/null || stat -f '%Lp' "$production_alertmanager") == 600 ]]

# The overridden `sh -ec` entrypoint must receive the initializer as one argv
# element. A scalar Compose command is tokenized and silently turns `mkdir`
# into a zero-argument script on the production Docker/Compose versions.
if command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1 && command -v jq >/dev/null 2>&1; then
  runtime_command_length=$(
    BACKEND_IMAGE=ghcr.io/example/maxposty@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa \
      docker compose \
        --project-directory "$repo_root" \
        --env-file "$production_env" \
        --file "$compose_file" \
        config --format json |
      jq -r '.services["runtime-storage-init"].command | length'
  )
  [[ "$runtime_command_length" == 1 ]] || {
    echo "runtime-storage-init command must remain a single shell script argument" >&2
    exit 1
  }
fi

without_alerts_env="$sandbox/without-alerts.env"
env \
  DEPLOY_STAGE=production \
  POSTGRES_OWNER_PASSWORD=owner_password_0123456789abcdef0123456789abcdef \
  POSTGRES_APP_PASSWORD=app_password_0123456789abcdef0123456789abcdef \
  POSTGRES_MONITOR_PASSWORD=monitor_password_0123456789abcdef0123456789abcdef \
  GRAFANA_ADMIN_PASSWORD=grafana_admin_0123456789abcdef0123456789abcdef \
  GRAFANA_SECRET_KEY=grafana_secret_0123456789abcdef0123456789abcdef \
  YANDEX_CLIENT_ID=yandex-client-id \
  YANDEX_CLIENT_SECRET=yandex-client-secret \
  OBSERVABILITY_ADMIN_USERS=makolov99 \
  MAX_BOT_TOKEN=max-bot-token \
  MAX_WEBHOOK_SECRET=0123456789abcdef0123456789abcdef \
  S3_HOST=https://s3.example.test \
  S3_ACCESS_KEY=test-access-key \
  S3_SECRET_KEY=test-secret-key+/= \
  "$repo_root/deploy/render-production-env.sh" "$without_alerts_env"
without_alerts_config="$sandbox/alertmanager-disabled.yml"
"$renderer" "$without_alerts_env" "$without_alerts_config"
grep -F 'receiver: operator-disabled' "$without_alerts_config" >/dev/null
if grep -F 'webhook_configs:' "$without_alerts_config"; then
  echo "Alertmanager must stay local when no receiver URL is configured" >&2
  exit 1
fi
if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
  docker run --rm --user "$(id -u):$(id -g)" --entrypoint=/bin/amtool \
    --volume "$sandbox:/config:ro" \
    'prom/alertmanager:v0.33.1@sha256:9e082985f56f4c8c9f724e18f2288c6708f472e56a5286b8863d080434ea065d' \
    check-config /config/alertmanager.yml >/dev/null
fi

bootstrap_env="$sandbox/bootstrap.env"
env \
  DEPLOY_STAGE=bootstrap \
  POSTGRES_OWNER_PASSWORD=owner_password_0123456789abcdef0123456789abcdef \
  POSTGRES_APP_PASSWORD=app_password_0123456789abcdef0123456789abcdef \
  POSTGRES_MONITOR_PASSWORD=monitor_password_0123456789abcdef0123456789abcdef \
  GRAFANA_ADMIN_PASSWORD=grafana_admin_0123456789abcdef0123456789abcdef \
  GRAFANA_SECRET_KEY=grafana_secret_0123456789abcdef0123456789abcdef \
  "$repo_root/deploy/render-production-env.sh" "$bootstrap_env"
bootstrap_alertmanager="$sandbox/bootstrap-alertmanager.yml"
"$renderer" "$bootstrap_env" "$bootstrap_alertmanager"
grep -F 'receiver: operator-disabled' "$bootstrap_alertmanager" >/dev/null
if grep -F 'webhook_configs:' "$bootstrap_alertmanager" >/dev/null; then
  echo "Bootstrap Alertmanager must not have an outbound receiver" >&2
  exit 1
fi

echo "Production operations policy tests passed."
