#!/usr/bin/env bash
set -euo pipefail
: "${COMPLIMENTARY_TEST_DIR:?test directory required}"
: "${COMPLIMENTARY_TEST_SOURCE:?test source required}"
: "${COMPLIMENTARY_TEST_IMAGE:?test image required}"
case "${1:-}" in
  inspect)
    case "$*" in
      *'.Config.Image'*)
        [[ ${COMPLIMENTARY_TEST_CASE:-} != wrong-image ]] || { echo 'ghcr.io/artemmakolov1/backend-max@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'; exit; }
        printf '%s\n' "$COMPLIMENTARY_TEST_IMAGE" ;;
      *'org.opencontainers.image.revision'*)
        [[ ${COMPLIMENTARY_TEST_CASE:-} != wrong-source ]] || { echo 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'; exit; }
        printf '%s\n' "$COMPLIMENTARY_TEST_SOURCE" ;;
      *'.State.Health.Status'*)
        [[ ${COMPLIMENTARY_TEST_CASE:-} != unhealthy ]] || { echo unhealthy; exit; }
        echo healthy ;;
      *) echo 'Unexpected container inspection' >&2; exit 64;;
    esac ;;
  exec)
    [[ ${2:-} == -i && ${3:-} == maxposty-backend-postgres-1 ]] || exit 64
    printf 'called\n' >"$COMPLIMENTARY_TEST_DIR/database-called"
    cat >"$COMPLIMENTARY_TEST_DIR/query.sql"
    case ${COMPLIMENTARY_TEST_CASE:-} in
      database-error)
        echo 'private SQL error for fixture.owner@example.test; password=synthetic-private-marker' >&2
        exit 1 ;;
      unexpected-output)
        printf 'schema_verified=1\nactive=true\nowned_workspaces=2\nchanged=true\nowned_entitlements_unlimited=true\nuser_id=private-fixture-id\n'
        exit ;;
    esac
    mode=''
    for argument in "$@"; do
      case "$argument" in inspect|grant|revoke) mode=$argument;; esac
    done
    case "$mode" in
      inspect) active=false; changed=false;;
      grant) active=true; changed=true;;
      revoke) active=false; changed=true;;
      *) echo 'Unexpected owner operation' >&2; exit 64;;
    esac
    unlimited=$active
    [[ ${COMPLIMENTARY_TEST_CASE:-} != finite-entitlements ]] || unlimited=false
    printf 'schema_verified=1\nactive=%s\nowned_workspaces=2\nchanged=%s\nowned_entitlements_unlimited=%s\n' "$active" "$changed" "$unlimited" ;;
  *) echo 'Unexpected fake Docker command' >&2; exit 64;;
esac
