#!/usr/bin/env bash
set -euo pipefail

repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
sandbox=$(mktemp -d)
trap 'rm -rf "$sandbox"' EXIT

for smtp_key in SMTP_HOST SMTP_PORT SMTP_USERNAME SMTP_PASSWORD SMTP_FROM_EMAIL SMTP_FROM_NAME; do
  grep -F "          $smtp_key: \${{ secrets.$smtp_key }}" "$repo_root/.github/workflows/deploy.yml" >/dev/null
  grep -F "      $smtp_key: \${$smtp_key}" "$repo_root/deploy/compose.production.yaml" >/dev/null
done
for billing_key in YOOKASSA_SHOP_ID YOOKASSA_SECRET_KEY YOOKASSA_DATA_KEY; do
  grep -F "          $billing_key: \${{ secrets.$billing_key }}" "$repo_root/.github/workflows/deploy.yml" >/dev/null
  grep -F "      $billing_key: \${$billing_key}" "$repo_root/deploy/compose.production.yaml" >/dev/null
done
for billing_flag in BILLING_LIVE_ENABLED YOOKASSA_RECEIPTS_CONFIRMED; do
  grep -F "          $billing_flag: \${{ vars.$billing_flag || 'false' }}" "$repo_root/.github/workflows/deploy.yml" >/dev/null
  grep -F "      $billing_flag: \${$billing_flag}" "$repo_root/deploy/compose.production.yaml" >/dev/null
done
for direct_secret in DIRECT_OAUTH_CLIENT_ID DIRECT_OAUTH_CLIENT_SECRET DIRECT_TOKEN_DATA_KEY; do
  grep -F "          $direct_secret: \${{ secrets.$direct_secret }}" "$repo_root/.github/workflows/deploy.yml" >/dev/null
  grep -F "      $direct_secret: \${$direct_secret}" "$repo_root/deploy/compose.production.yaml" >/dev/null
done
for direct_var in DIRECT_OAUTH_REDIRECT_URI DIRECT_API_BASE_URL; do
  grep -F "          $direct_var: \${{ vars.$direct_var }}" "$repo_root/.github/workflows/deploy.yml" >/dev/null
  grep -F "      $direct_var: \${$direct_var}" "$repo_root/deploy/compose.production.yaml" >/dev/null
done
for direct_flag in DIRECT_WRITES_ENABLED DIRECT_AUTO_LAUNCH_ENABLED; do
  grep -F "          $direct_flag: \${{ vars.$direct_flag || 'false' }}" "$repo_root/.github/workflows/deploy.yml" >/dev/null
  grep -F "      $direct_flag: \${$direct_flag}" "$repo_root/deploy/compose.production.yaml" >/dev/null
done
grep -F "          DIRECT_SANDBOX: \${{ vars.DIRECT_SANDBOX || 'true' }}" "$repo_root/.github/workflows/deploy.yml" >/dev/null
grep -F '      DIRECT_SANDBOX: ${DIRECT_SANDBOX}' "$repo_root/deploy/compose.production.yaml" >/dev/null

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
    YOOKASSA_SHOP_ID=123456 \
    YOOKASSA_SECRET_KEY=test_live_secret_key \
    YOOKASSA_DATA_KEY=MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY= \
    BILLING_ENFORCEMENT_ENABLED=false \
    BILLING_LIVE_ENABLED=false \
    YOOKASSA_RECEIPTS_CONFIRMED=false \
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
grep -Fx 'MEDIA_USER_MAX_BYTES=10737418240' "$production_env" >/dev/null
grep -Fx 'MEDIA_ORPHAN_GRACE_PERIOD=24h' "$production_env" >/dev/null
grep -Fx 'MEDIA_CLEANUP_INTERVAL=15m' "$production_env" >/dev/null
grep -Fx 'MEDIA_CLEANUP_BATCH_SIZE=50' "$production_env" >/dev/null
grep -Fx 'WORKSPACE_MAX_OWNED_TEAM_WORKSPACES=5' "$production_env" >/dev/null
grep -Fx 'DIRECT_OAUTH_CLIENT_ID=' "$production_env" >/dev/null
grep -Fx 'DIRECT_OAUTH_CLIENT_SECRET=' "$production_env" >/dev/null
grep -Fx 'DIRECT_OAUTH_REDIRECT_URI=' "$production_env" >/dev/null
grep -Fx 'DIRECT_TOKEN_DATA_KEY=' "$production_env" >/dev/null
grep -Fx 'DIRECT_API_BASE_URL=https://api-sandbox.direct.yandex.com/json/v5' "$production_env" >/dev/null
grep -Fx 'DIRECT_WRITES_ENABLED=false' "$production_env" >/dev/null
grep -Fx 'DIRECT_AUTO_LAUNCH_ENABLED=false' "$production_env" >/dev/null
grep -Fx 'DIRECT_SANDBOX=true' "$production_env" >/dev/null
grep -Fx 'BILLING_ENFORCEMENT_ENABLED=false' "$production_env" >/dev/null
grep -Fx 'BILLING_LIVE_ENABLED=false' "$production_env" >/dev/null
grep -Fx 'YOOKASSA_RECEIPTS_CONFIRMED=false' "$production_env" >/dev/null
grep -Fx 'YOOKASSA_SHOP_ID=123456' "$production_env" >/dev/null
grep -Fx 'YOOKASSA_SECRET_KEY=test_live_secret_key' "$production_env" >/dev/null
grep -Fx 'YOOKASSA_DATA_KEY=MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=' "$production_env" >/dev/null
grep -Fx 'YOOKASSA_RETURN_URL=https://maxposty.ru/app/?billing=pending#/workspace/settings/plan' "$production_env" >/dev/null
grep -Fx 'SMTP_HOST=' "$production_env" >/dev/null
grep -Fx 'SMTP_PORT=587' "$production_env" >/dev/null
grep -Fx 'SMTP_USERNAME=' "$production_env" >/dev/null
grep -Fx 'SMTP_PASSWORD=' "$production_env" >/dev/null
grep -Fx 'SMTP_FROM_EMAIL=' "$production_env" >/dev/null
grep -Fx 'SMTP_FROM_NAME=MaxPosty' "$production_env" >/dev/null
"$repo_root/deploy/validate-production-env.sh" "$production_env"

configured_smtp_env="$sandbox/configured-smtp.env"
render_production "$configured_smtp_env" \
  SMTP_HOST=smtp.example.test \
  SMTP_PORT=587 \
  SMTP_USERNAME=mailer@example.test \
  SMTP_PASSWORD=test-smtp-password \
  SMTP_FROM_EMAIL=noreply@example.test \
  SMTP_FROM_NAME=MaxPosty
grep -Fx 'SMTP_HOST=smtp.example.test' "$configured_smtp_env" >/dev/null
grep -Fx 'SMTP_PORT=587' "$configured_smtp_env" >/dev/null
grep -Fx 'SMTP_USERNAME=mailer@example.test' "$configured_smtp_env" >/dev/null
grep -Fx 'SMTP_PASSWORD=test-smtp-password' "$configured_smtp_env" >/dev/null
grep -Fx 'SMTP_FROM_EMAIL=noreply@example.test' "$configured_smtp_env" >/dev/null
grep -Fx 'SMTP_FROM_NAME=MaxPosty' "$configured_smtp_env" >/dev/null
"$repo_root/deploy/validate-production-env.sh" "$configured_smtp_env"

if render_production "$sandbox/partial-smtp.env" SMTP_HOST=smtp.example.test >/dev/null 2>&1; then
  echo "Production render accepted a partial SMTP configuration" >&2
  exit 1
fi

if render_production "$sandbox/invalid-smtp-port.env" \
  SMTP_HOST=smtp.example.test \
  SMTP_PORT=70000 \
  SMTP_USERNAME=mailer@example.test \
  SMTP_PASSWORD=test-smtp-password \
  SMTP_FROM_EMAIL=noreply@example.test >/dev/null 2>&1; then
  echo "Production render accepted an invalid SMTP port" >&2
  exit 1
fi

configured_s3_env="$sandbox/configured-s3.env"
render_production "$configured_s3_env" S3_BUCKET=media.maxposty.ru S3_REGION=ru-1
grep -Fx 'S3_BUCKET=media.maxposty.ru' "$configured_s3_env" >/dev/null
grep -Fx 'S3_REGION=ru-1' "$configured_s3_env" >/dev/null
"$repo_root/deploy/validate-production-env.sh" "$configured_s3_env"

configured_media_env="$sandbox/configured-media.env"
render_production "$configured_media_env" \
  MEDIA_USER_MAX_FILES=750 \
  MEDIA_USER_MAX_BYTES=21474836480 \
  MEDIA_ORPHAN_GRACE_PERIOD=48h \
  MEDIA_CLEANUP_INTERVAL=30m \
  MEDIA_CLEANUP_BATCH_SIZE=75
grep -Fx 'MEDIA_USER_MAX_FILES=750' "$configured_media_env" >/dev/null
grep -Fx 'MEDIA_USER_MAX_BYTES=21474836480' "$configured_media_env" >/dev/null
grep -Fx 'MEDIA_ORPHAN_GRACE_PERIOD=48h' "$configured_media_env" >/dev/null
grep -Fx 'MEDIA_CLEANUP_INTERVAL=30m' "$configured_media_env" >/dev/null
grep -Fx 'MEDIA_CLEANUP_BATCH_SIZE=75' "$configured_media_env" >/dev/null
"$repo_root/deploy/validate-production-env.sh" "$configured_media_env"

configured_workspace_env="$sandbox/configured-workspace.env"
render_production "$configured_workspace_env" WORKSPACE_MAX_OWNED_TEAM_WORKSPACES=25
grep -Fx 'WORKSPACE_MAX_OWNED_TEAM_WORKSPACES=25' "$configured_workspace_env" >/dev/null
"$repo_root/deploy/validate-production-env.sh" "$configured_workspace_env"

configured_billing_env="$sandbox/configured-billing.env"
render_production "$configured_billing_env" \
  BILLING_ENFORCEMENT_ENABLED=true \
  BILLING_LIVE_ENABLED=true \
  YOOKASSA_RECEIPTS_CONFIRMED=true
grep -Fx 'BILLING_ENFORCEMENT_ENABLED=true' "$configured_billing_env" >/dev/null
grep -Fx 'BILLING_LIVE_ENABLED=true' "$configured_billing_env" >/dev/null
grep -Fx 'YOOKASSA_RECEIPTS_CONFIRMED=true' "$configured_billing_env" >/dev/null
"$repo_root/deploy/validate-production-env.sh" "$configured_billing_env"

configured_direct_env="$sandbox/configured-direct.env"
render_production "$configured_direct_env" \
  DIRECT_OAUTH_CLIENT_ID=direct-client-id \
  DIRECT_OAUTH_CLIENT_SECRET=direct-client-secret \
  DIRECT_OAUTH_REDIRECT_URI=https://maxposty.ru/api/v1/advertising/direct/oauth/callback \
  DIRECT_TOKEN_DATA_KEY=MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY= \
  DIRECT_WRITES_ENABLED=true \
  DIRECT_AUTO_LAUNCH_ENABLED=true
grep -Fx 'DIRECT_WRITES_ENABLED=true' "$configured_direct_env" >/dev/null
grep -Fx 'DIRECT_AUTO_LAUNCH_ENABLED=true' "$configured_direct_env" >/dev/null
"$repo_root/deploy/validate-production-env.sh" "$configured_direct_env"

if render_production "$sandbox/partial-direct.env" \
  DIRECT_OAUTH_CLIENT_ID=direct-client-id >/dev/null 2>&1; then
  echo "Production render accepted partial Yandex Direct credentials" >&2
  exit 1
fi

if render_production "$sandbox/unsafe-direct-auto-launch.env" \
  DIRECT_AUTO_LAUNCH_ENABLED=true >/dev/null 2>&1; then
  echo "Production render accepted Yandex Direct auto-launch without writes and credentials" >&2
  exit 1
fi

if render_production "$sandbox/unsafe-live.env" \
  BILLING_LIVE_ENABLED=true BILLING_ENFORCEMENT_ENABLED=true >/dev/null 2>&1; then
  echo "Production render accepted live billing without receipt confirmation" >&2
  exit 1
fi

awk -F= '$1 == "BILLING_ENFORCEMENT_ENABLED" { print "BILLING_ENFORCEMENT_ENABLED=sometimes"; next } { print }' \
  "$production_env" >"$sandbox/invalid-billing-enforcement.env"
if "$repo_root/deploy/validate-production-env.sh" "$sandbox/invalid-billing-enforcement.env" >/dev/null 2>&1; then
  echo "Production validation accepted invalid BILLING_ENFORCEMENT_ENABLED" >&2
  exit 1
fi

for required_secret in \
  POSTGRES_MONITOR_PASSWORD GRAFANA_ADMIN_PASSWORD GRAFANA_SECRET_KEY \
  YANDEX_CLIENT_ID YANDEX_CLIENT_SECRET MAX_BOT_TOKEN MAX_WEBHOOK_SECRET \
  S3_HOST S3_ACCESS_KEY S3_SECRET_KEY; do
  if render_production "$sandbox/missing-$required_secret.env" "$required_secret=" >/dev/null 2>&1; then
    echo "Production render accepted an empty $required_secret" >&2
    exit 1
  fi
done

for workspace_limit in 0 1001 not-a-number; do
  awk -F= -v value="$workspace_limit" \
    '$1 == "WORKSPACE_MAX_OWNED_TEAM_WORKSPACES" { print "WORKSPACE_MAX_OWNED_TEAM_WORKSPACES=" value; next } { print }' \
    "$production_env" >"$sandbox/invalid-workspace-limit.env"
  if "$repo_root/deploy/validate-production-env.sh" "$sandbox/invalid-workspace-limit.env" >/dev/null 2>&1; then
    echo "Production validation accepted unsafe WORKSPACE_MAX_OWNED_TEAM_WORKSPACES=$workspace_limit" >&2
    exit 1
  fi
done

without_alerts_env="$sandbox/without-alerts.env"
render_production "$without_alerts_env" ALERTMANAGER_WEBHOOK_URL=
grep -Fx 'ALERTMANAGER_WEBHOOK_URL=' "$without_alerts_env" >/dev/null
"$repo_root/deploy/validate-production-env.sh" "$without_alerts_env"

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
  'MEDIA_USER_MAX_BYTES=1073741824' \
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
  DIRECT_OAUTH_CLIENT_ID=must-not-leak \
  DIRECT_OAUTH_CLIENT_SECRET=must-not-leak \
  DIRECT_OAUTH_REDIRECT_URI=must-not-leak \
  DIRECT_TOKEN_DATA_KEY=must-not-leak \
  DIRECT_WRITES_ENABLED=true \
  DIRECT_AUTO_LAUNCH_ENABLED=true \
  DIRECT_SANDBOX=false \
  OBSERVABILITY_ADMIN_USERS=must-not-leak \
  MAX_BOT_TOKEN=must-not-leak \
  S3_HOST=must-not-leak \
  S3_ACCESS_KEY=must-not-leak \
  S3_SECRET_KEY=must-not-leak \
  S3_BUCKET=must-not-leak \
  S3_REGION=must-not-leak \
  OPENAI_API_KEY=must-not-leak \
  SMTP_HOST=must-not-leak \
  SMTP_USERNAME=must-not-leak \
  SMTP_PASSWORD=must-not-leak \
  SMTP_FROM_EMAIL=must-not-leak \
  "$repo_root/deploy/render-production-env.sh" "$bootstrap_env"

for integration_key in ALERTMANAGER_WEBHOOK_URL YANDEX_CLIENT_ID OBSERVABILITY_ADMIN_USERS DIRECT_OAUTH_CLIENT_ID DIRECT_OAUTH_CLIENT_SECRET DIRECT_OAUTH_REDIRECT_URI DIRECT_TOKEN_DATA_KEY MAX_BOT_TOKEN S3_HOST S3_ACCESS_KEY S3_SECRET_KEY S3_BUCKET S3_REGION OPENAI_API_KEY SMTP_HOST SMTP_USERNAME SMTP_PASSWORD SMTP_FROM_EMAIL; do
  grep -Fx "$integration_key=" "$bootstrap_env" >/dev/null
  awk -F= -v key="$integration_key" \
    '$1 == key { print key "=must-not-be-present"; next } { print }' \
    "$bootstrap_env" >"$sandbox/tampered-$integration_key.env"
  if "$repo_root/deploy/validate-production-env.sh" "$sandbox/tampered-$integration_key.env" >/dev/null 2>&1; then
    echo "Bootstrap validation accepted a non-empty $integration_key" >&2
    exit 1
  fi
done
