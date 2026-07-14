#!/bin/sh
set -eu

: "${POSTGRES_DB:?POSTGRES_DB is required}"
: "${POSTGRES_USER:?POSTGRES_USER is required}"
: "${POSTGRES_PASSWORD:?POSTGRES_PASSWORD is required}"
: "${POSTGRES_APP_USER:?POSTGRES_APP_USER is required}"
: "${POSTGRES_APP_PASSWORD:?POSTGRES_APP_PASSWORD is required}"

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

validate_identifier POSTGRES_DB "$POSTGRES_DB"
validate_identifier POSTGRES_USER "$POSTGRES_USER"
validate_identifier POSTGRES_APP_USER "$POSTGRES_APP_USER"

case "$POSTGRES_PASSWORD" in
  *[!a-zA-Z0-9_-]*|'')
    echo "POSTGRES_PASSWORD must be URL-safe (letters, digits, underscore or hyphen)" >&2
    exit 1
    ;;
esac

if [ "${#POSTGRES_PASSWORD}" -lt 32 ]; then
  echo "POSTGRES_PASSWORD must contain at least 32 characters" >&2
  exit 1
fi

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

# This script runs only while the official image initializes a new cluster.
# The database owner keeps DDL/migration rights; the runtime role receives only
# connection and DML privileges, including default privileges for future tables.
psql --set ON_ERROR_STOP=1 \
  --username "$POSTGRES_USER" \
  --dbname "$POSTGRES_DB" \
  --set app_user="$POSTGRES_APP_USER" \
  --set app_password="$POSTGRES_APP_PASSWORD" \
  --set database_name="$POSTGRES_DB" \
  --set owner_user="$POSTGRES_USER" <<'SQL'
CREATE ROLE :"app_user"
  LOGIN
  PASSWORD :'app_password'
  NOSUPERUSER
  NOCREATEDB
  NOCREATEROLE
  NOINHERIT
  NOREPLICATION
  NOBYPASSRLS;

REVOKE ALL ON DATABASE :"database_name" FROM PUBLIC;
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT CONNECT ON DATABASE :"database_name" TO :"app_user";
GRANT USAGE ON SCHEMA public TO :"app_user";

ALTER DEFAULT PRIVILEGES FOR ROLE :"owner_user" IN SCHEMA public
  GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO :"app_user";
ALTER DEFAULT PRIVILEGES FOR ROLE :"owner_user" IN SCHEMA public
  GRANT USAGE, SELECT, UPDATE ON SEQUENCES TO :"app_user";

ALTER ROLE :"app_user" SET statement_timeout = '120s';
ALTER ROLE :"app_user" SET lock_timeout = '10s';
ALTER ROLE :"app_user" SET idle_in_transaction_session_timeout = '60s';
SQL
