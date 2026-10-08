#!/usr/bin/env bash
# Inventory and narrowly scoped R2 recovery. Never emit object keys or credentials.
set -euo pipefail
set +x
release_dir=${1:-}
mode=${2:-inventory}
allow_pending=${3:-false}
image=${4:-}
[[ "$mode" == inventory || "$mode" == rollout ]] || { echo "Invalid R2 recovery mode" >&2; exit 2; }
[[ "$allow_pending" == false || "$allow_pending" == true ]] || { echo "Invalid pending-media option" >&2; exit 2; }
[[ "$image" == ghcr.io/artemmakolov1/backend-max@sha256:8140a76d1f755a4f5122bfb64bf6eace468b960135a00a548fe2ebc8967cbf20 ]] || { echo "Unapproved R2 recovery image" >&2; exit 2; }
release_dir=$(CDPATH='' cd -- "$release_dir" && pwd -P)
releases_dir=$(dirname "$release_dir")
installation_dir=$(dirname "$releases_dir")
[[ $(basename "$releases_dir") == releases && $(basename "$release_dir") =~ ^[0-9a-f]{40}$ ]] || { echo "Invalid recovery release directory" >&2; exit 2; }
next_env="$release_dir/.env.production.next"
[[ -f "$next_env" && ! -L "$next_env" ]] || { echo "Fresh production environment is missing" >&2; exit 1; }
"$release_dir/deploy/validate-production-env.sh" "$next_env"
exec 9>"$installation_dir/.deploy.lock"
flock -n 9 || { echo "Another backend deployment is running" >&2; exit 1; }
[[ -L "$installation_dir/current" ]] || { echo "Accepted production release is missing" >&2; exit 1; }
accepted_dir=$(readlink -f "$installation_dir/current")
[[ $(dirname "$accepted_dir") == "$releases_dir" && "$accepted_dir" != "$release_dir" && $(basename "$accepted_dir") =~ ^[0-9a-f]{40}$ ]] || { echo "Invalid accepted release directory" >&2; exit 1; }
accepted_env="$accepted_dir/.env.production"
[[ -f "$accepted_env" && ! -L "$accepted_env" ]] || { echo "Accepted production environment is missing" >&2; exit 1; }
umask 077
scratch=$(mktemp -d "$release_dir/.r2-recovery.XXXXXX")
probe_uploaded=false
cleanup() {
  if [[ "$probe_uploaded" == true ]]; then
    request DELETE "$probe_url" 204 >/dev/null 2>&1 || echo "R2 recovery probe cleanup failed; operator cleanup required" >&2
  fi
  rm -rf "$scratch"
}
trap cleanup EXIT
pg_read() {
  docker exec -i maxposty-backend-postgres-1 sh -ec 'PGPASSWORD="$POSTGRES_PASSWORD" exec psql -X --no-password --username="$POSTGRES_USER" --dbname="$POSTGRES_DB" --quiet --tuples-only --no-align -v ON_ERROR_STOP=1'
}
pg_read >"$scratch/inventory" <<'SQL'
BEGIN TRANSACTION READ ONLY;
SET LOCAL statement_timeout='15s';
SET LOCAL lock_timeout='2s';
WITH refs AS (
 SELECT storage_key AS filename FROM post_attachments
 UNION SELECT image_path FROM posts WHERE image_path<>''
), assets AS (SELECT filename,MAX(size_bytes) AS bytes FROM media_assets GROUP BY filename)
SELECT 'referenced_keys=' || COUNT(*) FROM refs
UNION ALL SELECT 'referenced_bytes=' || COALESCE(SUM(a.bytes),0) FROM refs r LEFT JOIN assets a USING(filename)
UNION ALL SELECT 'referenced_missing_metadata=' || COUNT(*) FROM refs r LEFT JOIN assets a USING(filename) WHERE a.filename IS NULL
UNION ALL SELECT 'invalid_reference_keys=' || COUNT(*) FROM refs WHERE filename !~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,254}$'
UNION ALL SELECT 'asset_keys=' || COUNT(*) FROM assets
UNION ALL SELECT 'asset_bytes=' || COALESCE(SUM(bytes),0) FROM assets
UNION ALL SELECT 'pending_asset_rows=' || COUNT(*) FROM media_assets WHERE state='pending'
UNION ALL SELECT 'queued_gc_keys=' || COUNT(*) FROM media_gc_queue
UNION ALL SELECT 'cached_max_attachment_tokens=' || COUNT(*) FROM post_attachments WHERE provider_token<>''
UNION ALL SELECT 'published_max_posts=' || COUNT(*) FROM posts WHERE status='published' AND max_message_id<>''
UNION ALL SELECT 'max_history_remote_attachments=' || COUNT(*) FROM max_history_post_attachments WHERE remote_url<>'';
SELECT 'eligible_media_assets_24h=' || COUNT(*) FROM media_assets ma WHERE ma.updated_at < CURRENT_TIMESTAMP-INTERVAL '24 hours' AND (
 ma.state='pending' OR (
  NOT EXISTS (SELECT 1 FROM posts p WHERE p.workspace_id=ma.workspace_id AND p.image_path=ma.filename)
  AND NOT EXISTS (SELECT 1 FROM post_attachments pa WHERE pa.workspace_id=ma.workspace_id AND pa.storage_key=ma.filename)
 )
);
SELECT 'eligible_media_assets_720h=' || COUNT(*) FROM media_assets ma WHERE ma.updated_at < CURRENT_TIMESTAMP-INTERVAL '720 hours' AND (
 ma.state='pending' OR (
  NOT EXISTS (SELECT 1 FROM posts p WHERE p.workspace_id=ma.workspace_id AND p.image_path=ma.filename)
  AND NOT EXISTS (SELECT 1 FROM post_attachments pa WHERE pa.workspace_id=ma.workspace_id AND pa.storage_key=ma.filename)
 )
);
SELECT 'queued_gc_720h=' || COUNT(*) FROM media_gc_queue WHERE orphaned_at < CURRENT_TIMESTAMP-INTERVAL '720 hours';
ROLLBACK;
SQL
if ! awk -F= 'NF!=2 || $1 !~ /^[a-z][a-z0-9_]+$/ || $2 !~ /^[0-9]+$/ { bad=1 } END { exit bad }' "$scratch/inventory"; then
  echo "Invalid aggregate media inventory" >&2; exit 1
fi
echo "Read-only production media inventory:"
cat "$scratch/inventory"
metric() { awk -F= -v key="$1" '$1==key {print $2; exit}' "$scratch/inventory"; }
for key in referenced_keys referenced_bytes referenced_missing_metadata invalid_reference_keys asset_keys asset_bytes pending_asset_rows queued_gc_keys cached_max_attachment_tokens published_max_posts max_history_remote_attachments eligible_media_assets_24h eligible_media_assets_720h queued_gc_720h; do
  [[ $(awk -F= -v key="$key" '$1==key {n++} END {print n+0}' "$scratch/inventory") == 1 ]] || { echo "Incomplete aggregate media inventory" >&2; exit 1; }
done
[[ $(metric invalid_reference_keys) == 0 ]] || { echo "Cannot safely inspect legacy volume for invalid reference keys" >&2; exit 1; }
pg_read >"$scratch/referenced-keys" <<'SQL'
BEGIN TRANSACTION READ ONLY;
SET LOCAL statement_timeout='15s';
SELECT storage_key FROM post_attachments UNION SELECT image_path FROM posts WHERE image_path<>'';
ROLLBACK;
SQL
volume=maxposty-backend_media-data
if docker volume inspect "$volume" >/dev/null 2>&1; then
  docker run --rm --pull never -i --network none --read-only --cap-drop ALL --security-opt no-new-privileges --tmpfs /tmp:rw,noexec,nosuid,size=2m,mode=1777 --mount "type=volume,src=$volume,dst=/legacy,readonly" --entrypoint /bin/sh "$image" -c '
    set -eu
    if ! find /legacy -type f -print0 > /tmp/files 2>/dev/null; then echo "Legacy volume metadata inventory failed" >&2; exit 1; fi
    : > /tmp/sizes
    while IFS= read -r -d "" file; do
      if ! stat -c %s "$file" >> /tmp/sizes 2>/dev/null; then echo "Legacy volume metadata inventory failed" >&2; exit 1; fi
    done < /tmp/files
    total=$(wc -l < /tmp/sizes)
    bytes=$(awk "{ n+=\$1 } END { printf \"%.0f\", n }" /tmp/sizes)
    found=0; missing=0
    while IFS= read -r key; do
      case "$key" in ""|*[!A-Za-z0-9._-]*|.|..) echo "Invalid private key inventory" >&2; exit 1;; esac
      if [ -f "/legacy/$key" ]; then found=$((found+1)); else missing=$((missing+1)); fi
    done
    printf "legacy_volume_files=%s\nlegacy_volume_bytes=%s\nreferenced_local_present=%s\nreferenced_local_missing=%s\n" "$total" "$bytes" "$found" "$missing"
  ' <"$scratch/referenced-keys"
else
  echo "legacy_volume_available=false"
fi
if [[ "$mode" == inventory ]]; then
  echo "Inventory only: production configuration, database and storage objects unchanged"
  exit 0
fi
if [[ $(metric asset_keys) != 0 || $(metric referenced_keys) != 0 || $(metric queued_gc_keys) != 0 ]]; then
  [[ "$allow_pending" == true ]] || { echo "Existing media requires explicit allow_pending_media before R2 cutover; metadata remains untouched" >&2; exit 1; }
  echo "Existing media recovery remains pending; this helper does not rewrite database references"
fi
env_value() { awk -F= -v key="$2" '$1==key {sub(/^[^=]*=/,"");print;exit}' "$1"; }
endpoint=$(env_value "$next_env" S3_HOST); endpoint=${endpoint%/}
bucket=$(env_value "$next_env" S3_BUCKET)
region=$(env_value "$next_env" S3_REGION)
[[ "$endpoint" == https://97b27ab0a14bfe63909b26f167e99999.r2.cloudflarestorage.com && "$bucket" == maxposty-media-production && "$region" == auto ]] || { echo "R2 recovery requires the approved private bucket, exact account endpoint and auto region" >&2; exit 1; }
echo "Recovery storage provider: Cloudflare R2; private bucket and auto region verified"
mkdir -p "$installation_dir/backups"
backup=$(mktemp "$installation_dir/backups/accepted-env-before-r2.XXXXXX")
cp "$accepted_env" "$backup"; chmod 600 "$backup"
echo "Accepted production environment preserved in a private recovery backup"
config="$scratch/curl.conf"
printf 'user = "%s:%s"\naws-sigv4 = "aws:amz:auto:s3"\n' "$(env_value "$next_env" S3_ACCESS_KEY)" "$(env_value "$next_env" S3_SECRET_KEY)" >"$config"
request() {
  local method=$1 target=$2 expected=$3 digest=${4:-e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855} status
  printf 'url = "%s"\nheader = "x-amz-content-sha256: %s"\n' "$target" "$digest" >"$scratch/request.conf"
  local args=(--silent --show-error --proto '=https' --tlsv1.2 --connect-timeout 10 --max-time 60 --max-redirs 0 --config "$config" --config "$scratch/request.conf" --output "$scratch/response" --write-out '%{http_code}')
  case "$method" in HEAD) args+=(--head);; PUT) args+=(--upload-file "$scratch/payload");; DELETE) args+=(--request DELETE);; esac
  if ! status=$(curl "${args[@]}" 2>"$scratch/curl.error"); then echo "R2 recovery request failed; private response withheld" >&2; return 1; fi
  [[ "$status" == "$expected" ]] || { echo "R2 recovery $method failed (HTTP $status; private response withheld)" >&2; return 1; }
}
request HEAD "$endpoint/$bucket" 200
probe_key="maxposty-recovery-probe-$(openssl rand -hex 16).bin"
probe_url="$endpoint/$bucket/$probe_key"
request HEAD "$probe_url" 404
head -c 1048576 /dev/urandom >"$scratch/payload"
digest=$(sha256sum "$scratch/payload" | awk '{print $1}')
probe_uploaded=true
request PUT "$probe_url" 200 "$digest"
request GET "$probe_url" 200
cmp "$scratch/payload" "$scratch/response" >/dev/null || { echo "R2 recovery probe bytes did not match" >&2; exit 1; }
request DELETE "$probe_url" 204
probe_uploaded=false
request HEAD "$probe_url" 404
echo "VPS R2 1MiB probe passed: upload, identical download, delete and HEAD404"
echo "Fresh R2 configuration ready for the verified image roll-forward"
