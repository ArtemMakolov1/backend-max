#!/usr/bin/env bash
# Restore only the last accepted S3 settings; never source or log environment files.
set -euo pipefail
set +x

release_dir=${1:-}
[[ -d "$release_dir" ]] || { echo "Recovery release directory is missing" >&2; exit 2; }
release_dir=$(CDPATH='' cd -- "$release_dir" && pwd -P)
releases_dir=$(dirname "$release_dir")
installation_dir=$(dirname "$releases_dir")
[[ $(basename "$releases_dir") == releases && $(basename "$release_dir") =~ ^[0-9a-f]{40}$ ]] || {
  echo "Recovery requires a versioned release directory" >&2; exit 2;
}
for command_name in awk curl flock mktemp readlink; do
  command -v "$command_name" >/dev/null || { echo "Recovery command is missing: $command_name" >&2; exit 1; }
done
curl --help all | grep -F -- '--aws-sigv4' >/dev/null || { echo "Recovery requires curl with AWS SigV4 support" >&2; exit 1; }
exec 9>"$installation_dir/.deploy.lock"
flock -n 9 || { echo "Another backend deployment is running" >&2; exit 1; }
[[ -L "$installation_dir/current" ]] || { echo "No accepted current release exists" >&2; exit 1; }
accepted_dir=$(readlink -f "$installation_dir/current")
[[ $(dirname "$accepted_dir") == "$releases_dir" && $(basename "$accepted_dir") =~ ^[0-9a-f]{40}$ && "$accepted_dir" != "$release_dir" ]] || {
  echo "Accepted release path is invalid or is the recovery target" >&2; exit 1;
}
accepted_env="$accepted_dir/.env.production"
next_env="$release_dir/.env.production.next"
[[ -f "$accepted_env" && ! -L "$accepted_env" && -f "$next_env" && ! -L "$next_env" ]] || {
  echo "Accepted and fresh production environment files are required" >&2; exit 1;
}
env_value() {
  awk -F= -v key="$2" '$1 == key { sub(/^[^=]*=/, ""); print; exit }' "$1"
}
[[ $(env_value "$accepted_env" AUTH_BOOTSTRAP_MODE) == false ]] || { echo "Accepted environment must be production" >&2; exit 1; }
for key in S3_HOST S3_ACCESS_KEY S3_SECRET_KEY S3_BUCKET S3_REGION; do
  count=$(awk -F= -v key="$key" '$1 == key { n++ } END { print n+0 }' "$accepted_env")
  [[ "$count" -le 1 ]] || { echo "Accepted environment contains duplicate S3 settings" >&2; exit 1; }
  case "$key" in
    S3_HOST|S3_ACCESS_KEY|S3_SECRET_KEY)
      [[ "$count" == 1 && -n $(env_value "$accepted_env" "$key") ]] || { echo "Accepted S3 credentials are incomplete" >&2; exit 1; } ;;
  esac
done
umask 077
temporary=$(mktemp "$release_dir/.s3-recovery-env.XXXXXX")
probe_dir=$(mktemp -d "$release_dir/.s3-recovery-probe.XXXXXX")
trap 'rm -f "$temporary"; rm -rf "$probe_dir"' EXIT
awk '
  BEGIN { split("S3_HOST S3_ACCESS_KEY S3_SECRET_KEY S3_BUCKET S3_REGION", keys, " "); for (i in keys) restore[keys[i]]=1 }
  FNR==NR { p=index($0,"="); k=substr($0,1,p-1); if (k in restore) saved[k]=substr($0,p+1); next }
  { p=index($0,"="); k=substr($0,1,p-1); if (k in restore) print k "=" saved[k]; else print }
' "$accepted_env" "$next_env" >"$temporary"
chmod 600 "$temporary"
"$release_dir/deploy/validate-production-env.sh" "$temporary"
endpoint=$(env_value "$temporary" S3_HOST)
endpoint=${endpoint%/}
[[ "$endpoint" == https://* ]] || endpoint="https://$endpoint"
region=$(env_value "$temporary" S3_REGION)
host=${endpoint#https://}; host=${host%%:*}
if [[ "$host" =~ ^[0-9.]+$ || "$host" != *.* ]]; then
  echo "Accepted S3 provider: private/custom endpoint"
else
  provider_suffix=$(awk -F. '{ print $(NF-1) "." $NF }' <<<"$host")
  echo "Accepted S3 provider DNS suffix: $provider_suffix"
fi
if [[ -z "$region" ]]; then
  if [[ "$host" =~ ^s3-([a-z0-9-]+)\.hostkey\.com$ ]]; then region=${BASH_REMATCH[1]}; else region=us-east-1; fi
fi
bucket=$(env_value "$temporary" S3_BUCKET)
access_key=$(env_value "$temporary" S3_ACCESS_KEY)
secret_key=$(env_value "$temporary" S3_SECRET_KEY)
config="$probe_dir/curl.conf"
# validate-production-env permits only characters safe in these quoted config fields.
{
  printf 'user = "%s:%s"\n' "$access_key" "$secret_key"
  printf 'aws-sigv4 = "aws:amz:%s:s3"\n' "$region"
  printf 'header = "x-amz-content-sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"\n'
} >"$config"
unset access_key secret_key
request() {
  local method=$1 target=$2 status
  printf 'url = "%s"\n' "$target" >"$probe_dir/url.conf"
  local args=(--silent --show-error --proto '=https' --tlsv1.2 --connect-timeout 10 --max-time 30 --max-redirs 0 --config "$config" --config "$probe_dir/url.conf" --output "$probe_dir/response" --write-out '%{http_code}')
  [[ "$method" != HEAD ]] || args+=(--head)
  if ! status=$(curl "${args[@]}" 2>"$probe_dir/curl.error"); then
    echo "Accepted S3 settings failed the read-only connectivity check" >&2; return 1
  fi
  [[ "$status" == 200 ]] || { echo "Accepted S3 settings failed read-only $method preflight (HTTP $status)" >&2; return 1; }
}
if [[ -z "$bucket" ]]; then
  request GET "$endpoint/"
  # S3 ListBuckets has one Name element inside each Bucket; reject ambiguous results.
  mapfile -t buckets < <(awk 'BEGIN { RS="</Bucket>" } /<Bucket>/ && /<Name>[^<]+<\/Name>/ { part=$0; sub(/^.*<Bucket>/,"",part); match(part, /<Name>[^<]+<\/Name>/); name=substr(part,RSTART+6,RLENGTH-13); print name }' "$probe_dir/response")
  [[ ${#buckets[@]} == 1 ]] || { echo "Accepted S3 settings require exactly one accessible bucket when S3_BUCKET is empty" >&2; exit 1; }
  bucket=${buckets[0]}
  [[ "$bucket" =~ ^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$ && "$bucket" != *..* && "$bucket" != *.-* && "$bucket" != *-.* && ! "$bucket" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]] || {
    echo "S3 returned an invalid bucket name" >&2; exit 1;
  }
fi
request HEAD "$endpoint/$bucket"
mv -f "$temporary" "$next_env"
echo "Accepted S3 settings restored; read-only S3 preflight passed"
