#!/usr/bin/env bash
# Explicitly local: creates and removes one database/role in an owned test container.
set -euo pipefail
set +x
umask 077

if [[ ${1:-} != --inside-local-fixture ]]; then
  if [[ ${1:-} == --ci-service ]]; then
    [[ $# == 2 && $2 =~ ^[0-9a-f]{64}$ && ${GITHUB_ACTIONS:-} == true && ${TEST_DATABASE_URL:-} == 'postgresql://maxstudio_test:maxstudio_test_ci_only@127.0.0.1:5432/maxstudio_test?sslmode=disable' ]] || { echo "The explicit synthetic CI PostgreSQL service is required" >&2; exit 2; }
    container=$2
  else
    [[ $# == 1 && $1 =~ ^maxposty-[a-z0-9-]+-pg-[0-9]{8}$ ]] || { echo "An owned disposable PostgreSQL test container is required" >&2; exit 2; }
    container=$1
  fi
  repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
  [[ $(docker inspect --format '{{.Config.Image}} {{.State.Running}}' "$container") == 'postgres:18.4-alpine@sha256:9a8afca54e7861fd90fab5fdf4c42477a6b1cb7d293595148e674e0a3181de15 true' ]] || { echo "Expected pinned disposable PostgreSQL image" >&2; exit 1; }
  private_dir=$(docker exec "$container" mktemp -d /tmp/complimentary-policy.XXXXXX)
  [[ $private_dir =~ ^/tmp/complimentary-policy\.[A-Za-z0-9]+$ ]] || exit 1
  trap 'docker exec "$container" rm -rf "$private_dir" >/dev/null' EXIT
  docker cp "$repo_root/deploy/manage-complimentary-access.sh" "$container:$private_dir/operation.sh" >/dev/null
  docker cp "$repo_root/deploy/tests/complimentary-access-postgres-policy.sh" "$container:$private_dir/policy.sh" >/dev/null
  docker cp "$repo_root/internal/store/migrations" "$container:$private_dir/migrations" >/dev/null
  docker exec "$container" bash "$private_dir/policy.sh" --inside-local-fixture "$private_dir"
  exit
fi

[[ $# == 2 && -f /.dockerenv && $2 =~ ^/tmp/complimentary-policy\.[A-Za-z0-9]+$ && ( ${POSTGRES_USER:-}:${POSTGRES_DB:-} == postgres:maxposty_test || ${POSTGRES_USER:-}:${POSTGRES_DB:-} == maxstudio_test:maxstudio_test ) ]] || { echo "Disposable synthetic fixture guard failed" >&2; exit 2; }
fixture=$2
cluster_database=$POSTGRES_DB
test_database=complimentary_ops_${RANDOM}_${RANDOM}
test_role=complimentary_runtime_${RANDOM}_${RANDOM}
export PGPASSWORD="${POSTGRES_PASSWORD:?local test password required}"
psql_owner() { psql -X --no-password -U "$POSTGRES_USER" -d "${1:-$test_database}" -q -t -A -v ON_ERROR_STOP=1 "${@:2}"; }
cleanup() {
  psql_owner "$cluster_database" -v test_database="$test_database" -v test_role="$test_role" <<'SQL' >/dev/null 2>&1
DROP DATABASE IF EXISTS :"test_database" WITH (FORCE);
DROP ROLE IF EXISTS :"test_role";
SQL
}
trap cleanup EXIT
psql_owner "$POSTGRES_DB" -v test_database="$test_database" -v test_role="$test_role" <<'SQL' >/dev/null
CREATE ROLE :"test_role" NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS;
CREATE DATABASE :"test_database";
SQL
psql_owner "$test_database" -v test_role="$test_role" <<'SQL' >/dev/null
-- Reproduce init-app-role's explicit future-table DML grants.
GRANT USAGE ON SCHEMA public TO :"test_role";
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT,INSERT,UPDATE,DELETE ON TABLES TO :"test_role";
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT USAGE,SELECT,UPDATE ON SEQUENCES TO :"test_role";
CREATE TABLE schema_migrations(version TEXT PRIMARY KEY,checksum_sha256 TEXT NOT NULL CHECK(checksum_sha256~'^[0-9a-f]{64}$'),applied_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP);
SQL
for migration in "$fixture"/migrations/[0-9][0-9][0-9]_*.sql; do
  version=${migration##*/}; version=${version:0:3}
  checksum=$(sha256sum "$migration" | cut -d' ' -f1)
  { printf 'BEGIN;\n'; cat "$migration"; printf '\nINSERT INTO schema_migrations(version,checksum_sha256) VALUES (:%s,:%s);\nCOMMIT;\n' "'version'" "'checksum'"; } |
    psql_owner "$test_database" -v version="$version" -v checksum="$checksum" >"$fixture/migrate.log" 2>&1 || { echo "Disposable fixture migration failed" >&2; exit 1; }
done
psql_owner "$test_database" <<'SQL' >/dev/null
INSERT INTO users(id,email,created_at,updated_at) VALUES
 ('fixture-owner','fixture.owner@example.test',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP),
 ('fixture-other','fixture.other@example.test',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP);
INSERT INTO auth_identities(provider,subject,owner_id,created_at,updated_at) VALUES
 ('yandex','fixture-owner-subject','fixture-owner',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP),
 ('yandex','fixture-other-subject','fixture-other',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP);
INSERT INTO workspaces(id,name,owner_user_id,compat_owner_user_id,created_by)
 VALUES ('fixture-owned-team','Synthetic team','fixture-owner','fixture-owner','fixture-owner');
INSERT INTO workspace_members(workspace_id,user_id,role,created_by)
 VALUES ('fixture-owned-team','fixture-owner','owner','fixture-owner');
SQL

installation="$fixture/backend"
source_sha=0123456789abcdef0123456789abcdef01234567
image=ghcr.io/artemmakolov1/backend-max@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
schema_sha=$(sha256sum "$fixture/migrations/039_account_complimentary_access.sql" | cut -d' ' -f1)
mkdir -p "$fixture/bin" "$installation/releases/$source_sha" "$installation/backups"
ln -s "$installation/releases/$source_sha" "$installation/current"
printf 'BACKEND_IMAGE=%s\n' "$image" >"$installation/releases/$source_sha/.release"
printf 'SYNTHETIC_ACCEPTED_ENV=true\n' >"$installation/releases/$source_sha/.env.production"
cat >"$fixture/bin/docker" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
case ${1:-} in
 inspect)
  case "$*" in
   *'.Config.Image'*) printf '%s\n' "$COMPLIMENTARY_FIXTURE_IMAGE";;
   *'org.opencontainers.image.revision'*) printf '%s\n' "$COMPLIMENTARY_FIXTURE_SOURCE";;
   *'.State.Health.Status'*) echo healthy;;
   *) exit 64;;
  esac;;
 exec)
  [[ ${2:-} == -i && ${3:-} == maxposty-backend-postgres-1 ]] || exit 64
  shift 3
  # Execute the unmodified owner psql command in this disposable test container.
  exec "$@";;
 *) exit 64;;
esac
SH
chmod 0700 "$fixture/bin/docker"
export PATH="$fixture/bin:$PATH" POSTGRES_DB="$test_database"
export COMPLIMENTARY_FIXTURE_IMAGE="$image" COMPLIMENTARY_FIXTURE_SOURCE="$source_sha"
call() { bash "$fixture/operation.sh" "$1" "$source_sha" "${2:-fixture.owner@example.test}" AuditOperator 123456-1 "$installation" "${3:-$schema_sha}"; }
assert_result() { grep -Fxq "$1" "$fixture/result" || { echo "Incorrect sanitized operation result" >&2; exit 1; }; }
fingerprint() {
  psql_owner "$test_database" <<'SQL'
SELECT md5(jsonb_build_array(
 (SELECT COALESCE(jsonb_agg(to_jsonb(r) ORDER BY r.id),'[]'::jsonb) FROM workspaces r),
 (SELECT COALESCE(jsonb_agg(to_jsonb(r) ORDER BY r.workspace_id,r.user_id),'[]'::jsonb) FROM workspace_members r),
 (SELECT COALESCE(jsonb_agg(to_jsonb(r) ORDER BY r.workspace_id),'[]'::jsonb) FROM workspace_subscriptions r),
 (SELECT COALESCE(jsonb_agg(to_jsonb(r) ORDER BY r.workspace_id),'[]'::jsonb) FROM billing_subscription_contracts r),
 (SELECT COALESCE(jsonb_agg(to_jsonb(r) ORDER BY r.id),'[]'::jsonb) FROM billing_subscription_periods r),
 (SELECT COALESCE(jsonb_agg(to_jsonb(r) ORDER BY r.id),'[]'::jsonb) FROM billing_payment_attempts r)
)::text);
SQL
}
unchanged_before=$(fingerprint)
call inspect >"$fixture/result"
assert_result active=false; assert_result changed=false; assert_result owned_workspaces=2
call grant >"$fixture/result"
assert_result active=true; assert_result changed=true; assert_result owned_entitlements_unlimited=true
call inspect >"$fixture/result"
assert_result active=true; assert_result changed=false; assert_result owned_entitlements_unlimited=true
call grant >"$fixture/result"
assert_result active=true; assert_result changed=false
[[ $(psql_owner "$test_database" -c "SELECT count(*) FROM account_complimentary_access_events;") == 1 ]] || exit 1
[[ $(psql_owner "$test_database" -c "SELECT workspace_has_complimentary_access(id) FROM workspaces WHERE owner_user_id='fixture-other';") == f ]] || exit 1

# Runtime can read effective access but cannot activate it or alter its audit.
[[ $(psql_owner "$test_database" -v test_role="$test_role" <<'SQL'
SELECT has_table_privilege(:'test_role','account_complimentary_access','SELECT')
 AND has_table_privilege(:'test_role','account_complimentary_access_events','SELECT')
 AND NOT has_table_privilege(:'test_role','account_complimentary_access','INSERT,UPDATE,DELETE')
 AND NOT has_table_privilege(:'test_role','account_complimentary_access_events','INSERT,UPDATE,DELETE')
 AND NOT has_function_privilege(:'test_role','manage_account_complimentary_access(text,text,text,text)','EXECUTE');
SQL
) == t ]] || { echo "Runtime privileges allow self-grant or prevent reads" >&2; exit 1; }
[[ $(psql_owner "$test_database" -v test_role="$test_role" <<'SQL'
SET ROLE :"test_role";
SELECT workspace_has_complimentary_access('fixture-owned-team')
 AND workspace_entitlement_limit('fixture-owned-team','channels') IS NULL
 AND workspace_entitlement_limit('fixture-owned-team','seats') IS NULL
 AND workspace_entitlement_limit('fixture-owned-team','storage_bytes') IS NULL;
SQL
) == t ]] || exit 1
for attempt in table function audit; do
  case $attempt in
   table) unsafe_sql="UPDATE account_complimentary_access SET active=FALSE,revoked_at=CURRENT_TIMESTAMP;";;
   function) unsafe_sql="SELECT * FROM manage_account_complimentary_access('fixture.owner@example.test','grant','runtime','fixture');";;
   audit) unsafe_sql="DELETE FROM account_complimentary_access_events;";;
  esac
  if { printf 'SET ROLE :"test_role";\n%s\n' "$unsafe_sql"; } | psql_owner "$test_database" -v test_role="$test_role" >"$fixture/denied.log" 2>&1; then
    echo "Runtime privilege negative control unexpectedly succeeded" >&2; exit 1
  fi
done

bad_checksum=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
if call revoke fixture.owner@example.test "$bad_checksum" >"$fixture/result" 2>&1; then echo "Mismatched schema was accepted" >&2; exit 1; fi
psql_owner "$test_database" -c "INSERT INTO schema_migrations(version,checksum_sha256) VALUES ('040',repeat('a',64));" >/dev/null
if call revoke >"$fixture/result" 2>&1; then echo "Future schema was accepted" >&2; exit 1; fi
psql_owner "$test_database" -c "DELETE FROM schema_migrations WHERE version='040';" >/dev/null
if call grant absent.fixture@example.test >"$fixture/result" 2>&1; then echo "Absent identity was accepted" >&2; exit 1; fi
psql_owner "$test_database" <<'SQL' >/dev/null
UPDATE users SET email='fixture.owner@example.test' WHERE id='fixture-other';
SQL
if call grant >"$fixture/result" 2>&1; then echo "Ambiguous identity was accepted" >&2; exit 1; fi
psql_owner "$test_database" -c "UPDATE users SET email='fixture.other@example.test' WHERE id='fixture-other';" >/dev/null
[[ $(psql_owner "$test_database" -c "SELECT count(*) FROM account_complimentary_access_events;") == 1 ]] || { echo "A rejected operation changed the audit" >&2; exit 1; }
call revoke >"$fixture/result"
assert_result active=false; assert_result changed=true; assert_result owned_entitlements_unlimited=false
call inspect >"$fixture/result"
assert_result active=false; assert_result changed=false
call revoke >"$fixture/result"
assert_result changed=false
[[ $(psql_owner "$test_database" -c "SELECT count(*) FROM account_complimentary_access_events;") == 2 ]] || exit 1
[[ $(fingerprint) == "$unchanged_before" ]] || { echo "Grant/revoke changed ownership, roles, subscriptions or payments" >&2; exit 1; }
if grep -Eq 'fixture.owner|fixture-other|fixture-owner|password=' "$fixture/result"; then echo "Public operation output leaked identity" >&2; exit 1; fi
echo "Complimentary access real PostgreSQL policy tests passed: unlimited entitlements, read-only inspect, idempotent grant/revoke, runtime self-grant denied, identity/schema fences, unchanged ownership/roles/billing."
