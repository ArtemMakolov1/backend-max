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
  "$fixture/state-tls-retry" "$fixture/state-ambiguous" "$fixture/state-failure"
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

# A TCP connection followed by an HTTPS handshake timeout cannot have sent the
# mutating HTTP request and is safe to retry. This is the production failure
# mode that time_connect alone cannot distinguish from an ambiguous response.
PATH="$fixture/bin:/usr/bin:/bin" \
  TEST_CURL_STATE="$fixture/state-tls-retry" TEST_TLS_HANDSHAKE_TIMEOUT_ONCE=true \
  MAXPOSTY_BACKUP_CONNECT_ATTEMPTS=2 MAXPOSTY_BACKUP_CONNECT_RETRY_DELAY=0 \
  MAXPOSTY_BACKUP_CURL_CONFIG="$fixture/curl.config" \
  MAXPOSTY_BACKUP_REPOSITORY=example/backend \
  MAXPOSTY_BACKUP_SOURCE_SHA="$sha" \
  MAXPOSTY_BACKUP_RECIPIENT_CERT="$fixture/recipient.pem" \
  MAXPOSTY_BACKUP_API_URL=https://api.example.test \
  MAXPOSTY_BACKUP_UPLOAD_URL=https://uploads.example.test \
  "$repo_root/deploy/backup/after-backup-github-release.sh" \
  "$dump" "$media" "$manifest" "$image" >/dev/null
[[ -e "$fixture/state-tls-retry/tls-handshake-timeout-injected" ]]
[[ -s "$fixture/state-tls-retry/asset" && ! -e "$fixture/state-tls-retry/deleted" ]]
[[ $(<"$fixture/state-tls-retry/create-calls") == 2 ]]

if PATH="$fixture/bin:/usr/bin:/bin" \
  TEST_CURL_STATE="$fixture/state-ambiguous" TEST_ESTABLISHED_TIMEOUT_ONCE=true \
  MAXPOSTY_BACKUP_CONNECT_ATTEMPTS=2 MAXPOSTY_BACKUP_CONNECT_RETRY_DELAY=0 \
  MAXPOSTY_BACKUP_CURL_CONFIG="$fixture/curl.config" \
  MAXPOSTY_BACKUP_REPOSITORY=example/backend \
  MAXPOSTY_BACKUP_SOURCE_SHA="$sha" \
  MAXPOSTY_BACKUP_RECIPIENT_CERT="$fixture/recipient.pem" \
  MAXPOSTY_BACKUP_API_URL=https://api.example.test \
  MAXPOSTY_BACKUP_UPLOAD_URL=https://uploads.example.test \
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

# Deploys must not publish backups synchronously. The scheduled backup workflow
# owns the only contents:write permission and keeps release latency independent
# from media size or temporary GitHub API availability.
awk '
  /^  deploy:/ { in_deploy = 1; next }
  in_deploy && /^  [A-Za-z0-9_-]+:/ { exit(found ? 0 : 1) }
  in_deploy && /^      contents: read$/ { found = 1 }
  in_deploy && /^      contents: write$/ { exit 1 }
  END { if (!found) exit 1 }
' "$repo_root/.github/workflows/deploy.yml"
grep -F 'MAXPOSTY_BACKUP_CURL_CONFIG' "$repo_root/deploy/run-from-ci.sh" >/dev/null
if grep -Eq 'curl[^\n]*(GHCR_TOKEN|github\.token)' "$repo_root/.github/workflows/deploy.yml"; then
  echo "GitHub token must not be passed to curl on the command line" >&2
  exit 1
fi

# The scheduled backup is invoked through the .../current symlink, so the
# release guard must resolve physical paths: via-symlink calls pass the guard
# while calls from an unaccepted location are still rejected. Requires the
# same tooling the script itself checks for before the guard runs.
if command -v flock >/dev/null 2>&1 && docker compose version >/dev/null 2>&1; then
  guard_message='must run from the currently accepted versioned release'
  release_sha=0123456789abcdef0123456789abcdef01234567
  install -d "$fixture/install/releases/$release_sha"
  cp -R "$repo_root/deploy" "$fixture/install/releases/$release_sha/deploy"
  ln -s "releases/$release_sha" "$fixture/install/current"

  guard_stderr=$(printf 'token\n' | "$fixture/install/current/deploy/backup/run-scheduled-backup.sh" \
    example/backend 2>&1 >/dev/null || true)
  if grep -F "$guard_message" <<<"$guard_stderr" >/dev/null; then
    echo "Release guard rejected an invocation through the current symlink" >&2
    exit 1
  fi

  guard_stderr=$(printf 'token\n' | "$repo_root/deploy/backup/run-scheduled-backup.sh" \
    example/backend 2>&1 >/dev/null || true)
  if ! grep -F "$guard_message" <<<"$guard_stderr" >/dev/null; then
    echo "Release guard accepted an invocation outside the accepted release" >&2
    exit 1
  fi
else
  echo "Release guard symlink test skipped: flock and docker compose are required."
fi

echo "Encrypted immutable backup policy tests passed."
