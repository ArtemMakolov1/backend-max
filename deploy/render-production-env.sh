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

required_secret_names=(POSTGRES_OWNER_PASSWORD POSTGRES_APP_PASSWORD)
if [[ "$deploy_stage" == "production" ]]; then
  required_secret_names+=(YANDEX_CLIENT_ID YANDEX_CLIENT_SECRET MAX_BOT_TOKEN MAX_WEBHOOK_SECRET OPENAI_API_KEY)
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
  rendered_bot_token=''
  rendered_webhook_secret=''
  rendered_openai_key=''
else
  public_base_url=https://maxposty.ru
  frontend_origin=https://maxposty.ru
  auth_bootstrap_mode=false
  rendered_oauth_client_id=$YANDEX_CLIENT_ID
  rendered_oauth_client_secret=$YANDEX_CLIENT_SECRET
  rendered_oauth_redirect_uri=https://maxposty.ru/api/v1/auth/yandex/callback
  rendered_allowed_users=${YANDEX_ALLOWED_USERS:-}
  rendered_bot_token=$MAX_BOT_TOKEN
  rendered_webhook_secret=$MAX_WEBHOOK_SECRET
  rendered_openai_key=$OPENAI_API_KEY
fi

{
  printf 'POSTGRES_DB=maxstudio\n'
  printf 'POSTGRES_OWNER_USER=maxstudio_owner\n'
  printf 'POSTGRES_OWNER_PASSWORD=%s\n' "$POSTGRES_OWNER_PASSWORD"
  printf 'POSTGRES_APP_USER=maxstudio_app\n'
  printf 'POSTGRES_APP_PASSWORD=%s\n' "$POSTGRES_APP_PASSWORD"
  printf 'PGBOUNCER_DEFAULT_POOL_SIZE=%s\n' "${PGBOUNCER_DEFAULT_POOL_SIZE:-20}"
  printf 'PGBOUNCER_MIN_POOL_SIZE=%s\n' "${PGBOUNCER_MIN_POOL_SIZE:-2}"
  printf 'PGBOUNCER_RESERVE_POOL_SIZE=%s\n' "${PGBOUNCER_RESERVE_POOL_SIZE:-5}"
  printf 'PGBOUNCER_MAX_CLIENT_CONN=%s\n' "${PGBOUNCER_MAX_CLIENT_CONN:-200}"
  printf 'PUBLIC_BASE_URL=%s\n' "$public_base_url"
  printf 'FRONTEND_ORIGIN=%s\n' "$frontend_origin"
  printf 'AUTH_BOOTSTRAP_MODE=%s\n' "$auth_bootstrap_mode"
  printf 'YANDEX_CLIENT_ID=%s\n' "$rendered_oauth_client_id"
  printf 'YANDEX_CLIENT_SECRET=%s\n' "$rendered_oauth_client_secret"
  printf 'YANDEX_REDIRECT_URI=%s\n' "$rendered_oauth_redirect_uri"
  printf 'YANDEX_ALLOWED_USERS=%s\n' "$rendered_allowed_users"
  printf 'AUTH_SESSION_TTL=%s\n' "${AUTH_SESSION_TTL:-12h}"
  printf 'OAUTH_TRUST_X_REAL_IP=true\n'
  printf 'OAUTH_RATE_LIMIT_AT_EDGE=false\n'
  printf 'MAX_API_BASE_URL=https://platform-api2.max.ru\n'
  printf 'MAX_BOT_TOKEN=%s\n' "$rendered_bot_token"
  printf 'MAX_WEBHOOK_SECRET=%s\n' "$rendered_webhook_secret"
  printf 'MAX_WEBHOOK_URL=https://maxposty.ru/api/v1/webhooks/max\n'
  printf 'MAX_CA_CERT_FILE=%s\n' "${MAX_CA_CERT_FILE:-}"
  printf 'OPENAI_API_KEY=%s\n' "$rendered_openai_key"
  printf 'OPENAI_API_BASE_URL=https://api.openai.com\n'
  printf 'OPENAI_IMAGE_MODEL=%s\n' "${OPENAI_IMAGE_MODEL:-gpt-image-2}"
  printf 'OPENAI_RESEARCH_MODEL=%s\n' "${OPENAI_RESEARCH_MODEL:-gpt-5.4-mini}"
  printf 'AI_GLOBAL_MAX_CONCURRENT=%s\n' "${AI_GLOBAL_MAX_CONCURRENT:-4}"
  printf 'AI_USER_MAX_CONCURRENT=%s\n' "${AI_USER_MAX_CONCURRENT:-1}"
  printf 'AI_IMAGE_PER_MINUTE=%s\n' "${AI_IMAGE_PER_MINUTE:-2}"
  printf 'AI_IMAGE_PER_DAY=%s\n' "${AI_IMAGE_PER_DAY:-20}"
  printf 'AI_RESEARCH_PER_MINUTE=%s\n' "${AI_RESEARCH_PER_MINUTE:-2}"
  printf 'AI_RESEARCH_PER_DAY=%s\n' "${AI_RESEARCH_PER_DAY:-20}"
  printf 'AI_LEASE_TTL=%s\n' "${AI_LEASE_TTL:-4m}"
  printf 'SCHEDULER_INTERVAL=%s\n' "${SCHEDULER_INTERVAL:-15s}"
  printf 'BACKUP_RETENTION_DAYS=%s\n' "${BACKUP_RETENTION_DAYS:-14}"
} >"$temporary"

chmod 600 "$temporary"
"$script_dir/validate-production-env.sh" "$temporary"
mv -f "$temporary" "$output"
trap - EXIT
