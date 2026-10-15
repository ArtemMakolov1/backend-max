#!/bin/sh
# Works with retained raw WAL and newly compressed segments. PostgreSQL passes
# the destination only in restore mode; scheduled backups use --verify.
set -eu
umask 077
verify_only=false
if [ "${1:-}" = --verify ]; then
  verify_only=true
  shift
fi
name=${1:-}
destination=${2:-}
archive=${3:-/var/lib/postgresql/wal-archive}
fail() { echo "WAL restore verification failed" >&2; exit 1; }
if [ "$verify_only" = true ]; then
  [ "$#" -ge 1 ] && [ "$#" -le 2 ] || fail
  archive=${2:-/var/lib/postgresql/wal-archive}
else
  [ "$#" -ge 2 ] && [ "$#" -le 3 ] && [ -n "$destination" ] || fail
fi
printf '%s\n' "$name" | grep -Eq '^([0-9A-F]{24}|[0-9A-F]{8}\.history|[0-9A-F]{24}\.[0-9A-F]{8}\.backup|[0-9A-F]{24}\.partial)$' || fail
[ -d "$archive" ] && [ ! -L "$archive" ] || fail
raw="$archive/$name"
packed="$raw.gz"
has_raw=false
has_packed=false
if [ -e "$raw" ] || [ -L "$raw" ]; then
  [ -f "$raw" ] && [ ! -L "$raw" ] || fail
  has_raw=true
fi
if [ -e "$packed" ] || [ -L "$packed" ]; then
  printf '%s\n' "$name" | grep -Eq '^[0-9A-F]{24}$' || fail
  [ -f "$packed" ] && [ ! -L "$packed" ] && gzip -t "$packed" 2>/dev/null || fail
  has_packed=true
fi
[ "$has_raw" = true ] || [ "$has_packed" = true ] || fail
if [ "$has_raw" = true ] && [ "$has_packed" = true ]; then
  gzip -dc "$packed" 2>/dev/null | cmp -s "$raw" - || fail
fi
[ "$verify_only" = false ] || exit 0
temporary=$(mktemp "$destination.restore.XXXXXXXX") || fail
trap 'rm -f "$temporary"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
if [ "$has_packed" = true ]; then
  gzip -dc "$packed" >"$temporary" || fail
else
  cp "$raw" "$temporary" || fail
fi
chmod 600 "$temporary"
mv -f "$temporary" "$destination"
trap - EXIT
