#!/usr/bin/env bash
set -euo pipefail

migration_dir=${1:-internal/store/migrations}
if [[ ! -d "$migration_dir" ]]; then
  echo "Migration directory does not exist: $migration_dir" >&2
  exit 2
fi
if ! command -v perl >/dev/null 2>&1; then
  echo "perl is required to validate SQL migration compatibility" >&2
  exit 1
fi

shopt -s nullglob nocasematch
migration_files=("$migration_dir"/*.sql)
if ((${#migration_files[@]} == 0)); then
  echo "No SQL migrations found in $migration_dir" >&2
  exit 1
fi

reject_statement() {
  local file=$1
  local statement=$2
  local summary=${statement:0:240}
  echo "Migration is not safe for the automatic expand phase: $file" >&2
  echo "Rejected statement: $summary" >&2
  echo "Use an additive migration first; run destructive contract changes only after old releases are drained." >&2
  exit 1
}

for file in "${migration_files[@]}"; do
  while IFS= read -r statement || [[ -n "$statement" ]]; do
    statement=${statement#"${statement%%[![:space:]]*}"}
    statement=${statement%"${statement##*[![:space:]]}"}
    [[ -n "$statement" ]] || continue

    if [[ "$statement" =~ ^DROP[[:space:]]+(TABLE|SCHEMA|TYPE|VIEW|MATERIALIZED[[:space:]]+VIEW|INDEX|SEQUENCE) ]] ||
      [[ "$statement" =~ ^TRUNCATE([[:space:]]|$) ]] ||
      [[ "$statement" =~ ^DELETE[[:space:]]+FROM([[:space:]]|$) ]] ||
      [[ "$statement" =~ ^REVOKE([[:space:]]|$) ]] ||
      [[ "$statement" =~ ^CREATE[[:space:]]+OR[[:space:]]+REPLACE[[:space:]]+(VIEW|FUNCTION|PROCEDURE) ]]; then
      reject_statement "$file" "$statement"
    fi

    if [[ "$statement" =~ ^ALTER[[:space:]]+TABLE ]] &&
      { [[ "$statement" =~ [[:space:]]DROP([[:space:]]|$) ]] ||
        [[ "$statement" =~ [[:space:]]RENAME([[:space:]]|$) ]] ||
        [[ "$statement" =~ ALTER[[:space:]]+COLUMN.*[[:space:]]TYPE([[:space:]]|$) ]] ||
        [[ "$statement" =~ ALTER[[:space:]]+COLUMN.*SET[[:space:]]+NOT[[:space:]]+NULL ]] ||
        [[ "$statement" =~ ADD[[:space:]]+COLUMN.*NOT[[:space:]]+NULL ]] ||
        [[ "$statement" =~ (ENABLE|DISABLE|FORCE)[[:space:]]+ROW[[:space:]]+LEVEL[[:space:]]+SECURITY ]]; }; then
      reject_statement "$file" "$statement"
    fi

    if [[ "$statement" =~ ^ALTER[[:space:]]+TYPE.*[[:space:]]RENAME([[:space:]]|$) ]]; then
      reject_statement "$file" "$statement"
    fi
  done < <(perl -0pe 's{/\*.*?\*/}{}gs; s/--[^\r\n]*//g; s/[[:space:]]+/ /g; s/;/;\n/g' "$file")
done

echo "Expand-contract migration guard passed for ${#migration_files[@]} file(s)."
