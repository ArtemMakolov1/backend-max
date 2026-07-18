#!/usr/bin/env bash
set -euo pipefail

output=${1:-}
if [[ -z "$output" ]]; then
  echo "Usage: $0 OUTPUT_FILE" >&2
  exit 2
fi

deploy_stage=${DEPLOY_STAGE:-bootstrap}
if [[ "$deploy_stage" != "bootstrap" && "$deploy_stage" != "production" ]]; then
  echo "DEPLOY_STAGE must be bootstrap or production" >&2
  exit 1
fi

required_secret_names=(
  POSTGRES_OWNER_PASSWORD
  POSTGRES_APP_PASSWORD
  POSTGRES_MONITOR_PASSWORD
  GRAFANA_ADMIN_PASSWORD
  GRAFANA_SECRET_KEY
)
if [[ "$deploy_stage" == "production" ]]; then
  required_secret_names+=(
    YANDEX_CLIENT_ID
    YANDEX_CLIENT_SECRET
    MAX_BOT_TOKEN
    MAX_WEBHOOK_SECRET
    S3_HOST
    S3_ACCESS_KEY
    S3_SECRET_KEY
  )
fi
for name in "${required_secret_names[@]}"; do
  if [[ -z "${!name:-}" ]]; then
    echo "Missing required deployment secret: $name" >&2
    exit 1
  fi
done

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
mkdir -p "$(dirname -- "$output")"
umask 077
temporary=$(mktemp "${output}.tmp.XXXXXX")
trap 'rm -f "$temporary"' EXIT

if [[ "$deploy_stage" == "bootstrap" ]]; then
  public_base_url=http://178.159.94.83
  frontend_origin=http://178.159.94.83
  auth_bootstrap_mode=true
  rendered_oauth_client_id=''
  rendered_oauth_client_secret=''
  rendered_oauth_redirect_uri=''
  rendered_allowed_users=''
  rendered_observability_admins=''
  rendered_bot_token=''
  rendered_webhook_secret=''
  rendered_s3_host=''
  rendered_s3_access_key=''
  rendered_s3_secret_key=''
  rendered_s3_bucket=''
  rendered_s3_region=''
  rendered_openai_key=''
  rendered_alertmanager_webhook_url=''
else
  public_base_url=https://maxposty.ru
  frontend_origin=https://maxposty.ru
  auth_bootstrap_mode=false
  rendered_oauth_client_id=$YANDEX_CLIENT_ID
  rendered_oauth_client_secret=$YANDEX_CLIENT_SECRET
  rendered_oauth_redirect_uri=https://maxposty.ru/api/v1/auth/yandex/callback
  rendered_allowed_users=${YANDEX_ALLOWED_USERS:-}
  rendered_observability_admins=${OBSERVABILITY_ADMIN_USERS:-}
  if [[ -z "$rendered_observability_admins" ]]; then
    echo "Missing required production deployment variable: OBSERVABILITY_ADMIN_USERS" >&2
    exit 1
  fi
  rendered_bot_token=$MAX_BOT_TOKEN
  rendered_webhook_secret=$MAX_WEBHOOK_SECRET
  rendered_s3_host=$S3_HOST
  rendered_s3_access_key=$S3_ACCESS_KEY
  rendered_s3_secret_key=$S3_SECRET_KEY
  rendered_s3_bucket=${S3_BUCKET:-}
  rendered_s3_region=${S3_REGION:-}
  rendered_openai_key=${OPENAI_API_KEY:-}
  rendered_alertmanager_webhook_url=${ALERTMANAGER_WEBHOOK_URL:-}
fi

{
  printf 'POSTGRES_DB=maxstudio\n'
  printf 'POSTGRES_OWNER_USER=maxstudio_owner\n'
  printf 'POSTGRES_OWNER_PASSWORD=%s\n' "$POSTGRES_OWNER_PASSWORD"
  printf 'POSTGRES_APP_USER=maxstudio_app\n'
  printf 'POSTGRES_APP_PASSWORD=%s\n' "$POSTGRES_APP_PASSWORD"
  printf 'POSTGRES_MONITOR_USER=maxstudio_monitor\n'
  printf 'POSTGRES_MONITOR_PASSWORD=%s\n' "$POSTGRES_MONITOR_PASSWORD"
  printf 'PGBOUNCER_DEFAULT_POOL_SIZE=%s\n' "${PGBOUNCER_DEFAULT_POOL_SIZE:-20}"
  printf 'PGBOUNCER_MIN_POOL_SIZE=%s\n' "${PGBOUNCER_MIN_POOL_SIZE:-2}"
  printf 'PGBOUNCER_RESERVE_POOL_SIZE=%s\n' "${PGBOUNCER_RESERVE_POOL_SIZE:-5}"
  printf 'PGBOUNCER_MAX_CLIENT_CONN=%s\n' "${PGBOUNCER_MAX_CLIENT_CONN:-200}"
  printf 'PUBLIC_BASE_URL=%s\n' "$public_base_url"
  printf 'FRONTEND_ORIGIN=%s\n' "$frontend_origin"
  printf 'GRAFANA_ROOT_URL=%s/monitoring/\n' "$public_base_url"
  printf 'GRAFANA_ADMIN_PASSWORD=%s\n' "$GRAFANA_ADMIN_PASSWORD"
  printf 'GRAFANA_SECRET_KEY=%s\n' "$GRAFANA_SECRET_KEY"
  printf 'ALERTMANAGER_WEBHOOK_URL=%s\n' "$rendered_alertmanager_webhook_url"
  printf 'AUTH_BOOTSTRAP_MODE=%s\n' "$auth_bootstrap_mode"
  printf 'YANDEX_CLIENT_ID=%s\n' "$rendered_oauth_client_id"
  printf 'YANDEX_CLIENT_SECRET=%s\n' "$rendered_oauth_client_secret"
  printf 'YANDEX_REDIRECT_URI=%s\n' "$rendered_oauth_redirect_uri"
  printf 'YANDEX_ALLOWED_USERS=%s\n' "$rendered_allowed_users"
  printf 'OBSERVABILITY_ADMIN_USERS=%s\n' "$rendered_observability_admins"
  printf 'AUTH_SESSION_TTL=%s\n' "${AUTH_SESSION_TTL:-12h}"
  printf 'WORKSPACE_MAX_OWNED_TEAM_WORKSPACES=%s\n' "${WORKSPACE_MAX_OWNED_TEAM_WORKSPACES:-5}"
  printf 'OAUTH_TRUST_X_REAL_IP=true\n'
  printf 'OAUTH_RATE_LIMIT_AT_EDGE=false\n'
  printf 'MAX_API_BASE_URL=https://platform-api2.max.ru\n'
  printf 'MAX_BOT_TOKEN=%s\n' "$rendered_bot_token"
  printf 'MAX_WEBHOOK_SECRET=%s\n' "$rendered_webhook_secret"
  printf 'MAX_WEBHOOK_URL=https://maxposty.ru/api/v1/webhooks/max\n'
  printf 'MAX_CA_CERT_FILE=%s\n' "${MAX_CA_CERT_FILE:-}"
  printf 'S3_HOST=%s\n' "$rendered_s3_host"
  printf 'S3_ACCESS_KEY=%s\n' "$rendered_s3_access_key"
  printf 'S3_SECRET_KEY=%s\n' "$rendered_s3_secret_key"
  printf 'S3_BUCKET=%s\n' "$rendered_s3_bucket"
  printf 'S3_REGION=%s\n' "$rendered_s3_region"
  printf 'MEDIA_USER_MAX_FILES=%s\n' "${MEDIA_USER_MAX_FILES:-500}"
  printf 'MEDIA_USER_MAX_BYTES=%s\n' "${MEDIA_USER_MAX_BYTES:-1073741824}"
  printf 'MEDIA_ORPHAN_GRACE_PERIOD=%s\n' "${MEDIA_ORPHAN_GRACE_PERIOD:-24h}"
  printf 'MEDIA_CLEANUP_INTERVAL=%s\n' "${MEDIA_CLEANUP_INTERVAL:-15m}"
  printf 'MEDIA_CLEANUP_BATCH_SIZE=%s\n' "${MEDIA_CLEANUP_BATCH_SIZE:-50}"
  printf 'OPENAI_API_KEY=%s\n' "$rendered_openai_key"
  printf 'OPENAI_API_BASE_URL=https://api.openai.com\n'
  # Welcome-email SMTP is optional: empty values disable it (NoopSender).
  printf 'SMTP_HOST=%s\n' "${SMTP_HOST:-}"
  printf 'SMTP_PORT=%s\n' "${SMTP_PORT:-587}"
  printf 'SMTP_USERNAME=%s\n' "${SMTP_USERNAME:-}"
  printf 'SMTP_PASSWORD=%s\n' "${SMTP_PASSWORD:-}"
  printf 'SMTP_FROM_EMAIL=%s\n' "${SMTP_FROM_EMAIL:-}"
  printf 'SMTP_FROM_NAME=%s\n' "${SMTP_FROM_NAME:-MaxPosty}"
  printf 'OPENAI_IMAGE_MODEL=%s\n' "${OPENAI_IMAGE_MODEL:-gpt-image-2}"
  printf 'OPENAI_RESEARCH_MODEL=%s\n' "${OPENAI_RESEARCH_MODEL:-gpt-5.4-mini}"
  printf 'AI_GLOBAL_MAX_CONCURRENT=%s\n' "${AI_GLOBAL_MAX_CONCURRENT:-4}"
  printf 'AI_USER_MAX_CONCURRENT=%s\n' "${AI_USER_MAX_CONCURRENT:-1}"
  printf 'AI_IMAGE_PER_MINUTE=%s\n' "${AI_IMAGE_PER_MINUTE:-2}"
  printf 'AI_IMAGE_PER_DAY=%s\n' "${AI_IMAGE_PER_DAY:-20}"
  printf 'AI_RESEARCH_PER_MINUTE=%s\n' "${AI_RESEARCH_PER_MINUTE:-2}"
  printf 'AI_RESEARCH_PER_DAY=%s\n' "${AI_RESEARCH_PER_DAY:-20}"
  printf 'AI_LEASE_TTL=%s\n' "${AI_LEASE_TTL:-4m}"
  printf 'BILLING_ENFORCEMENT_ENABLED=%s\n' "${BILLING_ENFORCEMENT_ENABLED:-false}"
  printf 'SCHEDULER_INTERVAL=%s\n' "${SCHEDULER_INTERVAL:-15s}"
  printf 'BACKUP_RETENTION_DAYS=%s\n' "${BACKUP_RETENTION_DAYS:-14}"
  printf 'PITR_RETENTION_DAYS=%s\n' "${PITR_RETENTION_DAYS:-7}"
} >"$temporary"

chmod 600 "$temporary"
"$script_dir/validate-production-env.sh" "$temporary"
mv -f "$temporary" "$output"
trap - EXIT
