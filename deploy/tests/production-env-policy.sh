#!/usr/bin/env bash
set -euo pipefail

repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
sandbox=$(mktemp -d)
trap 'rm -rf "$sandbox"' EXIT

render_production() {
  local output=$1
  shift
  env \
    DEPLOY_STAGE=production \
    POSTGRES_OWNER_PASSWORD=owner_password_0123456789abcdef0123456789abcdef \
    POSTGRES_APP_PASSWORD=app_password_0123456789abcdef0123456789abcdef \
    POSTGRES_MONITOR_PASSWORD=monitor_password_0123456789abcdef0123456789abcdef \
    GRAFANA_ADMIN_PASSWORD=grafana_admin_0123456789abcdef0123456789abcdef \
    GRAFANA_SECRET_KEY=grafana_secret_0123456789abcdef0123456789abcdef \
    ALERTMANAGER_WEBHOOK_URL=https://alerts.example.test/maxposty \
    YANDEX_CLIENT_ID=yandex-client-id \
    YANDEX_CLIENT_SECRET=yandex-client-secret \
    OBSERVABILITY_ADMIN_USERS=makolov99 \
    MAX_BOT_TOKEN=max-bot-token \
    MAX_WEBHOOK_SECRET=0123456789abcdef0123456789abcdef \
    S3_HOST=https://s3.example.test \
    S3_ACCESS_KEY=test-access-key \
    S3_SECRET_KEY=test-secret-key+/= \
    OPENAI_API_KEY= \
    "$@" \
    "$repo_root/deploy/render-production-env.sh" "$output"
}

production_env="$sandbox/production.env"
render_production "$production_env"
grep -Fx 'AUTH_BOOTSTRAP_MODE=false' "$production_env" >/dev/null
grep -Fx 'OPENAI_API_KEY=' "$production_env" >/dev/null
grep -Fx 'GRAFANA_ROOT_URL=https://maxposty.ru/monitoring/' "$production_env" >/dev/null
grep -Fx 'ALERTMANAGER_WEBHOOK_URL=https://alerts.example.test/maxposty' "$production_env" >/dev/null
grep -Fx 'PITR_RETENTION_DAYS=7' "$production_env" >/dev/null
grep -Fx 'S3_HOST=https://s3.example.test' "$production_env" >/dev/null
grep -Fx 'S3_BUCKET=' "$production_env" >/dev/null
grep -Fx 'S3_REGION=' "$production_env" >/dev/null
grep -Fx 'MEDIA_USER_MAX_FILES=500' "$production_env" >/dev/null
grep -Fx 'MEDIA_USER_MAX_BYTES=1073741824' "$production_env" >/dev/null
grep -Fx 'MEDIA_ORPHAN_GRACE_PERIOD=24h' "$production_env" >/dev/null
grep -Fx 'MEDIA_CLEANUP_INTERVAL=15m' "$production_env" >/dev/null
grep -Fx 'MEDIA_CLEANUP_BATCH_SIZE=50' "$production_env" >/dev/null
"$repo_root/deploy/validate-production-env.sh" "$production_env"

configured_s3_env="$sandbox/configured-s3.env"
render_production "$configured_s3_env" S3_BUCKET=media.maxposty.ru S3_REGION=ru-1
grep -Fx 'S3_BUCKET=media.maxposty.ru' "$configured_s3_env" >/dev/null
grep -Fx 'S3_REGION=ru-1' "$configured_s3_env" >/dev/null
"$repo_root/deploy/validate-production-env.sh" "$configured_s3_env"

configured_media_env="$sandbox/configured-media.env"
render_production "$configured_media_env" \
  MEDIA_USER_MAX_FILES=750 \
  MEDIA_USER_MAX_BYTES=2147483648 \
  MEDIA_ORPHAN_GRACE_PERIOD=48h \
  MEDIA_CLEANUP_INTERVAL=30m \
  MEDIA_CLEANUP_BATCH_SIZE=75
grep -Fx 'MEDIA_USER_MAX_FILES=750' "$configured_media_env" >/dev/null
grep -Fx 'MEDIA_USER_MAX_BYTES=2147483648' "$configured_media_env" >/dev/null
grep -Fx 'MEDIA_ORPHAN_GRACE_PERIOD=48h' "$configured_media_env" >/dev/null
grep -Fx 'MEDIA_CLEANUP_INTERVAL=30m' "$configured_media_env" >/dev/null
grep -Fx 'MEDIA_CLEANUP_BATCH_SIZE=75' "$configured_media_env" >/dev/null
"$repo_root/deploy/validate-production-env.sh" "$configured_media_env"

for required_secret in \
  POSTGRES_MONITOR_PASSWORD GRAFANA_ADMIN_PASSWORD GRAFANA_SECRET_KEY ALERTMANAGER_WEBHOOK_URL \
  YANDEX_CLIENT_ID YANDEX_CLIENT_SECRET MAX_BOT_TOKEN MAX_WEBHOOK_SECRET \
  S3_HOST S3_ACCESS_KEY S3_SECRET_KEY; do
  if render_production "$sandbox/missing-$required_secret.env" "$required_secret=" >/dev/null 2>&1; then
    echo "Production render accepted an empty $required_secret" >&2
    exit 1
  fi
done

for invalid_alertmanager_url in \
  'http://alerts.example.test/maxposty' \
  'https://user:password@alerts.example.test/maxposty' \
  'https://alerts.example.test/maxposty#receiver' \
  'https://alerts..example.test/maxposty' \
  'https://alerts.example.test:65536/maxposty'; do
  awk -F= -v value="$invalid_alertmanager_url" \
    '$1 == "ALERTMANAGER_WEBHOOK_URL" { print "ALERTMANAGER_WEBHOOK_URL=" value; next } { print }' \
    "$production_env" >"$sandbox/invalid-alertmanager.env"
  if "$repo_root/deploy/validate-production-env.sh" "$sandbox/invalid-alertmanager.env" >/dev/null 2>&1; then
    echo "Production validation accepted an unsafe ALERTMANAGER_WEBHOOK_URL" >&2
    exit 1
  fi
done

awk -F= '$1 == "S3_HOST" { print "S3_HOST=http://s3.example.test"; next } { print }' \
  "$production_env" >"$sandbox/insecure-s3-host.env"
if "$repo_root/deploy/validate-production-env.sh" "$sandbox/insecure-s3-host.env" >/dev/null 2>&1; then
  echo "Production validation accepted an insecure S3_HOST" >&2
  exit 1
fi

awk -F= '$1 == "S3_BUCKET" { print "S3_BUCKET=Invalid_Bucket"; next } { print }' \
  "$configured_s3_env" >"$sandbox/invalid-s3-bucket.env"
if "$repo_root/deploy/validate-production-env.sh" "$sandbox/invalid-s3-bucket.env" >/dev/null 2>&1; then
  echo "Production validation accepted an invalid S3_BUCKET" >&2
  exit 1
fi

awk -F= '$1 == "S3_REGION" { print "S3_REGION=invalid region"; next } { print }' \
  "$configured_s3_env" >"$sandbox/invalid-s3-region.env"
if "$repo_root/deploy/validate-production-env.sh" "$sandbox/invalid-s3-region.env" >/dev/null 2>&1; then
  echo "Production validation accepted an invalid S3_REGION" >&2
  exit 1
fi

for media_override in \
  'MEDIA_USER_MAX_FILES=0' \
  'MEDIA_USER_MAX_BYTES=0' \
  'MEDIA_ORPHAN_GRACE_PERIOD=30m' \
  'MEDIA_CLEANUP_INTERVAL=30s' \
  'MEDIA_CLEANUP_BATCH_SIZE=1001'; do
  key=${media_override%%=*}
  awk -F= -v key="$key" -v replacement="$media_override" \
    '$1 == key { print replacement; next } { print }' \
    "$production_env" >"$sandbox/invalid-media.env"
  if "$repo_root/deploy/validate-production-env.sh" "$sandbox/invalid-media.env" >/dev/null 2>&1; then
    echo "Production validation accepted unsafe $media_override" >&2
    exit 1
  fi
done

if render_production "$sandbox/missing-observability-admins.env" OBSERVABILITY_ADMIN_USERS= >/dev/null 2>&1; then
  echo "Production render accepted empty OBSERVABILITY_ADMIN_USERS" >&2
  exit 1
fi

awk -F= '$1 == "OBSERVABILITY_ADMIN_USERS" { print "OBSERVABILITY_ADMIN_USERS=valid,not valid"; next } { print }' \
  "$production_env" >"$sandbox/invalid-observability-admins.env"
if "$repo_root/deploy/validate-production-env.sh" "$sandbox/invalid-observability-admins.env" >/dev/null 2>&1; then
  echo "Production validation accepted invalid OBSERVABILITY_ADMIN_USERS" >&2
  exit 1
fi

awk -F= '$1 == "OBSERVABILITY_ADMIN_USERS" { print "OBSERVABILITY_ADMIN_USERS=user@example.com"; next } { print }' \
  "$sandbox/production.env" >"$sandbox/email-observability-admin.env"
if "$repo_root/deploy/validate-production-env.sh" "$sandbox/email-observability-admin.env" >/dev/null 2>&1; then
  echo "Production validation accepted an email although auth checks only Yandex login/PSUID" >&2
  exit 1
fi

awk -F= '$1 == "POSTGRES_MONITOR_USER" { print "POSTGRES_MONITOR_USER=maxstudio_owner"; next } { print }' \
  "$production_env" >"$sandbox/duplicate-db-role.env"
if "$repo_root/deploy/validate-production-env.sh" "$sandbox/duplicate-db-role.env" >/dev/null 2>&1; then
  echo "Production validation accepted duplicate PostgreSQL roles" >&2
  exit 1
fi

bootstrap_env="$sandbox/bootstrap.env"
env \
  DEPLOY_STAGE=bootstrap \
  POSTGRES_OWNER_PASSWORD=owner_password_0123456789abcdef0123456789abcdef \
  POSTGRES_APP_PASSWORD=app_password_0123456789abcdef0123456789abcdef \
  POSTGRES_MONITOR_PASSWORD=monitor_password_0123456789abcdef0123456789abcdef \
  GRAFANA_ADMIN_PASSWORD=grafana_admin_0123456789abcdef0123456789abcdef \
  GRAFANA_SECRET_KEY=grafana_secret_0123456789abcdef0123456789abcdef \
  ALERTMANAGER_WEBHOOK_URL=must-not-leak \
  YANDEX_CLIENT_ID=must-not-leak \
  OBSERVABILITY_ADMIN_USERS=must-not-leak \
  MAX_BOT_TOKEN=must-not-leak \
  S3_HOST=must-not-leak \
  S3_ACCESS_KEY=must-not-leak \
  S3_SECRET_KEY=must-not-leak \
  S3_BUCKET=must-not-leak \
  S3_REGION=must-not-leak \
  OPENAI_API_KEY=must-not-leak \
  "$repo_root/deploy/render-production-env.sh" "$bootstrap_env"

for integration_key in ALERTMANAGER_WEBHOOK_URL YANDEX_CLIENT_ID OBSERVABILITY_ADMIN_USERS MAX_BOT_TOKEN S3_HOST S3_ACCESS_KEY S3_SECRET_KEY S3_BUCKET S3_REGION OPENAI_API_KEY; do
  grep -Fx "$integration_key=" "$bootstrap_env" >/dev/null
  awk -F= -v key="$integration_key" \
    '$1 == key { print key "=must-not-be-present"; next } { print }' \
    "$bootstrap_env" >"$sandbox/tampered-$integration_key.env"
  if "$repo_root/deploy/validate-production-env.sh" "$sandbox/tampered-$integration_key.env" >/dev/null 2>&1; then
    echo "Bootstrap validation accepted a non-empty $integration_key" >&2
    exit 1
  fi
done
