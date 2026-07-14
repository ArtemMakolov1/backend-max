#!/bin/sh
set -eu

: "${POSTGRES_DB:?POSTGRES_DB is required}"
: "${POSTGRES_OWNER_USER:?POSTGRES_OWNER_USER is required}"
: "${POSTGRES_OWNER_PASSWORD:?POSTGRES_OWNER_PASSWORD is required}"
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

validate_password() {
  value_name=$1
  value=$2
  case "$value" in
    *[!a-zA-Z0-9_-]*|'')
      echo "$value_name must be URL-safe (letters, digits, underscore or hyphen)" >&2
      exit 1
      ;;
  esac
  if [ "${#value}" -lt 32 ]; then
    echo "$value_name must contain at least 32 characters" >&2
    exit 1
  fi
}

validate_identifier POSTGRES_DB "$POSTGRES_DB"
validate_identifier POSTGRES_OWNER_USER "$POSTGRES_OWNER_USER"
validate_identifier POSTGRES_MONITOR_USER "$POSTGRES_MONITOR_USER"
validate_password POSTGRES_OWNER_PASSWORD "$POSTGRES_OWNER_PASSWORD"
validate_password POSTGRES_MONITOR_PASSWORD "$POSTGRES_MONITOR_PASSWORD"
if [ "$POSTGRES_OWNER_USER" = "$POSTGRES_MONITOR_USER" ]; then
  echo "POSTGRES_MONITOR_USER must differ from POSTGRES_OWNER_USER" >&2
  exit 1
fi

# The role is deliberately read-only and receives PostgreSQL's predefined
# pg_monitor capability. It can inspect server statistics but cannot read
# application tables, mutate tenant data, or create database objects.
PGPASSWORD=$POSTGRES_OWNER_PASSWORD psql \
  --host=postgres \
  --port=5432 \
  --username="$POSTGRES_OWNER_USER" \
  --dbname="$POSTGRES_DB" \
  --set=ON_ERROR_STOP=1 \
  --set=monitor_user="$POSTGRES_MONITOR_USER" \
  --set=monitor_password="$POSTGRES_MONITOR_PASSWORD" \
  --set=database_name="$POSTGRES_DB" <<'SQL'
SELECT format(
    'CREATE ROLE %I LOGIN PASSWORD %L NOSUPERUSER NOCREATEDB NOCREATEROLE INHERIT NOREPLICATION NOBYPASSRLS',
    :'monitor_user', :'monitor_password'
)
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = :'monitor_user') \gexec

SELECT format(
    'ALTER ROLE %I LOGIN PASSWORD %L NOSUPERUSER NOCREATEDB NOCREATEROLE INHERIT NOREPLICATION NOBYPASSRLS',
    :'monitor_user', :'monitor_password'
) \gexec
SELECT format('ALTER ROLE %I SET default_transaction_read_only = on', :'monitor_user') \gexec
SELECT format('ALTER ROLE %I SET statement_timeout = %L', :'monitor_user', '10s') \gexec
SELECT format('ALTER ROLE %I SET lock_timeout = %L', :'monitor_user', '2s') \gexec
SELECT format('REVOKE %I FROM %I', granted.rolname, :'monitor_user')
FROM pg_auth_members memberships
JOIN pg_roles granted ON granted.oid = memberships.roleid
JOIN pg_roles member ON member.oid = memberships.member
WHERE member.rolname = :'monitor_user'
  AND granted.rolname <> 'pg_monitor' \gexec
SELECT format('REVOKE ALL PRIVILEGES ON ALL TABLES IN SCHEMA public FROM %I', :'monitor_user') \gexec
SELECT format('REVOKE ALL PRIVILEGES ON ALL SEQUENCES IN SCHEMA public FROM %I', :'monitor_user') \gexec
SELECT format('REVOKE CREATE, TEMPORARY ON DATABASE %I FROM %I', :'database_name', :'monitor_user') \gexec
SELECT format('GRANT CONNECT ON DATABASE %I TO %I', :'database_name', :'monitor_user') \gexec
SELECT format('GRANT pg_monitor TO %I', :'monitor_user') \gexec

CREATE EXTENSION IF NOT EXISTS pg_stat_statements;
SQL
