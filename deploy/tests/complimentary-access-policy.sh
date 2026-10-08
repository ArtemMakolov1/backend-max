#!/usr/bin/env bash
set -euo pipefail
repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
fixture=$(mktemp -d)
trap 'rm -rf "$fixture"' EXIT
source_sha=0123456789abcdef0123456789abcdef01234567
image=ghcr.io/artemmakolov1/backend-max@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
schema_sha=cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
installation="$fixture/backend"
mkdir -p "$fixture/bin" "$installation/releases/$source_sha" "$installation/backups"
ln -s "$repo_root/deploy/tests/fake-complimentary-docker.sh" "$fixture/bin/docker"
ln -s "$installation/releases/$source_sha" "$installation/current"
printf 'BACKEND_IMAGE=%s\n' "$image" >"$installation/releases/$source_sha/.release"
printf 'PRIVATE_FIXTURE_MARKER=synthetic-private-marker\n' >"$installation/releases/$source_sha/.env.production"
export PATH="$fixture/bin:$PATH"
export COMPLIMENTARY_TEST_DIR="$fixture" COMPLIMENTARY_TEST_SOURCE="$source_sha" COMPLIMENTARY_TEST_IMAGE="$image"
script="$repo_root/deploy/manage-complimentary-access.sh"
workflow="$repo_root/.github/workflows/manage-complimentary-access.yml"
call() { "$script" "$1" "$source_sha" fixture.owner@example.test AuditOperator 123456-1 "$installation" "$schema_sha"; }
fail_without_database() {
  rm -f "$fixture/database-called"
  if call "$1" >"$fixture/result" 2>&1; then echo "Unsafe operation unexpectedly succeeded" >&2; exit 1; fi
  [[ ! -f "$fixture/database-called" ]] || { echo "Failed preflight reached the database" >&2; exit 1; }
}
for mode in inspect grant revoke; do
  COMPLIMENTARY_TEST_CASE=success call "$mode" >"$fixture/result"
  grep -Fxq "operation=$mode" "$fixture/result"
  grep -Fxq 'schema_verified=1' "$fixture/result"
  grep -Fxq 'owned_workspaces=2' "$fixture/result"
  grep -Fq "public.manage_account_complimentary_access(:'target_email',:'operation',:'operator_name',:'operation_ref')" "$fixture/query.sql"
  grep -Fq "version='039_account_complimentary_access.sql' AND checksum_sha256=:'schema_checksum'" "$fixture/query.sql"
  grep -Fq "version='042_content_analysis_cache.sql'" "$fixture/query.sql"
  grep -Fq "version>'042_content_analysis_cache.sql'" "$fixture/query.sql"
  grep -Fq "\\gset result_" "$fixture/query.sql"
  grep -Fq "w.owner_user_id=:'result_user_id' AND w.archived_at IS NULL" "$fixture/query.sql"
  for metric in channels seats storage_bytes; do
    grep -Fq "public.workspace_entitlement_limit(w.id,'$metric') IS NOT NULL" "$fixture/query.sql"
  done
  if [[ "$mode" == grant ]]; then
    grep -Fxq 'owned_entitlements_unlimited=true' "$fixture/result"
  else
    grep -Fxq 'owned_entitlements_unlimited=false' "$fixture/result"
  fi
  if [[ "$mode" == inspect ]]; then
    grep -Fxq 'BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY;' "$fixture/query.sql"
    grep -Fxq 'changed=false' "$fixture/result"
  else
    grep -Fxq 'BEGIN ISOLATION LEVEL SERIALIZABLE;' "$fixture/query.sql"
  fi
  if grep -Eq 'fixture.owner|synthetic-private-marker|private-fixture-id|billing_subscription|workspace_members' "$fixture/result" "$fixture/query.sql"; then
    echo "Operational output/SQL exposed private data or touched billing/roles" >&2; exit 1
  fi
done
for scenario in wrong-image wrong-source unhealthy; do
  export COMPLIMENTARY_TEST_CASE=$scenario
  fail_without_database grant
done
unset COMPLIMENTARY_TEST_CASE
exec 8>"$installation/.deploy.lock"
flock -n 8
fail_without_database grant
flock -u 8
exec 8>&-
for mode in grant revoke; do
  for scenario in database-error unexpected-output; do
    COMPLIMENTARY_TEST_CASE=$scenario
    export COMPLIMENTARY_TEST_CASE
    if call "$mode" >"$fixture/result" 2>&1; then echo "Unsafe private database result accepted" >&2; exit 1; fi
    if grep -Eq 'fixture.owner|synthetic-private-marker|private-fixture-id' "$fixture/result"; then
      echo "Private database details leaked" >&2; exit 1
    fi
  done
done
unset COMPLIMENTARY_TEST_CASE
if COMPLIMENTARY_TEST_CASE=finite-entitlements call grant >"$fixture/result" 2>&1; then
  echo "Grant with finite owned entitlements was reported as successful" >&2; exit 1
fi
rm -f "$fixture/database-called"
if "$script" grant "$source_sha" "fixture.owner@example.test';DELETE" AuditOperator 123456-1 "$installation" "$schema_sha" >"$fixture/result" 2>&1; then echo "Unsafe email accepted" >&2; exit 1; fi
[[ ! -f "$fixture/database-called" ]]
if "$script" grant "$source_sha" fixture.owner@example.test 'unsafe;operator' 123456-1 "$installation" "$schema_sha" >"$fixture/result" 2>&1; then echo "Unsafe operator accepted" >&2; exit 1; fi
[[ ! -f "$fixture/database-called" ]]
mv "$installation/releases/$source_sha/.env.production" "$fixture/private-env"
ln -s "$fixture/private-env" "$installation/releases/$source_sha/.env.production"
fail_without_database grant
grep -Fq "github.event_name == 'workflow_dispatch' && github.ref == 'refs/heads/main' && github.repository == 'ArtemMakolov1/backend-max'" "$workflow"
grep -Fxq '      name: production' "$workflow"
grep -Fq 'TARGET_EMAIL: ${{ secrets.COMPLIMENTARY_ACCESS_ACCOUNT_EMAIL }}' "$workflow"
if grep -Fq 'vars.COMPLIMENTARY_ACCESS_ACCOUNT_EMAIL' "$workflow"; then
  echo "Approved account identity would be exposed in runner environment logs" >&2; exit 1
fi
grep -Fxq '        default: inspect' "$workflow"
grep -Fxq '  group: maxposty-backend-production' "$workflow"
grep -Fxq '  cancel-in-progress: false' "$workflow"
grep -Fq './deploy/verify-release-gates.sh "$GITHUB_REPOSITORY" "$SOURCE_SHA" 1 0' "$workflow"
grep -Fq 'StrictHostKeyChecking=yes' "$workflow"
grep -Fq 'UserKnownHostsFile="$HOME/.ssh/known_hosts"' "$workflow"
grep -Fq 'if: ${{ always() }}' "$workflow"
if grep -Eq 'packages: write|contents: write|workflow_run:|password.*inputs|email.*inputs|target_email:' "$workflow"; then
  echo "Owner grant workflow accepts an arbitrary target or excess privileges" >&2; exit 1
fi
if bash "$repo_root/deploy/tests/complimentary-access-postgres-policy.sh" maxposty-backend-postgres-1 >"$fixture/result" 2>&1; then
  echo "Real PostgreSQL regression accepted a production container" >&2; exit 1
fi
if GITHUB_ACTIONS=true TEST_DATABASE_URL='postgresql://invalid-fixture' bash "$repo_root/deploy/tests/complimentary-access-postgres-policy.sh" --ci-service aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa >"$fixture/result" 2>&1; then
  echo "Real PostgreSQL regression accepted a non-synthetic service DSN" >&2; exit 1
fi
echo "Complimentary access operational policy tests passed."
