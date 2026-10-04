#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
validator="$repo_root/deploy/validate-expand-contract-migrations.sh"
reviewed="$repo_root/internal/store/migrations/039_account_complimentary_access.sql"
scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT

expect_accept() {
  local name=$1
  if ! bash "$validator" "$scratch/$name" >"$scratch/result" 2>&1; then
    echo "Expected compatible migration case: $name" >&2
    cat "$scratch/result" >&2
    exit 1
  fi
}

expect_reject() {
  local name=$1
  if bash "$validator" "$scratch/$name" >"$scratch/result" 2>&1; then
    echo "Unexpectedly accepted incompatible migration case: $name" >&2
    exit 1
  fi
  if ! grep -q 'Migration is not safe' "$scratch/result"; then
    echo "Migration case failed outside the compatibility guard: $name" >&2
    cat "$scratch/result" >&2
    exit 1
  fi
}

mkdir "$scratch/frozen" "$scratch/tampered" "$scratch/renamed" "$scratch/old-revoke" \
  "$scratch/other-replacement" "$scratch/spoofed" "$scratch/case-renamed" "$scratch/additive" "$scratch/old-drop"
cp "$reviewed" "$scratch/frozen/039_account_complimentary_access.sql"
expect_accept frozen

cp "$reviewed" "$scratch/tampered/039_account_complimentary_access.sql"
printf '\n-- Even a comment changes the reviewed complete-body checksum.\n' >>"$scratch/tampered/039_account_complimentary_access.sql"
expect_reject tampered

cp "$reviewed" "$scratch/renamed/040_account_complimentary_access.sql"
expect_reject renamed
cp "$reviewed" "$scratch/case-renamed/039_ACCOUNT_COMPLIMENTARY_ACCESS.sql"
expect_reject case-renamed

printf '%s\n' 'REVOKE SELECT ON users FROM PUBLIC;' >"$scratch/old-revoke/040_permissions.sql"
expect_reject old-revoke
printf '%s\n' 'CREATE OR REPLACE FUNCTION existing_feature() RETURNS boolean LANGUAGE sql AS $$ SELECT true; $$;' \
  >"$scratch/other-replacement/040_functions.sql"
expect_reject other-replacement
printf '%s\n' 'REVOKE ALL ON users FROM PUBLIC;' >"$scratch/spoofed/039_account_complimentary_access.sql"
expect_reject spoofed
printf '%s\n' 'DROP TABLE users;' >"$scratch/old-drop/040_drop.sql"
expect_reject old-drop

printf '%s\n' 'CREATE TABLE new_additive_feature(id TEXT PRIMARY KEY);' \
  'CREATE FUNCTION new_additive_helper() RETURNS boolean LANGUAGE sql AS $$ SELECT true; $$;' \
  >"$scratch/additive/040_feature.sql"
expect_accept additive

echo 'Expand-contract migration policy tests passed (reviewed SHA only; destructive/tampered migrations rejected).'
