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
    YANDEX_CLIENT_ID=yandex-client-id \
    YANDEX_CLIENT_SECRET=yandex-client-secret \
    MAX_BOT_TOKEN=max-bot-token \
    MAX_WEBHOOK_SECRET=0123456789abcdef0123456789abcdef \
    OPENAI_API_KEY= \
    "$@" \
    "$repo_root/deploy/render-production-env.sh" "$output"
}

production_env="$sandbox/production.env"
render_production "$production_env"
grep -Fx 'AUTH_BOOTSTRAP_MODE=false' "$production_env" >/dev/null
grep -Fx 'OPENAI_API_KEY=' "$production_env" >/dev/null
"$repo_root/deploy/validate-production-env.sh" "$production_env"

for required_secret in YANDEX_CLIENT_ID YANDEX_CLIENT_SECRET MAX_BOT_TOKEN MAX_WEBHOOK_SECRET; do
  if render_production "$sandbox/missing-$required_secret.env" "$required_secret=" >/dev/null 2>&1; then
    echo "Production render accepted an empty $required_secret" >&2
    exit 1
  fi
done

bootstrap_env="$sandbox/bootstrap.env"
env \
  DEPLOY_STAGE=bootstrap \
  POSTGRES_OWNER_PASSWORD=owner_password_0123456789abcdef0123456789abcdef \
  POSTGRES_APP_PASSWORD=app_password_0123456789abcdef0123456789abcdef \
  YANDEX_CLIENT_ID=must-not-leak \
  MAX_BOT_TOKEN=must-not-leak \
  OPENAI_API_KEY=must-not-leak \
  "$repo_root/deploy/render-production-env.sh" "$bootstrap_env"

for integration_key in YANDEX_CLIENT_ID MAX_BOT_TOKEN OPENAI_API_KEY; do
  grep -Fx "$integration_key=" "$bootstrap_env" >/dev/null
  awk -F= -v key="$integration_key" \
    '$1 == key { print key "=must-not-be-present"; next } { print }' \
    "$bootstrap_env" >"$sandbox/tampered-$integration_key.env"
  if "$repo_root/deploy/validate-production-env.sh" "$sandbox/tampered-$integration_key.env" >/dev/null 2>&1; then
    echo "Bootstrap validation accepted a non-empty $integration_key" >&2
    exit 1
  fi
done
