#!/usr/bin/env bash
set -euo pipefail

# Production-only synchronous offsite backup hook. The three validated local
# snapshots are packed without an intermediate plaintext archive, encrypted for
# the offline recovery key, uploaded to a draft GitHub Release, downloaded back
# and hash-checked, then made immutable. Any failure happens before migrations.

dump=${1:-}
media=${2:-}
manifest=${3:-}
image=${4:-}

curl_config=${MAXPOSTY_BACKUP_CURL_CONFIG:-}
repository=${MAXPOSTY_BACKUP_REPOSITORY:-}
source_sha=${MAXPOSTY_BACKUP_SOURCE_SHA:-}
snapshot_image=${MAXPOSTY_BACKUP_SNAPSHOT_IMAGE:-$image}
snapshot_source_sha=${MAXPOSTY_BACKUP_SNAPSHOT_SOURCE_SHA:-$source_sha}
recipient_cert=${MAXPOSTY_BACKUP_RECIPIENT_CERT:-/opt/maxposty/backend/certs/backup-recipient.pem}
api_url=${MAXPOSTY_BACKUP_API_URL:-https://api.github.com}
upload_url=${MAXPOSTY_BACKUP_UPLOAD_URL:-https://uploads.github.com}
max_plaintext_bytes=${MAXPOSTY_BACKUP_MAX_PLAINTEXT_BYTES:-536870912}

fail() {
  echo "Offsite backup failed: $*" >&2
  exit 1
}

for command in curl id jq openssl sha256sum stat tar readlink sleep; do
  command -v "$command" >/dev/null 2>&1 || fail "required command is missing: $command"
done

[[ "$repository" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || fail "invalid GitHub repository"
[[ "$source_sha" =~ ^[0-9a-f]{40}$ ]] || fail "invalid source commit"
[[ "$snapshot_source_sha" =~ ^[0-9a-f]{40}$ ]] || fail "invalid snapshot source commit"
[[ "$image" =~ ^ghcr\.io/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+@sha256:[0-9a-f]{64}$ ]] || fail "release image is not an immutable GHCR digest"
[[ "$snapshot_image" =~ ^ghcr\.io/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+@sha256:[0-9a-f]{64}$ ]] || fail "snapshot image is not an immutable GHCR digest"
[[ "$api_url" =~ ^https://[^[:space:]]+$ || "$api_url" =~ ^http://127\.0\.0\.1:[0-9]+$ ]] || fail "invalid GitHub API URL"
[[ "$upload_url" =~ ^https://[^[:space:]]+$ || "$upload_url" =~ ^http://127\.0\.0\.1:[0-9]+$ ]] || fail "invalid GitHub upload URL"
[[ "$max_plaintext_bytes" =~ ^[1-9][0-9]*$ ]] || fail "invalid backup size limit"

[[ -f "$curl_config" && ! -L "$curl_config" ]] || fail "temporary GitHub credentials are unavailable"
[[ $(stat -c '%a' "$curl_config") == 600 ]] || fail "temporary GitHub credentials must have mode 0600"
[[ $(stat -c '%u' "$curl_config") == "$(id -u)" ]] || fail "temporary GitHub credentials have an unexpected owner"
[[ -f "$recipient_cert" && ! -L "$recipient_cert" ]] || fail "backup recipient certificate is unavailable"
openssl x509 -in "$recipient_cert" -noout -checkend 2592000 >/dev/null 2>&1 || fail "backup recipient certificate is invalid or expires within 30 days"

backup_dir=''
timestamp=''
for file in "$dump" "$media" "$manifest"; do
  [[ "$file" == /* && -f "$file" && ! -L "$file" ]] || fail "backup input is not a regular absolute file"
  [[ $(stat -c '%a' "$file") == 600 ]] || fail "backup inputs must have mode 0600"
  [[ $(stat -c '%u' "$file") == "$(id -u)" ]] || fail "backup inputs have an unexpected owner"
  canonical=$(readlink -f "$file")
  [[ "$canonical" == "$file" ]] || fail "backup input path must already be canonical"
  if [[ -z "$backup_dir" ]]; then
    backup_dir=$(dirname -- "$file")
  elif [[ $(dirname -- "$file") != "$backup_dir" ]]; then
    fail "backup inputs must share one directory"
  fi
done

dump_name=$(basename -- "$dump")
media_name=$(basename -- "$media")
manifest_name=$(basename -- "$manifest")
if [[ "$dump_name" =~ ^postgres-([0-9]{8}T[0-9]{6}Z)\.dump$ ]]; then
  timestamp=${BASH_REMATCH[1]}
else
  fail "unexpected PostgreSQL backup filename"
fi
[[ "$media_name" == "media-${timestamp}.tar.gz" ]] || fail "media backup timestamp does not match"
[[ "$manifest_name" == "backup-${timestamp}.sha256" ]] || fail "manifest timestamp does not match"

mapfile -t manifest_lines <"$manifest"
[[ ${#manifest_lines[@]} -eq 2 ]] || fail "backup manifest must contain exactly two entries"
printf '%s\n' "${manifest_lines[@]}" | awk -v dump="$dump_name" -v media="$media_name" '
  BEGIN { found_dump = 0; found_media = 0 }
  NF != 2 || $1 !~ /^[0-9a-f]{64}$/ { exit 1 }
  $2 == dump { found_dump++ ; next }
  $2 == media { found_media++ ; next }
  { exit 1 }
  END { if (found_dump != 1 || found_media != 1) exit 1 }
' || fail "backup manifest contains unexpected paths or hashes"
(
  cd "$backup_dir"
  sha256sum --strict -c "$manifest_name" >/dev/null
) || fail "backup manifest verification failed"

plaintext_bytes=$(( $(stat -c '%s' "$dump") + $(stat -c '%s' "$media") + $(stat -c '%s' "$manifest") ))
(( plaintext_bytes <= max_plaintext_bytes )) || fail "backup exceeds the safe GitHub Release asset limit"

temporary=$(mktemp -d)
chmod 700 "$temporary"
release_id=''
release_published=false
completed=false
cleanup() {
  status=$?
  if [[ "$completed" != true && "$release_published" != true && "$release_id" =~ ^[1-9][0-9]*$ ]]; then
    curl --config "$curl_config" --silent --show-error --fail-with-body --max-time 30 \
      -X DELETE "$api_url/repos/$repository/releases/$release_id" >/dev/null 2>&1 || true
  fi
  rm -rf "$temporary"
  exit "$status"
}
trap cleanup EXIT INT TERM
umask 077

dump_sha=$(sha256sum "$dump" | awk '{print $1}')
media_sha=$(sha256sum "$media" | awk '{print $1}')
manifest_sha=$(sha256sum "$manifest" | awk '{print $1}')
metadata="$temporary/metadata.json"
jq -n \
  --arg schema_version "1" \
  --arg created_at "$timestamp" \
  --arg source_sha "$snapshot_source_sha" \
  --arg image "$snapshot_image" \
  --arg candidate_source_sha "$source_sha" \
  --arg candidate_image "$image" \
  --arg dump_name "$dump_name" --arg dump_sha "$dump_sha" --argjson dump_size "$(stat -c '%s' "$dump")" \
  --arg media_name "$media_name" --arg media_sha "$media_sha" --argjson media_size "$(stat -c '%s' "$media")" \
  --arg manifest_name "$manifest_name" --arg manifest_sha "$manifest_sha" --argjson manifest_size "$(stat -c '%s' "$manifest")" \
  '{schema_version: $schema_version, created_at: $created_at, source_sha: $source_sha, image: $image,
    candidate_source_sha: $candidate_source_sha, candidate_image: $candidate_image,
    files: [
      {name: $dump_name, sha256: $dump_sha, size: $dump_size},
      {name: $media_name, sha256: $media_sha, size: $media_size},
      {name: $manifest_name, sha256: $manifest_sha, size: $manifest_size}
    ]}' >"$metadata"

asset_name="maxposty-backup-${timestamp}.tar.cms"
encrypted="$temporary/$asset_name"
tar --format=ustar --sort=name --owner=0 --group=0 --numeric-owner --mtime='UTC 1970-01-01' \
  -cf - -C "$backup_dir" "$dump_name" "$media_name" "$manifest_name" -C "$temporary" metadata.json |
  openssl cms -encrypt -binary -stream -outform DER -aes-256-gcm \
    -recip "$recipient_cert" -out "$encrypted"
[[ -s "$encrypted" ]] || fail "encrypted backup is empty"
chmod 600 "$encrypted"
encrypted_size=$(stat -c '%s' "$encrypted")
(( encrypted_size < 2147483648 )) || fail "encrypted backup exceeds GitHub's 2 GiB per-asset limit"
encrypted_sha=$(sha256sum "$encrypted" | awk '{print $1}')

tag="maxposty-backup-${timestamp}"
create_request="$temporary/create-release.json"
create_response="$temporary/create-release-response.json"
jq -n --arg tag "$tag" --arg sha "$source_sha" --arg name "Encrypted production backup ${timestamp}" \
  --arg body "Encrypted MaxPosty backup. Recovery requires the offline private key. Source commit: ${source_sha}." \
  '{tag_name: $tag, target_commitish: $sha, name: $name, body: $body,
    draft: true, prerelease: true, make_latest: "false"}' >"$create_request"
curl --config "$curl_config" --silent --show-error --fail-with-body --max-time 60 \
  -H 'Accept: application/vnd.github+json' -H 'Content-Type: application/json' \
  -X POST --data-binary "@$create_request" "$api_url/repos/$repository/releases" >"$create_response"
release_id=$(jq -er '.id | select(type == "number" and . > 0)' "$create_response") || fail "GitHub did not return a release id"
jq -e --arg tag "$tag" '.draft == true and .tag_name == $tag' "$create_response" >/dev/null || fail "GitHub created an unexpected release"

upload_response="$temporary/upload-response.json"
curl --config "$curl_config" --silent --show-error --fail-with-body --max-time 600 \
  -H 'Accept: application/vnd.github+json' -H 'Content-Type: application/octet-stream' \
  -X POST --data-binary "@$encrypted" \
  "$upload_url/repos/$repository/releases/$release_id/assets?name=$asset_name" >"$upload_response"
asset_id=$(jq -er '.id | select(type == "number" and . > 0)' "$upload_response") || fail "GitHub did not return an asset id"
remote_name=$(jq -er '.name' "$upload_response")
remote_state=$(jq -er '.state' "$upload_response")
remote_size=$(jq -er '.size | select(type == "number")' "$upload_response")
remote_digest=$(jq -er '.digest | select(type == "string")' "$upload_response")
[[ "$remote_name" == "$asset_name" && "$remote_state" == uploaded ]] || fail "GitHub did not finish the asset upload"
[[ "$remote_size" == "$encrypted_size" ]] || fail "uploaded asset size does not match"
[[ "$remote_digest" == "sha256:$encrypted_sha" ]] || fail "uploaded asset digest does not match"

redownloaded="$temporary/redownloaded.cms"
curl --config "$curl_config" --silent --show-error --fail-with-body --location --max-time 600 \
  -H 'Accept: application/octet-stream' \
  "$api_url/repos/$repository/releases/assets/$asset_id" -o "$redownloaded"
[[ $(stat -c '%s' "$redownloaded") == "$encrypted_size" ]] || fail "redownloaded asset size does not match"
[[ $(sha256sum "$redownloaded" | awk '{print $1}') == "$encrypted_sha" ]] || fail "redownloaded asset digest does not match"

publish_request="$temporary/publish-release.json"
publish_response="$temporary/publish-release-response.json"
printf '%s\n' '{"draft":false}' >"$publish_request"
# From this point an ambiguous network error may still mean GitHub published the
# immutable release. Preserve it instead of risking deletion of the only offsite copy.
release_published=true
curl --config "$curl_config" --silent --show-error --fail-with-body --max-time 60 \
  -H 'Accept: application/vnd.github+json' -H 'Content-Type: application/json' \
  -X PATCH --data-binary "@$publish_request" "$api_url/repos/$repository/releases/$release_id" >"$publish_response"

verified=false
for _ in {1..10}; do
  curl --config "$curl_config" --silent --show-error --fail-with-body --max-time 60 \
    -H 'Accept: application/vnd.github+json' \
    "$api_url/repos/$repository/releases/$release_id" >"$publish_response"
  if jq -e --arg tag "$tag" --arg name "$asset_name" --arg digest "sha256:$encrypted_sha" \
    '.draft == false and .immutable == true and .tag_name == $tag and
     ([.assets[] | select(.name == $name and .state == "uploaded" and .digest == $digest)] | length == 1)' \
    "$publish_response" >/dev/null; then
    verified=true
    break
  fi
  sleep 2
done
[[ "$verified" == true ]] || fail "published backup release is not immutable or its asset changed"

completed=true
echo "Encrypted offsite backup verified and published: $tag (sha256:$encrypted_sha)"
