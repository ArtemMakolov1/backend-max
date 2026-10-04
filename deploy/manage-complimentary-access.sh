#!/usr/bin/env bash
# Trusted owner-only operation. Never print account identity, credentials or SQL errors.
set -euo pipefail
set +x
umask 077

[[ $# == 7 ]] || { echo "Expected operation, source, approved email, operator, run, installation and schema checksum" >&2; exit 2; }
operation=$1
source_sha=$2
target_email=${3,,}
github_actor=$4
run_reference=$5
installation_dir=$6
schema_checksum=$7
case "$operation" in inspect|grant|revoke) ;; *) echo "Invalid complimentary access operation" >&2; exit 2;; esac
[[ "$source_sha" =~ ^[0-9a-f]{40}$ && "$schema_checksum" =~ ^[0-9a-f]{64}$ ]] || { echo "Exact source and schema checksums are required" >&2; exit 2; }
[[ ${#target_email} -le 254 && "$target_email" =~ ^[a-z0-9][a-z0-9._+-]{0,63}@[a-z0-9][a-z0-9.-]*\.[a-z]{2,63}$ ]] || { echo "A normalized approved account email is required" >&2; exit 2; }
[[ "$github_actor" =~ ^[A-Za-z0-9_-]{1,39}$ && "$run_reference" =~ ^[0-9]{1,20}-[1-9][0-9]{0,5}$ ]] || { echo "Valid GitHub operator and run metadata are required" >&2; exit 2; }
installation_dir=$(CDPATH='' cd -- "$installation_dir" && pwd -P)
[[ -d "$installation_dir/backups" && ! -L "$installation_dir/backups" ]] || { echo "Private production backup directory is required" >&2; exit 1; }
exec 9>"$installation_dir/.deploy.lock"
flock -n 9 || { echo "Another backend production operation is running" >&2; exit 1; }
[[ -L "$installation_dir/current" ]] || { echo "Accepted production release is missing" >&2; exit 1; }
accepted_dir=$(readlink -f "$installation_dir/current")
[[ "$accepted_dir" == "$installation_dir/releases/$source_sha" ]] || { echo "Accepted source differs from the reviewed workflow" >&2; exit 1; }
[[ -f "$accepted_dir/.release" && ! -L "$accepted_dir/.release" && -f "$accepted_dir/.env.production" && ! -L "$accepted_dir/.env.production" ]] || { echo "Accepted production metadata is missing" >&2; exit 1; }
image=$(awk -F= '$1=="BACKEND_IMAGE" {sub(/^[^=]*=/,"");print;exit}' "$accepted_dir/.release")
[[ "$image" =~ ^ghcr\.io/artemmakolov1/backend-max@sha256:[0-9a-f]{64}$ ]] || { echo "Accepted backend image must be immutable" >&2; exit 1; }
[[ $(docker inspect --format '{{.Config.Image}}' maxposty-backend-backend-1) == "$image" ]] || { echo "Running backend image differs from the accepted image" >&2; exit 1; }
[[ $(docker inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' maxposty-backend-backend-1) == "$source_sha" ]] || { echo "Running backend revision differs from the reviewed source" >&2; exit 1; }
[[ $(docker inspect --format '{{.State.Health.Status}}' maxposty-backend-backend-1) == healthy ]] || { echo "Production backend must be healthy" >&2; exit 1; }

scratch=$(mktemp -d "$installation_dir/backups/.complimentary-access.XXXXXX")
trap 'rm -rf "$scratch"' EXIT
transaction='BEGIN ISOLATION LEVEL SERIALIZABLE;'
[[ "$operation" != inspect ]] || transaction='BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY;'
{
  printf '%s\n' "$transaction"
  cat <<'SQL'
SET LOCAL statement_timeout='15s';
SET LOCAL lock_timeout='2s';
SET LOCAL search_path=pg_catalog,public;
-- Fail within the same transaction unless the reviewed schema is the latest.
SELECT 'schema_verified=' || (1/COUNT(*)::int)
FROM public.schema_migrations
WHERE version='039_account_complimentary_access.sql' AND checksum_sha256=:'schema_checksum'
  AND NOT EXISTS(SELECT 1 FROM public.schema_migrations WHERE version>'039_account_complimentary_access.sql');
-- Keep the account identifier private. A separate statement lets STABLE
-- entitlement helpers observe this transaction's completed grant/revoke.
SELECT user_id,active::text AS active,owned_workspaces::text AS owned_workspaces,changed::text AS changed
FROM public.manage_account_complimentary_access(:'target_email',:'operation',:'operator_name',:'operation_ref') \gset result_
SELECT 'active=' || :'result_active' || E'\nowned_workspaces=' || :'result_owned_workspaces' || E'\nchanged=' || :'result_changed';
SELECT 'owned_entitlements_unlimited=' || CASE WHEN :'result_active'::boolean THEN
  NOT EXISTS (
    SELECT 1 FROM public.workspaces w
    WHERE w.owner_user_id=:'result_user_id' AND w.archived_at IS NULL
      AND (NOT public.workspace_has_complimentary_access(w.id)
        OR public.workspace_entitlement_limit(w.id,'channels') IS NOT NULL
        OR public.workspace_entitlement_limit(w.id,'seats') IS NOT NULL
        OR public.workspace_entitlement_limit(w.id,'storage_bytes') IS NOT NULL)
  ) ELSE FALSE END::text;
COMMIT;
SQL
} >"$scratch/operation.sql"

# The owner connection stays inside the existing database container. Its
# password is read only by the container shell and never enters host argv.
if ! docker exec -i maxposty-backend-postgres-1 sh -ec '
  PGCONNECT_TIMEOUT=10 PGPASSWORD="$POSTGRES_PASSWORD" exec psql -X --no-password \
    --username="$POSTGRES_USER" --dbname="$POSTGRES_DB" --quiet --tuples-only --no-align \
    -v ON_ERROR_STOP=1 -v target_email="$1" -v operation="$2" \
    -v operator_name="$3" -v operation_ref="$4" -v schema_checksum="$5"
' sh "$target_email" "$operation" "github:$github_actor" "github-run:$run_reference" "$schema_checksum" \
  <"$scratch/operation.sql" >"$scratch/result" 2>"$scratch/database.error"; then
  echo "Complimentary access operation failed; private database details withheld" >&2
  exit 1
fi
if ! awk -F= '
  NF!=2 {bad=1;next}
  $1=="schema_verified" && $2=="1" {schema++;next}
  $1=="active" && ($2=="true" || $2=="false") {active++;next}
  $1=="owned_workspaces" && $2 ~ /^[0-9]+$/ && length($2)<=19 {owned++;next}
  $1=="changed" && ($2=="true" || $2=="false") {changed++;next}
  $1=="owned_entitlements_unlimited" && ($2=="true" || $2=="false") {unlimited++;next}
  {bad=1}
  END {exit bad || schema!=1 || active!=1 || owned!=1 || changed!=1 || unlimited!=1}
' "$scratch/result"; then
  echo "Invalid private complimentary access result; inspect before another operation" >&2
  exit 1
fi
active=$(awk -F= '$1=="active" {print $2}' "$scratch/result")
changed=$(awk -F= '$1=="changed" {print $2}' "$scratch/result")
unlimited=$(awk -F= '$1=="owned_entitlements_unlimited" {print $2}' "$scratch/result")
if [[ "$operation" == grant && ( "$active" != true || "$unlimited" != true ) || "$operation" == revoke && ( "$active" != false || "$unlimited" != false ) || "$operation" == inspect && "$changed" != false ]]; then
  echo "Unexpected complimentary access state; inspect before another operation" >&2
  exit 1
fi
printf 'operation=%s\n' "$operation"
cat "$scratch/result"
echo "Complimentary account access verified; roles, subscriptions and payments unchanged"
