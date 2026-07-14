#!/usr/bin/env bash
set -euo pipefail

env_file=${1:-}
if [[ -z "$env_file" || ! -f "$env_file" ]]; then
  echo "Usage: $0 PATH_TO_ENV_FILE" >&2
  exit 2
fi

seen_keys=' '
line_number=0
while IFS= read -r line || [[ -n "$line" ]]; do
  line_number=$((line_number + 1))
  [[ -n "$line" ]] || continue
  [[ "$line" == \#* ]] && continue
  if [[ "$line" != *=* ]]; then
    echo "Invalid production environment line $line_number" >&2
    exit 1
  fi
  key=${line%%=*}
  value=${line#*=}
  if [[ ! "$key" =~ ^[A-Z][A-Z0-9_]*$ ]]; then
    echo "Invalid production environment key on line $line_number" >&2
    exit 1
  fi
  if [[ "$seen_keys" == *" $key "* ]]; then
    echo "Duplicate production environment key: $key" >&2
    exit 1
  fi
  case "$key" in
    POSTGRES_DB|POSTGRES_OWNER_USER|POSTGRES_OWNER_PASSWORD|POSTGRES_APP_USER|POSTGRES_APP_PASSWORD|\
    PGBOUNCER_DEFAULT_POOL_SIZE|PGBOUNCER_MIN_POOL_SIZE|PGBOUNCER_RESERVE_POOL_SIZE|PGBOUNCER_MAX_CLIENT_CONN|\
    PUBLIC_BASE_URL|FRONTEND_ORIGIN|AUTH_BOOTSTRAP_MODE|YANDEX_CLIENT_ID|YANDEX_CLIENT_SECRET|YANDEX_REDIRECT_URI|YANDEX_ALLOWED_USERS|\
    AUTH_SESSION_TTL|OAUTH_TRUST_X_REAL_IP|OAUTH_RATE_LIMIT_AT_EDGE|MAX_API_BASE_URL|MAX_BOT_TOKEN|\
    MAX_WEBHOOK_SECRET|MAX_WEBHOOK_URL|MAX_CA_CERT_FILE|OPENAI_API_KEY|OPENAI_API_BASE_URL|OPENAI_IMAGE_MODEL|\
    OPENAI_RESEARCH_MODEL|AI_GLOBAL_MAX_CONCURRENT|AI_USER_MAX_CONCURRENT|AI_IMAGE_PER_MINUTE|AI_IMAGE_PER_DAY|\
    AI_RESEARCH_PER_MINUTE|AI_RESEARCH_PER_DAY|AI_LEASE_TTL|SCHEDULER_INTERVAL|BACKUP_RETENTION_DAYS)
      ;;
    *)
      echo "Unknown production environment key: $key" >&2
      exit 1
      ;;
  esac
  seen_keys+="$key "
done <"$env_file"

has_key() {
  local requested_key=$1
  grep -q "^${requested_key}=" "$env_file"
}

env_value() {
  local requested_key=$1
  awk -F= -v requested_key="$requested_key" '$1 == requested_key { sub(/^[^=]*=/, ""); print; exit }' "$env_file"
}

required_keys=(
  POSTGRES_DB POSTGRES_OWNER_USER POSTGRES_OWNER_PASSWORD POSTGRES_APP_USER POSTGRES_APP_PASSWORD
  PGBOUNCER_DEFAULT_POOL_SIZE PGBOUNCER_MIN_POOL_SIZE PGBOUNCER_RESERVE_POOL_SIZE PGBOUNCER_MAX_CLIENT_CONN
  PUBLIC_BASE_URL FRONTEND_ORIGIN AUTH_BOOTSTRAP_MODE YANDEX_CLIENT_ID YANDEX_CLIENT_SECRET YANDEX_REDIRECT_URI
  AUTH_SESSION_TTL OAUTH_TRUST_X_REAL_IP OAUTH_RATE_LIMIT_AT_EDGE MAX_API_BASE_URL MAX_BOT_TOKEN
  MAX_WEBHOOK_SECRET MAX_WEBHOOK_URL MAX_CA_CERT_FILE OPENAI_API_KEY OPENAI_API_BASE_URL OPENAI_IMAGE_MODEL
  OPENAI_RESEARCH_MODEL AI_GLOBAL_MAX_CONCURRENT AI_USER_MAX_CONCURRENT AI_IMAGE_PER_MINUTE AI_IMAGE_PER_DAY
  AI_RESEARCH_PER_MINUTE AI_RESEARCH_PER_DAY AI_LEASE_TTL SCHEDULER_INTERVAL BACKUP_RETENTION_DAYS
)
for key in "${required_keys[@]}"; do
  if ! has_key "$key"; then
    echo "Missing production environment key: $key" >&2
    exit 1
  fi
done

nonempty_keys=(
  POSTGRES_DB POSTGRES_OWNER_USER POSTGRES_OWNER_PASSWORD POSTGRES_APP_USER POSTGRES_APP_PASSWORD
)
for key in "${nonempty_keys[@]}"; do
  if [[ -z "$(env_value "$key")" ]]; then
    echo "Production environment key must not be empty: $key" >&2
    exit 1
  fi
done

for key in YANDEX_CLIENT_ID YANDEX_CLIENT_SECRET MAX_BOT_TOKEN OPENAI_API_KEY OPENAI_IMAGE_MODEL OPENAI_RESEARCH_MODEL; do
  value=$(env_value "$key")
  if [[ -n "$value" && ! "$value" =~ ^[A-Za-z0-9._~:/+@,=-]+$ ]]; then
    echo "Production environment key contains unsupported characters: $key" >&2
    exit 1
  fi
done

value=$(env_value YANDEX_ALLOWED_USERS)
if [[ -n "$value" && ! "$value" =~ ^[A-Za-z0-9._@,+-]+$ ]]; then
  echo "YANDEX_ALLOWED_USERS must be a comma-separated list without spaces" >&2
  exit 1
fi

for key in POSTGRES_DB POSTGRES_OWNER_USER POSTGRES_APP_USER; do
  value=$(env_value "$key")
  if [[ ! "$value" =~ ^[A-Za-z0-9_]+$ ]]; then
    echo "$key may contain only letters, digits and underscores" >&2
    exit 1
  fi
done
for key in POSTGRES_OWNER_PASSWORD POSTGRES_APP_PASSWORD; do
  value=$(env_value "$key")
  if [[ ${#value} -lt 32 || ! "$value" =~ ^[A-Za-z0-9_-]+$ ]]; then
    echo "$key must contain at least 32 URL-safe characters" >&2
    exit 1
  fi
done
value=$(env_value MAX_WEBHOOK_SECRET)
if [[ -n "$value" && (${#value} -lt 32 || ${#value} -gt 256 || ! "$value" =~ ^[A-Za-z0-9_-]+$) ]]; then
  echo "MAX_WEBHOOK_SECRET must contain 32-256 URL-safe characters" >&2
  exit 1
fi

expect_exact() {
  local key=$1
  local expected=$2
  if [[ "$(env_value "$key")" != "$expected" ]]; then
    echo "$key must be exactly $expected" >&2
    exit 1
  fi
}
expect_exact OAUTH_TRUST_X_REAL_IP "true"
expect_exact OAUTH_RATE_LIMIT_AT_EDGE "false"
expect_exact MAX_API_BASE_URL "https://platform-api2.max.ru"
expect_exact MAX_WEBHOOK_URL "https://maxposty.ru/api/v1/webhooks/max"
expect_exact OPENAI_API_BASE_URL "https://api.openai.com"

bootstrap_mode=$(env_value AUTH_BOOTSTRAP_MODE)
case "$bootstrap_mode" in
  true)
    expect_exact PUBLIC_BASE_URL "http://178.159.94.83"
    expect_exact FRONTEND_ORIGIN "http://178.159.94.83"
    for key in YANDEX_CLIENT_ID YANDEX_CLIENT_SECRET YANDEX_REDIRECT_URI YANDEX_ALLOWED_USERS MAX_BOT_TOKEN MAX_WEBHOOK_SECRET OPENAI_API_KEY; do
      if [[ -n "$(env_value "$key")" ]]; then
        echo "$key must be empty in fail-closed bootstrap mode" >&2
        exit 1
      fi
    done
    ;;
  false)
    expect_exact PUBLIC_BASE_URL "https://maxposty.ru"
    expect_exact FRONTEND_ORIGIN "https://maxposty.ru"
    expect_exact YANDEX_REDIRECT_URI "https://maxposty.ru/api/v1/auth/yandex/callback"
    for key in YANDEX_CLIENT_ID YANDEX_CLIENT_SECRET MAX_BOT_TOKEN MAX_WEBHOOK_SECRET OPENAI_API_KEY; do
      if [[ -z "$(env_value "$key")" ]]; then
        echo "$key must not be empty in production mode" >&2
        exit 1
      fi
    done
    ;;
  *)
    echo "AUTH_BOOTSTRAP_MODE must be true or false" >&2
    exit 1
    ;;
esac

numeric_keys=(
  PGBOUNCER_DEFAULT_POOL_SIZE PGBOUNCER_MIN_POOL_SIZE PGBOUNCER_RESERVE_POOL_SIZE PGBOUNCER_MAX_CLIENT_CONN
  AI_GLOBAL_MAX_CONCURRENT AI_USER_MAX_CONCURRENT AI_IMAGE_PER_MINUTE AI_IMAGE_PER_DAY
  AI_RESEARCH_PER_MINUTE AI_RESEARCH_PER_DAY BACKUP_RETENTION_DAYS
)
for key in "${numeric_keys[@]}"; do
  value=$(env_value "$key")
  if [[ ! "$value" =~ ^[1-9][0-9]*$ ]]; then
    echo "$key must be a positive integer" >&2
    exit 1
  fi
done
value=$(env_value BACKUP_RETENTION_DAYS)
if (( value > 365 )); then
  echo "BACKUP_RETENTION_DAYS must not exceed 365" >&2
  exit 1
fi

for key in AUTH_SESSION_TTL AI_LEASE_TTL SCHEDULER_INTERVAL; do
  value=$(env_value "$key")
  if [[ ! "$value" =~ ^[1-9][0-9]*(s|m|h)$ ]]; then
    echo "$key must be a positive Go duration using s, m or h" >&2
    exit 1
  fi
done

value=$(env_value MAX_CA_CERT_FILE)
if [[ -n "$value" && ! "$value" =~ ^/app/certs/[A-Za-z0-9._/-]+$ ]]; then
  echo "MAX_CA_CERT_FILE must be empty or point inside /app/certs" >&2
  exit 1
fi
