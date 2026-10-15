#!/bin/sh
# Runs only in a disposable pinned PostgreSQL container with /tmp writable.
set -eu
helpers=${1:-/helpers}
if "$helpers/compress-legacy-wal.sh" /var/lib/postgresql "$helpers/archive-wal.sh" >/dev/null 2>&1; then
  echo 'Legacy conversion must reject the PostgreSQL data root' >&2
  exit 1
fi
mkdir -m 700 /tmp/wal-archive
initdb -D /tmp/primary --auth=trust >/dev/null
cat >>/tmp/primary/postgresql.conf <<CONFIG
listen_addresses = ''
unix_socket_directories = '/tmp'
archive_mode = on
archive_timeout = '300s'
archive_command = '$helpers/archive-wal.sh %p %f /tmp/wal-archive'
CONFIG
cleanup() {
  pg_ctl -D /tmp/primary -m immediate stop >/dev/null 2>&1 || true
  pg_ctl -D /tmp/recovery -m immediate stop >/dev/null 2>&1 || true
}
trap cleanup EXIT
pg_ctl -D /tmp/primary -l /tmp/primary.log -w start >/dev/null
psql -h /tmp -U postgres -d postgres -v ON_ERROR_STOP=1 -c 'CREATE TABLE wal_recovery_fixture(value integer); INSERT INTO wal_recovery_fixture VALUES(1)' >/dev/null
pg_basebackup -h /tmp -U postgres -D /tmp/recovery --wal-method=stream --checkpoint=fast
pg_verifybackup /tmp/recovery >/dev/null
psql -h /tmp -U postgres -d postgres -v ON_ERROR_STOP=1 -c 'INSERT INTO wal_recovery_fixture VALUES(2)' >/dev/null
target=$(psql -h /tmp -U postgres -d postgres -Atc 'SELECT pg_current_wal_lsn()')
segment=$(psql -h /tmp -U postgres -d postgres -Atc 'SELECT pg_walfile_name(pg_switch_wal())')
archived=false
for attempt in $(seq 1 30); do
  if [ -f "/tmp/wal-archive/$segment.gz" ]; then
    archived=true
    break
  fi
  sleep 1
done
[ "$archived" = true ] || { cat /tmp/primary.log; exit 1; }
"$helpers/restore-wal.sh" --verify "$segment" /tmp/wal-archive
pg_ctl -D /tmp/primary -m fast -w stop >/dev/null
cat >>/tmp/recovery/postgresql.conf <<CONFIG
archive_mode = off
restore_command = '$helpers/restore-wal.sh %f %p /tmp/wal-archive'
recovery_target_lsn = '$target'
recovery_target_action = 'promote'
CONFIG
touch /tmp/recovery/recovery.signal
# Force recovery to obtain the post-base segment from the compressed archive.
rm -f "/tmp/recovery/pg_wal/$segment"
pg_ctl -D /tmp/recovery -l /tmp/recovery.log -w start >/dev/null || { cat /tmp/recovery.log; exit 1; }
count=$(psql -h /tmp -U postgres -d postgres -Atc 'SELECT count(*) FROM wal_recovery_fixture')
[ "$count" = 2 ] || { cat /tmp/recovery.log; exit 1; }
grep -F 'restored log file' /tmp/recovery.log >/dev/null
pg_ctl -D /tmp/recovery -m fast -w stop >/dev/null

# Converting legacy files verifies and removes only completed archive entries.
legacy=000000010000000000000099
dd if=/dev/zero of="/tmp/wal-archive/$legacy" bs=1048576 count=16 2>/dev/null
"$helpers/compress-legacy-wal.sh" /tmp/wal-archive "$helpers/archive-wal.sh"
[ ! -e "/tmp/wal-archive/$legacy" ]
"$helpers/restore-wal.sh" --verify "$legacy" /tmp/wal-archive
echo 'Pinned PostgreSQL compressed archive, native PITR replay and legacy conversion passed'
