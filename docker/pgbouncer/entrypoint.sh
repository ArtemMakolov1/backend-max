#!/bin/sh
set -eu

: "${POSTGRES_HOST:?POSTGRES_HOST is required}"
: "${POSTGRES_PORT:?POSTGRES_PORT is required}"
: "${POSTGRES_DB:?POSTGRES_DB is required}"
: "${POSTGRES_APP_USER:?POSTGRES_APP_USER is required}"
: "${POSTGRES_APP_PASSWORD:?POSTGRES_APP_PASSWORD is required}"
: "${POSTGRES_MONITOR_USER:?POSTGRES_MONITOR_USER is required}"
: "${POSTGRES_MONITOR_PASSWORD:?POSTGRES_MONITOR_PASSWORD is required}"

validate_identifier() {
  value_name=$1
  value=$2
  case "$value" in
    *[!a-zA-Z0-9_]*|'')
      echo "$value_name may contain only letters, digits and underscores" >&2
      exit 1
      ;;
  esac
}

validate_numeric() {
  value_name=$1
  value=$2
  case "$value" in
    *[!0-9]*|'')
      echo "$value_name must be numeric" >&2
      exit 1
      ;;
  esac
}

validate_identifier POSTGRES_DB "$POSTGRES_DB"
validate_identifier POSTGRES_APP_USER "$POSTGRES_APP_USER"
validate_identifier POSTGRES_MONITOR_USER "$POSTGRES_MONITOR_USER"

case "$POSTGRES_HOST" in
  *[!a-zA-Z0-9_.-]*|'')
    echo "POSTGRES_HOST contains unsupported characters" >&2
    exit 1
    ;;
esac

case "$POSTGRES_PORT" in
  *[!0-9]*|'')
    echo "POSTGRES_PORT must be numeric" >&2
    exit 1
    ;;
esac

case "$POSTGRES_APP_PASSWORD" in
  *[!a-zA-Z0-9_-]*|'')
    echo "POSTGRES_APP_PASSWORD must be URL-safe (letters, digits, underscore or hyphen)" >&2
    exit 1
    ;;
esac

if [ "${#POSTGRES_APP_PASSWORD}" -lt 32 ]; then
  echo "POSTGRES_APP_PASSWORD must contain at least 32 characters" >&2
  exit 1
fi

case "$POSTGRES_MONITOR_PASSWORD" in
  *[!a-zA-Z0-9_-]*|'')
    echo "POSTGRES_MONITOR_PASSWORD must be URL-safe (letters, digits, underscore or hyphen)" >&2
    exit 1
    ;;
esac

if [ "${#POSTGRES_MONITOR_PASSWORD}" -lt 32 ]; then
  echo "POSTGRES_MONITOR_PASSWORD must contain at least 32 characters" >&2
  exit 1
fi

validate_numeric PGBOUNCER_DEFAULT_POOL_SIZE "$PGBOUNCER_DEFAULT_POOL_SIZE"
validate_numeric PGBOUNCER_MIN_POOL_SIZE "$PGBOUNCER_MIN_POOL_SIZE"
validate_numeric PGBOUNCER_RESERVE_POOL_SIZE "$PGBOUNCER_RESERVE_POOL_SIZE"
validate_numeric PGBOUNCER_MAX_CLIENT_CONN "$PGBOUNCER_MAX_CLIENT_CONN"

config_dir=/tmp/maxstudio-pgbouncer
umask 077
mkdir -p "$config_dir"

cat >"$config_dir/pgbouncer.ini" <<EOF
[databases]
$POSTGRES_DB = host=$POSTGRES_HOST port=$POSTGRES_PORT dbname=$POSTGRES_DB

[pgbouncer]
listen_addr = 0.0.0.0
listen_port = 6432
unix_socket_dir =
auth_type = scram-sha-256
auth_file = $config_dir/userlist.txt
pool_mode = transaction
max_client_conn = $PGBOUNCER_MAX_CLIENT_CONN
default_pool_size = $PGBOUNCER_DEFAULT_POOL_SIZE
min_pool_size = $PGBOUNCER_MIN_POOL_SIZE
reserve_pool_size = $PGBOUNCER_RESERVE_POOL_SIZE
reserve_pool_timeout = 3
max_prepared_statements = 100
server_connect_timeout = 15
server_login_retry = 5
server_idle_timeout = 60
server_lifetime = 3600
query_wait_timeout = 30
query_timeout = 120
client_idle_timeout = 600
idle_transaction_timeout = 60
application_name_add_host = 1
log_connections = 0
log_disconnections = 0
log_pooler_errors = 1
ignore_startup_parameters = extra_float_digits
stats_users = $POSTGRES_MONITOR_USER
EOF

# PgBouncer performs SCRAM-SHA-256 client authentication. The URL-safe plain
# password is kept only in this container's tmpfs so PgBouncer can also use it
# for SCRAM authentication to PostgreSQL; it is never baked into an image.
printf '"%s" "%s"\n' "$POSTGRES_APP_USER" "$POSTGRES_APP_PASSWORD" >"$config_dir/userlist.txt"
printf '"%s" "%s"\n' "$POSTGRES_MONITOR_USER" "$POSTGRES_MONITOR_PASSWORD" >>"$config_dir/userlist.txt"

exec pgbouncer "$config_dir/pgbouncer.ini"
