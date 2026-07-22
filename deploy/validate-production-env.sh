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
    POSTGRES_DB|POSTGRES_OWNER_USER|POSTGRES_OWNER_PASSWORD|POSTGRES_APP_USER|POSTGRES_APP_PASSWORD|POSTGRES_MONITOR_USER|POSTGRES_MONITOR_PASSWORD|\
    PGBOUNCER_DEFAULT_POOL_SIZE|PGBOUNCER_MIN_POOL_SIZE|PGBOUNCER_RESERVE_POOL_SIZE|PGBOUNCER_MAX_CLIENT_CONN|\
    PUBLIC_BASE_URL|FRONTEND_ORIGIN|GRAFANA_ROOT_URL|GRAFANA_ADMIN_PASSWORD|GRAFANA_SECRET_KEY|ALERTMANAGER_WEBHOOK_URL|AUTH_BOOTSTRAP_MODE|YANDEX_CLIENT_ID|YANDEX_CLIENT_SECRET|YANDEX_REDIRECT_URI|YANDEX_ALLOWED_USERS|OBSERVABILITY_ADMIN_USERS|\
    AUTH_SESSION_TTL|WORKSPACE_MAX_OWNED_TEAM_WORKSPACES|OAUTH_TRUST_X_REAL_IP|OAUTH_RATE_LIMIT_AT_EDGE|MAX_API_BASE_URL|MAX_BOT_TOKEN|\
    MAX_WEBHOOK_SECRET|MAX_WEBHOOK_URL|MAX_CA_CERT_FILE|S3_HOST|S3_ACCESS_KEY|S3_SECRET_KEY|S3_BUCKET|S3_REGION|\
    MEDIA_USER_MAX_FILES|MEDIA_USER_MAX_BYTES|MEDIA_ORPHAN_GRACE_PERIOD|MEDIA_CLEANUP_INTERVAL|MEDIA_CLEANUP_BATCH_SIZE|\
    OPENAI_API_KEY|OPENAI_API_BASE_URL|OPENAI_IMAGE_MODEL|\
    OPENAI_RESEARCH_MODEL|AI_GLOBAL_MAX_CONCURRENT|AI_USER_MAX_CONCURRENT|AI_IMAGE_PER_MINUTE|AI_IMAGE_PER_DAY|\
    AI_RESEARCH_PER_MINUTE|AI_RESEARCH_PER_DAY|AI_LEASE_TTL|BILLING_ENFORCEMENT_ENABLED|BILLING_LIVE_ENABLED|YOOKASSA_RECEIPTS_CONFIRMED|YOOKASSA_SHOP_ID|YOOKASSA_SECRET_KEY|YOOKASSA_DATA_KEY|YOOKASSA_RETURN_URL|SCHEDULER_INTERVAL|BACKUP_RETENTION_DAYS|PITR_RETENTION_DAYS|\
    SMTP_HOST|SMTP_PORT|SMTP_USERNAME|SMTP_PASSWORD|SMTP_FROM_EMAIL|SMTP_FROM_NAME)
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
  POSTGRES_DB POSTGRES_OWNER_USER POSTGRES_OWNER_PASSWORD POSTGRES_APP_USER POSTGRES_APP_PASSWORD POSTGRES_MONITOR_USER POSTGRES_MONITOR_PASSWORD
  PGBOUNCER_DEFAULT_POOL_SIZE PGBOUNCER_MIN_POOL_SIZE PGBOUNCER_RESERVE_POOL_SIZE PGBOUNCER_MAX_CLIENT_CONN
  PUBLIC_BASE_URL FRONTEND_ORIGIN GRAFANA_ROOT_URL GRAFANA_ADMIN_PASSWORD GRAFANA_SECRET_KEY ALERTMANAGER_WEBHOOK_URL AUTH_BOOTSTRAP_MODE YANDEX_CLIENT_ID YANDEX_CLIENT_SECRET YANDEX_REDIRECT_URI OBSERVABILITY_ADMIN_USERS
  AUTH_SESSION_TTL WORKSPACE_MAX_OWNED_TEAM_WORKSPACES OAUTH_TRUST_X_REAL_IP OAUTH_RATE_LIMIT_AT_EDGE MAX_API_BASE_URL MAX_BOT_TOKEN
  MAX_WEBHOOK_SECRET MAX_WEBHOOK_URL MAX_CA_CERT_FILE S3_HOST S3_ACCESS_KEY S3_SECRET_KEY S3_BUCKET S3_REGION
  MEDIA_USER_MAX_FILES MEDIA_USER_MAX_BYTES MEDIA_ORPHAN_GRACE_PERIOD MEDIA_CLEANUP_INTERVAL MEDIA_CLEANUP_BATCH_SIZE
  OPENAI_API_KEY OPENAI_API_BASE_URL OPENAI_IMAGE_MODEL
  OPENAI_RESEARCH_MODEL AI_GLOBAL_MAX_CONCURRENT AI_USER_MAX_CONCURRENT AI_IMAGE_PER_MINUTE AI_IMAGE_PER_DAY
  AI_RESEARCH_PER_MINUTE AI_RESEARCH_PER_DAY AI_LEASE_TTL BILLING_ENFORCEMENT_ENABLED BILLING_LIVE_ENABLED YOOKASSA_RECEIPTS_CONFIRMED YOOKASSA_SHOP_ID YOOKASSA_SECRET_KEY YOOKASSA_DATA_KEY YOOKASSA_RETURN_URL SCHEDULER_INTERVAL BACKUP_RETENTION_DAYS PITR_RETENTION_DAYS
  SMTP_HOST SMTP_PORT SMTP_USERNAME SMTP_PASSWORD SMTP_FROM_EMAIL SMTP_FROM_NAME
)
for key in "${required_keys[@]}"; do
  if ! has_key "$key"; then
    echo "Missing production environment key: $key" >&2
    exit 1
  fi
done

nonempty_keys=(
  POSTGRES_DB POSTGRES_OWNER_USER POSTGRES_OWNER_PASSWORD POSTGRES_APP_USER POSTGRES_APP_PASSWORD POSTGRES_MONITOR_USER POSTGRES_MONITOR_PASSWORD
  GRAFANA_ROOT_URL GRAFANA_ADMIN_PASSWORD GRAFANA_SECRET_KEY
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

value=$(env_value OBSERVABILITY_ADMIN_USERS)
if [[ -n "$value" && ! "$value" =~ ^[A-Za-z0-9._+-]+(,[A-Za-z0-9._+-]+)*$ ]]; then
  echo "OBSERVABILITY_ADMIN_USERS must be a comma-separated list without spaces" >&2
  exit 1
fi

for key in POSTGRES_DB POSTGRES_OWNER_USER POSTGRES_APP_USER POSTGRES_MONITOR_USER; do
  value=$(env_value "$key")
  if [[ ! "$value" =~ ^[A-Za-z0-9_]+$ ]]; then
    echo "$key may contain only letters, digits and underscores" >&2
    exit 1
  fi
done
owner_user=$(env_value POSTGRES_OWNER_USER)
app_user=$(env_value POSTGRES_APP_USER)
monitor_user=$(env_value POSTGRES_MONITOR_USER)
if [[ "$owner_user" == "$app_user" || "$owner_user" == "$monitor_user" || "$app_user" == "$monitor_user" ]]; then
  echo "POSTGRES_OWNER_USER, POSTGRES_APP_USER and POSTGRES_MONITOR_USER must be distinct" >&2
  exit 1
fi
for key in POSTGRES_OWNER_PASSWORD POSTGRES_APP_PASSWORD POSTGRES_MONITOR_PASSWORD GRAFANA_ADMIN_PASSWORD GRAFANA_SECRET_KEY; do
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

yookassa_shop_id=$(env_value YOOKASSA_SHOP_ID)
yookassa_secret_key=$(env_value YOOKASSA_SECRET_KEY)
yookassa_data_key=$(env_value YOOKASSA_DATA_KEY)
yookassa_return_url=$(env_value YOOKASSA_RETURN_URL)
if [[ -n "$yookassa_shop_id" || -n "$yookassa_secret_key" || -n "$yookassa_data_key" || -n "$yookassa_return_url" ]]; then
  [[ "$yookassa_shop_id" =~ ^[0-9]{1,64}$ ]] || { echo "YOOKASSA_SHOP_ID must contain 1-64 digits" >&2; exit 1; }
  [[ -n "$yookassa_secret_key" && ${#yookassa_secret_key} -le 512 && "$yookassa_secret_key" =~ ^[A-Za-z0-9._~-]+$ ]] || { echo "YOOKASSA_SECRET_KEY contains unsupported characters" >&2; exit 1; }
  [[ "$yookassa_data_key" =~ ^[A-Za-z0-9+/]{43}=$ ]] || { echo "YOOKASSA_DATA_KEY must be standard base64 for 32 bytes" >&2; exit 1; }
  [[ "$yookassa_return_url" == "https://maxposty.ru/app/?billing=pending#/workspace/settings/plan" ]] || { echo "YOOKASSA_RETURN_URL must be the MaxPosty plan settings callback" >&2; exit 1; }
fi

value=$(env_value ALERTMANAGER_WEBHOOK_URL)
if [[ -n "$value" ]]; then
  if [[ ${#value} -gt 2048 || "$value" != https://* || "$value" == *[[:space:]]* || "$value" == *#* ]]; then
    echo "ALERTMANAGER_WEBHOOK_URL must be a valid HTTPS URL without fragments or whitespace" >&2
    exit 1
  fi
  endpoint=${value#https://}
  authority=${endpoint%%/*}
  authority=${authority%%\?*}
  if [[ -z "$authority" || "$authority" == *"@"* || ! "$authority" =~ ^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?(:[1-9][0-9]{0,4})?$ ]]; then
    echo "ALERTMANAGER_WEBHOOK_URL must use an HTTPS hostname without embedded credentials" >&2
    exit 1
  fi
  hostname=${authority%%:*}
  if [[ "$hostname" == *..* || "$hostname" == *.-* || "$hostname" == *-.* ]]; then
    echo "ALERTMANAGER_WEBHOOK_URL must use a valid HTTPS hostname" >&2
    exit 1
  fi
  if [[ "$authority" == *:* ]]; then
    port=${authority##*:}
    if ((port > 65535)); then
      echo "ALERTMANAGER_WEBHOOK_URL port must not exceed 65535" >&2
      exit 1
    fi
  fi
fi

value=$(env_value S3_HOST)
if [[ -n "$value" ]]; then
  case "$value" in
    https://*) endpoint=${value#https://} ;;
    *://*)
      echo "S3_HOST must use HTTPS when a URL scheme is provided" >&2
      exit 1
      ;;
    *) endpoint=$value ;;
  esac
  endpoint=${endpoint%/}
  if [[ ! "$endpoint" =~ ^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?(:[1-9][0-9]{0,4})?$ ]]; then
    echo "S3_HOST must be an HTTPS S3 endpoint without path, query or credentials" >&2
    exit 1
  fi
fi

for key in S3_ACCESS_KEY S3_SECRET_KEY; do
  value=$(env_value "$key")
  if [[ -n "$value" && ! "$value" =~ ^[A-Za-z0-9._~+/=-]+$ ]]; then
    echo "$key contains unsupported characters" >&2
    exit 1
  fi
done

value=$(env_value S3_BUCKET)
if [[ -n "$value" ]]; then
  if [[ ! "$value" =~ ^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$ ]] ||
     [[ "$value" == *".."* || "$value" == *".-"* || "$value" == *"-."* ]] ||
     [[ "$value" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]]; then
    echo "S3_BUCKET must be empty or a valid HOSTKEY bucket name" >&2
    exit 1
  fi
fi

value=$(env_value S3_REGION)
if [[ -n "$value" && ! "$value" =~ ^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$ ]]; then
  echo "S3_REGION must be empty or a valid S3 region name" >&2
  exit 1
fi

smtp_port=$(env_value SMTP_PORT)
if [[ ! "$smtp_port" =~ ^[1-9][0-9]*$ ]] || ((smtp_port > 65535)); then
  echo "SMTP_PORT must be an integer between 1 and 65535" >&2
  exit 1
fi

smtp_configured=false
for key in SMTP_HOST SMTP_USERNAME SMTP_PASSWORD SMTP_FROM_EMAIL; do
  if [[ -n "$(env_value "$key")" ]]; then
    smtp_configured=true
    break
  fi
done
if [[ "$smtp_configured" == true ]]; then
  for key in SMTP_HOST SMTP_USERNAME SMTP_PASSWORD SMTP_FROM_EMAIL; do
    if [[ -z "$(env_value "$key")" ]]; then
      echo "$key must not be empty when SMTP delivery is configured" >&2
      exit 1
    fi
  done
  smtp_host=$(env_value SMTP_HOST)
  if [[ ! "$smtp_host" =~ ^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])$ ]] || [[ "$smtp_host" == *..* ]]; then
    echo "SMTP_HOST must be a valid hostname" >&2
    exit 1
  fi
  smtp_from_email=$(env_value SMTP_FROM_EMAIL)
  if [[ ! "$smtp_from_email" =~ ^[^[:space:]@]+@[^[:space:]@]+\.[^[:space:]@]+$ ]]; then
    echo "SMTP_FROM_EMAIL must be a valid email address" >&2
    exit 1
  fi
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

case "$(env_value BILLING_ENFORCEMENT_ENABLED)" in
  true|false) ;;
  *)
    echo "BILLING_ENFORCEMENT_ENABLED must be true or false" >&2
    exit 1
    ;;
esac

case "$(env_value BILLING_LIVE_ENABLED)" in
  true|false) ;;
  *) echo "BILLING_LIVE_ENABLED must be true or false" >&2; exit 1 ;;
esac
case "$(env_value YOOKASSA_RECEIPTS_CONFIRMED)" in
  true|false) ;;
  *) echo "YOOKASSA_RECEIPTS_CONFIRMED must be true or false" >&2; exit 1 ;;
esac
if [[ "$(env_value BILLING_LIVE_ENABLED)" == "true" ]]; then
  [[ "$(env_value BILLING_ENFORCEMENT_ENABLED)" == "true" ]] || { echo "BILLING_LIVE_ENABLED requires BILLING_ENFORCEMENT_ENABLED=true" >&2; exit 1; }
  [[ "$(env_value YOOKASSA_RECEIPTS_CONFIRMED)" == "true" ]] || { echo "BILLING_LIVE_ENABLED requires YOOKASSA_RECEIPTS_CONFIRMED=true" >&2; exit 1; }
  [[ -n "$yookassa_shop_id" && -n "$yookassa_secret_key" && -n "$yookassa_data_key" && -n "$yookassa_return_url" ]] || { echo "BILLING_LIVE_ENABLED requires complete YooKassa configuration" >&2; exit 1; }
fi

bootstrap_mode=$(env_value AUTH_BOOTSTRAP_MODE)
case "$bootstrap_mode" in
  true)
    expect_exact PUBLIC_BASE_URL "http://178.159.94.83"
    expect_exact FRONTEND_ORIGIN "http://178.159.94.83"
    expect_exact GRAFANA_ROOT_URL "http://178.159.94.83/monitoring/"
    for key in ALERTMANAGER_WEBHOOK_URL YANDEX_CLIENT_ID YANDEX_CLIENT_SECRET YANDEX_REDIRECT_URI YANDEX_ALLOWED_USERS OBSERVABILITY_ADMIN_USERS MAX_BOT_TOKEN MAX_WEBHOOK_SECRET S3_HOST S3_ACCESS_KEY S3_SECRET_KEY S3_BUCKET S3_REGION OPENAI_API_KEY YOOKASSA_SHOP_ID YOOKASSA_SECRET_KEY YOOKASSA_DATA_KEY YOOKASSA_RETURN_URL SMTP_HOST SMTP_USERNAME SMTP_PASSWORD SMTP_FROM_EMAIL; do
      if [[ -n "$(env_value "$key")" ]]; then
        echo "$key must be empty in fail-closed bootstrap mode" >&2
        exit 1
      fi
    done
    ;;
  false)
    expect_exact PUBLIC_BASE_URL "https://maxposty.ru"
    expect_exact FRONTEND_ORIGIN "https://maxposty.ru"
    expect_exact GRAFANA_ROOT_URL "https://maxposty.ru/monitoring/"
    expect_exact YANDEX_REDIRECT_URI "https://maxposty.ru/api/v1/auth/yandex/callback"
    for key in YANDEX_CLIENT_ID YANDEX_CLIENT_SECRET MAX_BOT_TOKEN MAX_WEBHOOK_SECRET S3_HOST S3_ACCESS_KEY S3_SECRET_KEY YOOKASSA_SHOP_ID YOOKASSA_SECRET_KEY YOOKASSA_DATA_KEY YOOKASSA_RETURN_URL; do
      if [[ -z "$(env_value "$key")" ]]; then
        echo "$key must not be empty in production mode" >&2
        exit 1
      fi
    done
    if [[ -z "$(env_value OBSERVABILITY_ADMIN_USERS)" ]]; then
      echo "OBSERVABILITY_ADMIN_USERS must not be empty in production mode" >&2
      exit 1
    fi
    ;;
  *)
    echo "AUTH_BOOTSTRAP_MODE must be true or false" >&2
    exit 1
    ;;
esac

numeric_keys=(
  PGBOUNCER_DEFAULT_POOL_SIZE PGBOUNCER_MIN_POOL_SIZE PGBOUNCER_RESERVE_POOL_SIZE PGBOUNCER_MAX_CLIENT_CONN
  MEDIA_USER_MAX_FILES MEDIA_USER_MAX_BYTES MEDIA_CLEANUP_BATCH_SIZE
  WORKSPACE_MAX_OWNED_TEAM_WORKSPACES
  AI_GLOBAL_MAX_CONCURRENT AI_USER_MAX_CONCURRENT AI_IMAGE_PER_MINUTE AI_IMAGE_PER_DAY
  AI_RESEARCH_PER_MINUTE AI_RESEARCH_PER_DAY BACKUP_RETENTION_DAYS PITR_RETENTION_DAYS
)
for key in "${numeric_keys[@]}"; do
  value=$(env_value "$key")
  if [[ ! "$value" =~ ^[1-9][0-9]*$ ]]; then
    echo "$key must be a positive integer" >&2
    exit 1
  fi
done
value=$(env_value MEDIA_USER_MAX_FILES)
if (( value > 100000 )); then
  echo "MEDIA_USER_MAX_FILES must not exceed 100000" >&2
  exit 1
fi
value=$(env_value MEDIA_USER_MAX_BYTES)
if (( value > 1125899906842624 )); then
  echo "MEDIA_USER_MAX_BYTES must not exceed 1125899906842624" >&2
  exit 1
fi
if (( value < 10737418240 )); then
  echo "MEDIA_USER_MAX_BYTES must be at least 10737418240 for the current paid plans" >&2
  exit 1
fi
value=$(env_value MEDIA_CLEANUP_BATCH_SIZE)
if (( value > 1000 )); then
  echo "MEDIA_CLEANUP_BATCH_SIZE must not exceed 1000" >&2
  exit 1
fi
value=$(env_value WORKSPACE_MAX_OWNED_TEAM_WORKSPACES)
if (( value > 1000 )); then
  echo "WORKSPACE_MAX_OWNED_TEAM_WORKSPACES must not exceed 1000" >&2
  exit 1
fi
value=$(env_value BACKUP_RETENTION_DAYS)
if (( value > 365 )); then
  echo "BACKUP_RETENTION_DAYS must not exceed 365" >&2
  exit 1
fi
value=$(env_value PITR_RETENTION_DAYS)
if (( value > 90 )); then
  echo "PITR_RETENTION_DAYS must not exceed 90" >&2
  exit 1
fi

for key in AUTH_SESSION_TTL AI_LEASE_TTL SCHEDULER_INTERVAL MEDIA_ORPHAN_GRACE_PERIOD MEDIA_CLEANUP_INTERVAL; do
  value=$(env_value "$key")
  if [[ ! "$value" =~ ^[1-9][0-9]*(s|m|h)$ ]]; then
    echo "$key must be a positive Go duration using s, m or h" >&2
    exit 1
  fi
done

duration_seconds() {
  local value=$1
  local amount=${value%?}
  case "${value: -1}" in
    s) printf '%s\n' "$amount" ;;
    m) printf '%s\n' "$((amount * 60))" ;;
    h) printf '%s\n' "$((amount * 3600))" ;;
  esac
}

value=$(duration_seconds "$(env_value MEDIA_ORPHAN_GRACE_PERIOD)")
if (( value < 3600 || value > 2592000 )); then
  echo "MEDIA_ORPHAN_GRACE_PERIOD must be between 1h and 720h" >&2
  exit 1
fi
value=$(duration_seconds "$(env_value MEDIA_CLEANUP_INTERVAL)")
if (( value < 60 || value > 86400 )); then
  echo "MEDIA_CLEANUP_INTERVAL must be between 1m and 24h" >&2
  exit 1
fi

value=$(env_value MAX_CA_CERT_FILE)
if [[ -n "$value" && ! "$value" =~ ^/app/certs/[A-Za-z0-9._/-]+$ ]]; then
  echo "MAX_CA_CERT_FILE must be empty or point inside /app/certs" >&2
  exit 1
fi
