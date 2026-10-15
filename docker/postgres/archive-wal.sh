#!/bin/sh
# PostgreSQL owns immutable completed input segments. Publish without replacing
# an existing archive, and acknowledge only a byte-identical, durable copy.
set -eu
umask 077

compress_legacy=false
if [ "${1:-}" = --compress-legacy ]; then
  compress_legacy=true
  shift
fi
source=${1:-}
name=${2:-}
archive=${3:-/var/lib/postgresql/wal-archive}
fail() { echo "WAL archive verification failed" >&2; exit 1; }
[ "$#" -ge 2 ] && [ "$#" -le 3 ] || fail
[ -f "$source" ] && [ ! -L "$source" ] || fail
[ -d "$archive" ] && [ ! -L "$archive" ] || fail

compressed=false
if printf '%s\n' "$name" | grep -Eq '^[0-9A-F]{24}$'; then
  compressed=true
elif ! printf '%s\n' "$name" | grep -Eq '^([0-9A-F]{8}\.history|[0-9A-F]{24}\.[0-9A-F]{8}\.backup|[0-9A-F]{24}\.partial)$'; then
  fail
fi
if [ "$compress_legacy" = true ]; then
  [ "$compressed" = true ] && [ "$source" = "$archive/$name" ] || fail
fi

verify_compressed() {
  [ -f "$1" ] && [ ! -L "$1" ] || return 1
  gzip -t "$1" 2>/dev/null || return 1
  # gzip -t checks the trailer/CRC separately: a matching cmp must not mask a
  # decompressor error at EOF on shells without pipefail.
  gzip -dc "$1" 2>/dev/null | cmp -s "$source" -
}

raw="$archive/$name"
packed="$raw.gz"
raw_exists=false
if [ -e "$raw" ] || [ -L "$raw" ]; then
  [ -f "$raw" ] && [ ! -L "$raw" ] && cmp -s "$source" "$raw" || fail
  raw_exists=true
fi
if [ "$compressed" = true ] && { [ -e "$packed" ] || [ -L "$packed" ]; }; then
  verify_compressed "$packed" || fail
  sync
  exit 0
fi
if [ "$raw_exists" = true ] && [ "$compress_legacy" = false ]; then
  sync
  exit 0
fi

temporary=$(mktemp "$archive/.$name.tmp.XXXXXXXX") || fail
trap 'rm -f "$temporary"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
if [ "$compressed" = true ]; then
  # stdin omits source filename/timestamp and works with Alpine BusyBox gzip.
  gzip -1 -c <"$source" >"$temporary" || fail
  verify_compressed "$temporary" || fail
  final=$packed
else
  cp "$source" "$temporary" && cmp -s "$source" "$temporary" || fail
  final=$raw
fi
chmod 600 "$temporary"
sync
# A hard link publishes atomically without overwriting a concurrent archive.
if ! ln "$temporary" "$final" 2>/dev/null; then
  if [ "$compressed" = true ]; then
    verify_compressed "$final" || fail
  else
    [ -f "$final" ] && [ ! -L "$final" ] && cmp -s "$source" "$final" || fail
  fi
fi
sync
rm -f "$temporary"
trap - EXIT
