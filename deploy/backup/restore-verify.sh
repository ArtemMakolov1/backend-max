#!/usr/bin/env bash
set -euo pipefail

encrypted=${1:-}
private_key=${2:-}
output_dir=${3:-}

if [[ -z "$encrypted" || -z "$private_key" || -z "$output_dir" ]]; then
  echo "Usage: $0 ENCRYPTED_BACKUP PRIVATE_KEY EMPTY_OUTPUT_DIRECTORY" >&2
  exit 2
fi
[[ -f "$encrypted" && ! -L "$encrypted" ]] || { echo "Encrypted backup must be a regular file" >&2; exit 1; }
[[ -f "$private_key" && ! -L "$private_key" ]] || { echo "Private key must be a regular file" >&2; exit 1; }
private_key_mode=$(stat -f '%Lp' "$private_key" 2>/dev/null || stat -c '%a' "$private_key")
[[ "$private_key_mode" == 400 || "$private_key_mode" == 600 ]] || {
  echo "Private key must have mode 0400 or 0600" >&2
  exit 1
}
if [[ -e "$output_dir" ]]; then
  [[ -d "$output_dir" && ! -L "$output_dir" && -z $(find "$output_dir" -mindepth 1 -print -quit) ]] || {
    echo "Output directory must be empty" >&2
    exit 1
  }
  chmod 700 "$output_dir"
else
  install -d -m 700 "$output_dir"
fi

openssl_bin=${OPENSSL_BIN:-}
if [[ -z "$openssl_bin" ]]; then
  for candidate in openssl /opt/homebrew/opt/openssl@3/bin/openssl /opt/homebrew/opt/openssl/bin/openssl; do
    if command -v "$candidate" >/dev/null 2>&1 && "$candidate" version 2>/dev/null | grep -q '^OpenSSL 3\.'; then
      openssl_bin=$candidate
      break
    fi
  done
fi
[[ -n "$openssl_bin" && -x $(command -v "$openssl_bin" 2>/dev/null || true) ]] || {
  echo "OpenSSL 3 is required (set OPENSSL_BIN when the system openssl is LibreSSL)" >&2
  exit 1
}
"$openssl_bin" version | grep -q '^OpenSSL 3\.' || { echo "OpenSSL 3 is required" >&2; exit 1; }
for command in jq pg_restore sha256sum tar; do
  command -v "$command" >/dev/null 2>&1 || { echo "Required command is missing: $command" >&2; exit 1; }
done

temporary=$(mktemp -d)
trap 'rm -rf "$temporary"' EXIT
umask 077
archive="$temporary/backup.tar"
"$openssl_bin" cms -decrypt -binary -inform DER -inkey "$private_key" -in "$encrypted" -out "$archive"

members="$temporary/members"
tar -tf "$archive" >"$members"
[[ $(wc -l <"$members" | tr -d ' ') == 4 ]] || { echo "Backup archive must contain exactly four files" >&2; exit 1; }
tar -tvf "$archive" | awk '$1 !~ /^-/ { exit 1 } END { if (NR != 4) exit 1 }' || {
  echo "Backup archive must contain regular files only" >&2
  exit 1
}
awk '
  $0 == "metadata.json" { metadata++; next }
  $0 ~ /^postgres-[0-9]{8}T[0-9]{6}Z\.dump$/ { dump++; next }
  $0 ~ /^media-[0-9]{8}T[0-9]{6}Z\.tar\.gz$/ { media++; next }
  $0 ~ /^backup-[0-9]{8}T[0-9]{6}Z\.sha256$/ { manifest++; next }
  { exit 1 }
  END { if (metadata != 1 || dump != 1 || media != 1 || manifest != 1) exit 1 }
' "$members" || { echo "Backup archive contains unsafe or unexpected paths" >&2; exit 1; }

tar -xf "$archive" -C "$output_dir"
metadata="$output_dir/metadata.json"
[[ -f "$metadata" && ! -L "$metadata" ]] || { echo "Backup metadata is not a regular file" >&2; exit 1; }
jq -e '.schema_version == "1" and
  (.created_at | test("^[0-9]{8}T[0-9]{6}Z$")) and
  (.source_sha | test("^[0-9a-f]{40}$")) and
  (.image | test("^ghcr\\.io/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+@sha256:[0-9a-f]{64}$")) and
  (.files | type == "array" and length == 3)' "$metadata" >/dev/null || {
  echo "Backup metadata is invalid" >&2
  exit 1
}

timestamp=$(jq -er '.created_at' "$metadata")
dump="postgres-${timestamp}.dump"
media="media-${timestamp}.tar.gz"
manifest="backup-${timestamp}.sha256"
for name in "$dump" "$media" "$manifest"; do
  [[ -f "$output_dir/$name" && ! -L "$output_dir/$name" ]] || { echo "Backup file is missing: $name" >&2; exit 1; }
  expected_sha=$(jq -er --arg name "$name" '.files[] | select(.name == $name) | .sha256' "$metadata")
  expected_size=$(jq -er --arg name "$name" '.files[] | select(.name == $name) | .size' "$metadata")
  [[ "$expected_sha" =~ ^[0-9a-f]{64}$ && "$expected_size" =~ ^[0-9]+$ ]] || { echo "Invalid metadata for $name" >&2; exit 1; }
  [[ $(sha256sum "$output_dir/$name" | awk '{print $1}') == "$expected_sha" ]] || { echo "Hash mismatch for $name" >&2; exit 1; }
  [[ $(stat -f '%z' "$output_dir/$name" 2>/dev/null || stat -c '%s' "$output_dir/$name") == "$expected_size" ]] || { echo "Size mismatch for $name" >&2; exit 1; }
done

(
  cd "$output_dir"
  sha256sum --strict -c "$manifest" >/dev/null
)
pg_restore --list "$output_dir/$dump" >/dev/null
tar -tzf "$output_dir/$media" >/dev/null
chmod 600 "$output_dir/$dump" "$output_dir/$media" "$output_dir/$manifest" "$metadata"
echo "Backup decrypted and verified in $output_dir"
echo "Image digest: $(jq -r '.image' "$metadata")"
