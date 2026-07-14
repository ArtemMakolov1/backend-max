#!/usr/bin/env bash
set -euo pipefail

image=${1:-}
registry_user=${2:-}
configure_webhook=${3:-false}
repository=${4:-}
source_sha=${5:-}

if [[ ! "$registry_user" =~ ^[A-Za-z0-9-]+$ ]]; then
  echo "Invalid GHCR user" >&2
  exit 2
fi
if [[ "$configure_webhook" != "true" && "$configure_webhook" != "false" ]]; then
  echo "configure_webhook must be true or false" >&2
  exit 2
fi
if [[ ! "$repository" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]]; then
  echo "Invalid GitHub repository" >&2
  exit 2
fi
if [[ ! "$source_sha" =~ ^[0-9a-f]{40}$ ]]; then
  echo "Invalid source commit" >&2
  exit 2
fi

token=''
IFS= read -r token || [[ -n "$token" ]]
if [[ ${#token} -lt 20 ]]; then
  echo "A GHCR token must be provided on standard input" >&2
  exit 1
fi

root_dir=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
releases_dir=$(dirname -- "$root_dir")
if [[ $(basename -- "$releases_dir") != releases ]]; then
  echo "Deployment bundle must run from a versioned releases directory" >&2
  exit 1
fi
installation_dir=$(dirname -- "$releases_dir")
auth_dir=$(mktemp -d "$root_dir/.docker-auth.XXXXXX")
chmod 700 "$auth_dir"
deployment_complete=false
cleanup() {
  rm -rf "$auth_dir"
  rm -f "${hook_temporary:-}" "${cert_temporary:-}"
  if [[ "$deployment_complete" != "true" ]]; then
    rm -f "$root_dir/.env.production.next" "$root_dir/.release.next"
  fi
}
trap cleanup EXIT

export DOCKER_CONFIG=$auth_dir
curl_config="$auth_dir/github-curl.config"
{
  printf 'header = "Authorization: Bearer %s"\n' "$token"
  printf 'header = "X-GitHub-Api-Version: 2026-03-10"\n'
  printf 'connect-timeout = 20\n'
} >"$curl_config"
chmod 600 "$curl_config"

hook_source="$root_dir/deploy/backup/after-backup-github-release.sh"
cert_source="$root_dir/deploy/backup/recipient-cert.pem"
[[ -x "$hook_source" ]] || { echo "Versioned offsite backup hook is missing" >&2; exit 1; }
openssl x509 -in "$cert_source" -noout -checkend 2592000 >/dev/null 2>&1 || {
  echo "Versioned backup recipient certificate is invalid or expires within 30 days" >&2
  exit 1
}
install -d -m 755 "$installation_dir/hooks"
# This directory is bind-mounted into the rootless application container.
# It contains public certificates only; execute permission is required so the
# container user can traverse it and read the explicitly world-readable PEMs.
install -d -m 755 "$installation_dir/certs"
hook_temporary="$installation_dir/hooks/.after-backup.$$.tmp"
cert_temporary="$installation_dir/certs/.backup-recipient.$$.tmp"
install -m 755 "$hook_source" "$hook_temporary"
install -m 644 "$cert_source" "$cert_temporary"
mv -f "$hook_temporary" "$installation_dir/hooks/after-backup"
mv -f "$cert_temporary" "$installation_dir/certs/backup-recipient.pem"

export MAXPOSTY_BACKUP_CURL_CONFIG=$curl_config
export MAXPOSTY_BACKUP_REPOSITORY=$repository
export MAXPOSTY_BACKUP_SOURCE_SHA=$source_sha
export MAXPOSTY_BACKUP_RECIPIENT_CERT="$installation_dir/certs/backup-recipient.pem"
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
