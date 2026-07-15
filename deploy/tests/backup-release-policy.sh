#!/usr/bin/env bash
set -euo pipefail

repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
if ! stat -c '%a' "$repo_root" >/dev/null 2>&1; then
  echo "Encrypted backup policy integration test skipped: GNU stat is required."
  exit 0
fi

fixture=$(mktemp -d)
trap 'rm -rf "$fixture"' EXIT
install -d "$fixture/bin" "$fixture/backups" "$fixture/state-success" "$fixture/state-retry" \
  "$fixture/state-ambiguous" "$fixture/state-failure"
ln -s "$repo_root/deploy/tests/fake-backup-curl.sh" "$fixture/bin/curl"
for command in jq openssl sha256sum stat tar readlink sleep awk basename dirname mktemp chmod rm; do
  ln -s "$(type -P "$command")" "$fixture/bin/$command"
done

timestamp=20260714T120000Z
dump="$fixture/backups/postgres-${timestamp}.dump"
media="$fixture/backups/media-${timestamp}.tar.gz"
manifest="$fixture/backups/backup-${timestamp}.sha256"
printf 'validated custom dump fixture\n' >"$dump"
tar -czf "$media" --files-from /dev/null
(
  cd "$fixture/backups"
  sha256sum "$(basename "$dump")" "$(basename "$media")" >"$(basename "$manifest")"
)
chmod 600 "$dump" "$media" "$manifest"

openssl req -x509 -newkey rsa:3072 -sha256 -nodes \
  -keyout "$fixture/private-key.pem" -out "$fixture/recipient.pem" -days 31 \
  -subj '/CN=MaxPosty Backup Test/' \
  -addext 'basicConstraints=critical,CA:FALSE' \
  -addext 'keyUsage=critical,keyEncipherment' >/dev/null 2>&1
printf 'header = "Authorization: Bearer test-only"\n' >"$fixture/curl.config"
chmod 600 "$fixture/curl.config"
image="ghcr.io/example/backend@sha256:$(printf 'a%.0s' {1..64})"
sha=0123456789abcdef0123456789abcdef01234567

PATH="$fixture/bin:/usr/bin:/bin" \
  TEST_CURL_STATE="$fixture/state-success" \
  MAXPOSTY_BACKUP_CURL_CONFIG="$fixture/curl.config" \
  MAXPOSTY_BACKUP_REPOSITORY=example/backend \
  MAXPOSTY_BACKUP_SOURCE_SHA="$sha" \
  MAXPOSTY_BACKUP_RECIPIENT_CERT="$fixture/recipient.pem" \
  MAXPOSTY_BACKUP_API_URL=http://127.0.0.1:8123 \
  MAXPOSTY_BACKUP_UPLOAD_URL=http://127.0.0.1:8123 \
  "$repo_root/deploy/backup/after-backup-github-release.sh" \
  "$dump" "$media" "$manifest" "$image" >/dev/null

[[ -s "$fixture/state-success/asset" && ! -e "$fixture/state-success/deleted" ]]
[[ $(<"$fixture/state-success/create-calls") == 1 ]]
openssl cms -decrypt -binary -inform DER -inkey "$fixture/private-key.pem" \
  -in "$fixture/state-success/asset" -out "$fixture/roundtrip.tar"
tar -tf "$fixture/roundtrip.tar" | grep -Fx metadata.json >/dev/null
tar -tf "$fixture/roundtrip.tar" | grep -Fx "$(basename "$dump")" >/dev/null

PATH="$fixture/bin:/usr/bin:/bin" \
  TEST_CURL_STATE="$fixture/state-retry" TEST_CONNECT_FAILURE_ONCE=true \
  MAXPOSTY_BACKUP_CONNECT_ATTEMPTS=2 MAXPOSTY_BACKUP_CONNECT_RETRY_DELAY=0 \
  MAXPOSTY_BACKUP_CURL_CONFIG="$fixture/curl.config" \
  MAXPOSTY_BACKUP_REPOSITORY=example/backend \
  MAXPOSTY_BACKUP_SOURCE_SHA="$sha" \
  MAXPOSTY_BACKUP_RECIPIENT_CERT="$fixture/recipient.pem" \
  MAXPOSTY_BACKUP_API_URL=http://127.0.0.1:8123 \
  MAXPOSTY_BACKUP_UPLOAD_URL=http://127.0.0.1:8123 \
  "$repo_root/deploy/backup/after-backup-github-release.sh" \
  "$dump" "$media" "$manifest" "$image" >/dev/null
[[ -e "$fixture/state-retry/connect-failure-injected" ]]
[[ -s "$fixture/state-retry/asset" && ! -e "$fixture/state-retry/deleted" ]]
[[ $(<"$fixture/state-retry/create-calls") == 2 ]]

if PATH="$fixture/bin:/usr/bin:/bin" \
  TEST_CURL_STATE="$fixture/state-ambiguous" TEST_ESTABLISHED_TIMEOUT_ONCE=true \
  MAXPOSTY_BACKUP_CONNECT_ATTEMPTS=2 MAXPOSTY_BACKUP_CONNECT_RETRY_DELAY=0 \
  MAXPOSTY_BACKUP_CURL_CONFIG="$fixture/curl.config" \
  MAXPOSTY_BACKUP_REPOSITORY=example/backend \
  MAXPOSTY_BACKUP_SOURCE_SHA="$sha" \
  MAXPOSTY_BACKUP_RECIPIENT_CERT="$fixture/recipient.pem" \
  MAXPOSTY_BACKUP_API_URL=http://127.0.0.1:8123 \
  MAXPOSTY_BACKUP_UPLOAD_URL=http://127.0.0.1:8123 \
  "$repo_root/deploy/backup/after-backup-github-release.sh" \
  "$dump" "$media" "$manifest" "$image" >/dev/null 2>&1; then
  echo "Offsite backup retried an ambiguous request after connecting" >&2
  exit 1
fi
[[ -e "$fixture/state-ambiguous/established-timeout-injected" ]]
[[ $(<"$fixture/state-ambiguous/create-calls") == 1 ]]

cp "$fixture/state-success/asset" "$fixture/tampered.cms"
size=$(stat -c '%s' "$fixture/tampered.cms")
tamper_offset=$((size / 2))
original_byte=$(od -An -tu1 -j "$tamper_offset" -N 1 "$fixture/tampered.cms" | tr -d '[:space:]')
tampered_byte=$((original_byte ^ 1))
printf -v tampered_octal '\\%03o' "$tampered_byte"
printf '%b' "$tampered_octal" | dd of="$fixture/tampered.cms" bs=1 seek="$tamper_offset" conv=notrunc status=none
if openssl cms -decrypt -binary -inform DER -inkey "$fixture/private-key.pem" \
  -in "$fixture/tampered.cms" -out "$fixture/tampered.tar" 2>/dev/null; then
  echo "CMS authentication accepted a modified backup" >&2
  exit 1
fi

if PATH="$fixture/bin:/usr/bin:/bin" \
  TEST_CURL_STATE="$fixture/state-failure" TEST_BAD_DIGEST=true \
  MAXPOSTY_BACKUP_CURL_CONFIG="$fixture/curl.config" \
  MAXPOSTY_BACKUP_REPOSITORY=example/backend \
  MAXPOSTY_BACKUP_SOURCE_SHA="$sha" \
  MAXPOSTY_BACKUP_RECIPIENT_CERT="$fixture/recipient.pem" \
  MAXPOSTY_BACKUP_API_URL=http://127.0.0.1:8123 \
  MAXPOSTY_BACKUP_UPLOAD_URL=http://127.0.0.1:8123 \
  "$repo_root/deploy/backup/after-backup-github-release.sh" \
  "$dump" "$media" "$manifest" "$image" >/dev/null 2>&1; then
  echo "Offsite backup accepted a mismatched GitHub digest" >&2
  exit 1
fi
[[ -e "$fixture/state-failure/deleted" ]]

[[ $(grep -c '^      contents: write$' "$repo_root/.github/workflows/deploy.yml") -eq 1 ]]
awk '
  /^  deploy:/ { in_deploy = 1; next }
  in_deploy && /^  [A-Za-z0-9_-]+:/ { exit(found ? 0 : 1) }
  in_deploy && /^      contents: write$/ { found = 1 }
  END { if (!found) exit 1 }
' "$repo_root/.github/workflows/deploy.yml"
grep -F 'MAXPOSTY_BACKUP_CURL_CONFIG' "$repo_root/deploy/run-from-ci.sh" >/dev/null
if grep -Eq 'curl[^\n]*(GHCR_TOKEN|github\.token)' "$repo_root/.github/workflows/deploy.yml"; then
  echo "GitHub token must not be passed to curl on the command line" >&2
  exit 1
fi

echo "Encrypted immutable backup policy tests passed."
