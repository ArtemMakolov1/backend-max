#!/bin/sh
# One-off operation inside the accepted postgres container, while the caller
# holds the host deployment lock. Never touches live pg_wal or unknown names.
set -eu
umask 077
archive=${1:-/var/lib/postgresql/wal-archive}
helper=${2:-/usr/local/bin/maxposty-archive-wal}
fail() { echo "Legacy WAL conversion verification failed" >&2; exit 1; }
identity_of() { stat -c '%d:%i:%s:%Y:%Z' "$1" 2>/dev/null || stat -f '%d:%i:%z:%m:%c' "$1"; }
size_of() { stat -c '%s' "$1" 2>/dev/null || stat -f '%z' "$1"; }
[ "$#" -le 2 ] && [ -d "$archive" ] && [ ! -L "$archive" ] || fail
[ -f "$helper" ] && [ ! -L "$helper" ] || fail
archive=$(CDPATH='' cd -- "$archive" && pwd -P) || fail
case "$archive" in
  /var/lib/postgresql/wal-archive) ;;
  /var/lib/postgresql|/var/lib/postgresql/*) fail ;;
esac
converted=0
reclaimed=0
for raw in "$archive"/*; do
  name=${raw##*/}
  printf '%s\n' "$name" | grep -Eq '^[0-9A-F]{24}$' || continue
  [ -f "$raw" ] && [ ! -L "$raw" ] || fail
  identity=$(identity_of "$raw") || fail
  raw_size=$(size_of "$raw") || fail
  "$helper" --compress-legacy "$raw" "$name" "$archive" || fail
  packed="$raw.gz"
  [ -f "$packed" ] && [ ! -L "$packed" ] && gzip -t "$packed" 2>/dev/null || fail
  gzip -dc "$packed" 2>/dev/null | cmp -s "$raw" - || fail
  [ "$(identity_of "$raw")" = "$identity" ] && [ ! -L "$raw" ] || fail
  # The published gzip has already been synced by archive-wal. Only now is the
  # old, byte-identical regular entry removed; interruption leaves either form.
  rm "$raw" || fail
  sync
  packed_size=$(size_of "$packed") || fail
  converted=$((converted + 1))
  reclaimed=$((reclaimed + raw_size - packed_size))
done
printf 'Legacy WAL conversion complete: converted=%s reclaimed_bytes=%s\n' "$converted" "$reclaimed"
