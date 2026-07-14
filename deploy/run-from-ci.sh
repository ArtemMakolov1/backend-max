#!/usr/bin/env bash
set -euo pipefail

image=${1:-}
registry_user=${2:-}
configure_webhook=${3:-false}

if [[ ! "$registry_user" =~ ^[A-Za-z0-9-]+$ ]]; then
  echo "Invalid GHCR user" >&2
  exit 2
fi
if [[ "$configure_webhook" != "true" && "$configure_webhook" != "false" ]]; then
  echo "configure_webhook must be true or false" >&2
  exit 2
fi

token=''
IFS= read -r token || [[ -n "$token" ]]
if [[ ${#token} -lt 20 ]]; then
  echo "A GHCR token must be provided on standard input" >&2
  exit 1
fi

root_dir=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
auth_dir=$(mktemp -d "$root_dir/.docker-auth.XXXXXX")
chmod 700 "$auth_dir"
deployment_complete=false
cleanup() {
  rm -rf "$auth_dir"
  if [[ "$deployment_complete" != "true" ]]; then
    rm -f "$root_dir/.env.production.next" "$root_dir/.release.next"
  fi
}
trap cleanup EXIT

export DOCKER_CONFIG=$auth_dir
printf '%s' "$token" | docker login ghcr.io --username "$registry_user" --password-stdin >/dev/null
unset token

"$root_dir/deploy/deploy-production.sh" "$image"
deployment_complete=true

if [[ "$configure_webhook" == "true" ]]; then
  bootstrap_mode=$(awk -F= '$1 == "AUTH_BOOTSTRAP_MODE" { print $2; exit }' "$root_dir/.env.production")
  if [[ "$bootstrap_mode" == "true" ]]; then
    echo "MAX webhook cannot be configured during the fail-closed HTTP bootstrap stage" >&2
    exit 1
  fi
  docker compose \
    --project-name maxposty-backend \
    --env-file "$root_dir/.env.production" \
    --env-file "$root_dir/.release" \
    --file "$root_dir/deploy/compose.production.yaml" \
    --profile ops run --rm --no-deps setup-max-webhook
fi
