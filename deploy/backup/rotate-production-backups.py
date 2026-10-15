#!/usr/bin/env python3
"""Bound local backup copies without shortening the configured PITR window.

This file is self-contained so a reviewed main-branch copy can be delivered to
the production host on stdin. It never opens PGDATA or reads database rows.
"""

import argparse
import datetime as dt
import fcntl
import json
import os
import re
import stat
import subprocess
import sys
from dataclasses import dataclass


UTC = dt.timezone.utc
MAX_RECORDS = 10000
MAX_OUTPUT = 2 * 1024 * 1024
PROJECT = "maxposty-backend"
BASE_ROOT = "/var/lib/postgresql/base-backups"
WAL_ROOT = "/var/lib/postgresql/wal-archive"
STAMP = r"[0-9]{8}T[0-9]{6}Z"
LOGICAL = re.compile(rf"(postgres|media|backup)-({STAMP})(\.dump|\.tar\.gz|\.sha256)\Z")
LOGICAL_TEMPORARY = re.compile(rf"(postgres|media|backup)-({STAMP})(\.dump|\.tar\.gz|\.sha256)\.tmp\Z")
BASE = re.compile(rf"base-({STAMP})\Z")
WAL = re.compile(r"[0-9A-F]{24}\Z")
WAL_ARCHIVE = re.compile(r"([0-9A-F]{24})(?:\.gz)?\Z")


class RetentionError(Exception):
    def __init__(self, code):
        self.code = code
        super().__init__(code)


def emit(event, **fields):
    print(json.dumps({"event": event, **fields}, sort_keys=True), flush=True)


def timestamp(value):
    try:
        parsed = dt.datetime.strptime(value, "%Y%m%dT%H%M%SZ").replace(tzinfo=UTC)
    except ValueError:
        return None
    return parsed if parsed.strftime("%Y%m%dT%H%M%SZ") == value else None


def utc_now():
    return dt.datetime.now(UTC)


@dataclass(frozen=True)
class Record:
    name: str
    device: int
    inode: int
    size: int
    kind: str


def identity(info):
    return info.st_dev, info.st_ino


def bounded_file(path, maximum=1024 * 1024):
    try:
        fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
        with os.fdopen(fd, "rb") as handle:
            info = os.fstat(handle.fileno())
            if not stat.S_ISREG(info.st_mode) or info.st_size > maximum:
                raise RetentionError("invalid_release_metadata")
            data = handle.read(maximum + 1)
    except OSError:
        raise RetentionError("release_metadata_unavailable") from None
    if len(data) > maximum:
        raise RetentionError("invalid_release_metadata")
    try:
        return data.decode("utf-8")
    except UnicodeDecodeError:
        raise RetentionError("invalid_release_metadata") from None


def accepted_release(installation):
    installation = os.path.abspath(installation)
    if installation == "/" or os.path.realpath(installation) != installation:
        raise RetentionError("invalid_installation_path")
    try:
        info = os.lstat(installation)
        if not stat.S_ISDIR(info.st_mode):
            raise RetentionError("invalid_installation_path")
        current = os.path.join(installation, "current")
        if not stat.S_ISLNK(os.lstat(current).st_mode):
            raise RetentionError("accepted_release_unavailable")
        release = os.path.realpath(current)
        release_name = os.path.basename(release)
        if (not re.fullmatch(r"[0-9a-f]{40}", release_name)
                or os.path.dirname(release) != os.path.join(installation, "releases")
                or os.path.realpath(os.path.dirname(release)) != os.path.dirname(release)
                or not stat.S_ISDIR(os.lstat(release).st_mode)):
            raise RetentionError("invalid_accepted_release")
    except OSError:
        raise RetentionError("accepted_release_unavailable") from None
    env_file = os.path.join(release, ".env.production")
    selected = {}
    # Only these public numeric values and the bootstrap guard are retained.
    # No other environment-file value is parsed into an output or child argv.
    for line in bounded_file(env_file).splitlines():
        key, separator, value = line.partition("=")
        if separator and key in {"BACKUP_RETENTION_DAYS", "PITR_RETENTION_DAYS", "AUTH_BOOTSTRAP_MODE"}:
            if key in selected:
                raise RetentionError("invalid_retention_configuration")
            selected[key] = value
    if selected.get("AUTH_BOOTSTRAP_MODE") != "false":
        raise RetentionError("production_release_required")
    release_values = bounded_file(os.path.join(release, ".release"), 8192).splitlines()
    image_values = [line.partition("=")[2] for line in release_values if line.startswith("BACKEND_IMAGE=")]
    if len(image_values) != 1 or not re.fullmatch(r"ghcr\.io/[a-z0-9._/-]+@sha256:[0-9a-f]{64}", image_values[0]):
        raise RetentionError("invalid_accepted_image")
    compose_file = os.path.join(release, "deploy", "compose.production.yaml")
    if not os.path.isfile(compose_file) or os.path.realpath(compose_file) != compose_file:
        raise RetentionError("invalid_compose_file")
    return installation, release, selected


def days(value, maximum):
    if not isinstance(value, str) or not re.fullmatch(r"[1-9][0-9]{0,2}", value) or int(value) > maximum:
        raise RetentionError("invalid_retention_configuration")
    return int(value)


def acquire_lock(installation, inherited_fd=None):
    path = os.path.join(installation, ".deploy.lock")
    try:
        fd = os.open(path, os.O_RDWR | os.O_NOFOLLOW | os.O_NONBLOCK)
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode) or identity(info) != identity(os.lstat(path)):
            raise RetentionError("invalid_deployment_lock")
        if inherited_fd is not None:
            inherited = os.fstat(inherited_fd)
            if not stat.S_ISREG(inherited.st_mode) or identity(inherited) != identity(info):
                raise RetentionError("invalid_inherited_lock")
            os.close(fd)
            fd = inherited_fd
        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        return fd
    except BlockingIOError:
        raise RetentionError("deployment_lock_busy") from None
    except OSError:
        raise RetentionError("deployment_lock_unavailable") from None


def logical_inventory(directory, now):
    try:
        fd = os.open(directory, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    except FileNotFoundError:
        return None, [], []
    except OSError:
        raise RetentionError("invalid_backup_directory") from None
    records, ignored = [], []
    try:
        names = os.listdir(fd)
        if len(names) > MAX_RECORDS:
            raise RetentionError("inventory_limit_exceeded")
        for name in names:
            info = os.stat(name, dir_fd=fd, follow_symlinks=False)
            matched = LOGICAL.fullmatch(name)
            expected = {"postgres": ".dump", "media": ".tar.gz", "backup": ".sha256"}
            instant = timestamp(matched[2]) if matched else None
            if (not matched or expected[matched[1]] != matched[3] or instant is None
                    or instant > now + dt.timedelta(minutes=5)
                    or not stat.S_ISREG(info.st_mode) or info.st_size <= 0):
                ignored.append(name)
                continue
            if matched[1] == "backup" and not completed_manifest(fd, name, info, matched[2]):
                ignored.append(name)
                continue
            records.append(Record(name, info.st_dev, info.st_ino, info.st_size, matched[1]))
        return fd, records, ignored
    except Exception:
        os.close(fd)
        raise


def completed_manifest(directory_fd, name, expected_info, stamp):
    if expected_info.st_size > 1024:
        return False
    try:
        fd = os.open(name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=directory_fd)
        with os.fdopen(fd, "rb") as handle:
            current = os.fstat(handle.fileno())
            if (not stat.S_ISREG(current.st_mode) or identity(current) != identity(expected_info)
                    or current.st_size != expected_info.st_size):
                return False
            data = handle.read(1025)
    except OSError:
        return False
    try:
        lines = data.decode("ascii").splitlines()
    except UnicodeDecodeError:
        return False
    if len(lines) != 2 or not data.endswith(b"\n"):
        return False
    expected_names = {f"postgres-{stamp}.dump", f"media-{stamp}.tar.gz"}
    names = []
    for line in lines:
        matched = re.fullmatch(r"[0-9a-f]{64}  ([A-Za-z0-9.-]+)", line)
        if not matched:
            return False
        names.append(matched[1])
    return set(names) == expected_names and len(set(names)) == 2


def logical_plan(records, ignored=(), protected_generation=None):
    groups = {}
    for record in records:
        stamp = LOGICAL.fullmatch(record.name)[2]
        groups.setdefault(stamp, {})[record.kind] = record
    # A sidecar that is empty, a symlink or otherwise invalid makes the whole
    # generation partial. Never reinterpret its dump as a standalone copy.
    for name in ignored:
        matched = LOGICAL.fullmatch(name) or LOGICAL_TEMPORARY.fullmatch(name)
        if matched:
            groups.setdefault(matched[2], {})["partial"] = None
    scheduled = sorted(stamp for stamp, group in groups.items()
                       if set(group) == {"postgres", "media", "backup"})
    standalone = sorted(stamp for stamp, group in groups.items() if set(group) == {"postgres"})
    obsolete = [stamp for stamp in scheduled[:-2] + standalone[:-2] if stamp != protected_generation]
    return [record for stamp in sorted(obsolete) for record in groups[stamp].values()]


def remove_logical(directory, fd, records):
    if fd is None:
        return 0, 0
    try:
        if identity(os.fstat(fd)) != identity(os.lstat(directory)) or os.path.islink(directory):
            raise RetentionError("backup_directory_changed")
        # Check the entire plan before deleting any member of a generation.
        for record in records:
            info = os.stat(record.name, dir_fd=fd, follow_symlinks=False)
            if (not stat.S_ISREG(info.st_mode) or identity(info) != (record.device, record.inode)
                    or info.st_size != record.size):
                raise RetentionError("backup_changed_during_rotation")
        count, reclaimed = 0, 0
        for record in records:
            info = os.stat(record.name, dir_fd=fd, follow_symlinks=False)
            if (not stat.S_ISREG(info.st_mode) or identity(info) != (record.device, record.inode)
                    or info.st_size != record.size):
                raise RetentionError("backup_changed_during_rotation")
            os.unlink(record.name, dir_fd=fd)
            count += 1
            reclaimed += record.size
        return count, reclaimed
    except OSError:
        raise RetentionError("logical_rotation_failed") from None


def physical_plan(records, now, retention_days, start_times=None, protected_generation=None):
    bases = sorted(record.name for record in records if record.kind == "base")
    if not bases:
        return [], None
    cutoff = now - dt.timedelta(days=retention_days)
    start_times = start_times or {name: timestamp(BASE.fullmatch(name)[1]) for name in bases}
    if set(start_times) != set(bases) or any(not isinstance(value, dt.datetime) or value.tzinfo is None
                                            for value in start_times.values()):
        raise RetentionError("invalid_pitr_anchor")
    boundary = [name for name in bases if start_times[name] <= cutoff]
    anchor = max(boundary, key=lambda name: start_times[name]) if boundary else min(bases, key=lambda name: start_times[name])
    # Keep the stepping stones inside the recovery window. Keeping only the
    # current boundary and latest two makes tomorrow's boundary unavailable,
    # pinning the WAL archive to an increasingly old base until a full window
    # has passed. A failed or repeated same-day backup must not create that gap.
    keep = {name for name in bases if start_times[name] >= cutoff} | set(bases[-2:]) | {anchor}
    if protected_generation and "base-" + protected_generation in bases:
        keep.add("base-" + protected_generation)
    oldest = min(keep, key=lambda name: start_times[name])
    return [record for record in records if record.kind == "base" and record.name not in keep], oldest


def wal_plan(records, anchor_segment):
    if not WAL.fullmatch(anchor_segment):
        raise RetentionError("invalid_pitr_anchor")
    obsolete = []
    for record in records:
        if record.kind != "wal":
            continue
        matched = WAL_ARCHIVE.fullmatch(record.name)
        if not matched:
            raise RetentionError("invalid_rotation_plan")
        segment = matched[1]
        if segment[:8] == anchor_segment[:8] and segment[8:] < anchor_segment[8:]:
            obsolete.append(record)
    return obsolete


def wal_anchor(records, obsolete_bases, labels, chronological_anchor):
    if chronological_anchor is None:
        return None
    obsolete = {record.name for record in obsolete_bases}
    timeline = labels[chronological_anchor][0][:8]
    retained = [record.name for record in records if record.kind == "base" and record.name not in obsolete
                and labels[record.name][0][:8] == timeline]
    # Wall-clock time can move backward, while WAL positions continue forward.
    # Every retained base on this timeline must keep its required START WAL.
    return min(retained, key=lambda name: labels[name][0][8:])


def parse_label(label):
    segments = re.findall(r"^START WAL LOCATION: [0-9A-F]+/[0-9A-F]+ \(file ([0-9A-F]{24})\)$", label, re.MULTILINE)
    starts = re.findall(r"^START TIME: (.+)$", label, re.MULTILINE)
    if len(segments) != 1 or len(starts) != 1:
        raise RetentionError("invalid_pitr_anchor")
    matched = re.fullmatch(r"([0-9]{4}-[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2}:[0-9]{2}(?:\.[0-9]{1,6})?) (UTC|GMT|[+-][0-9]{2}(?::?[0-9]{2})?)", starts[0])
    if not matched:
        raise RetentionError("invalid_pitr_anchor")
    zone = "+00:00" if matched[2] in {"UTC", "GMT"} else matched[2]
    if re.fullmatch(r"[+-][0-9]{2}", zone):
        zone += ":00"
    elif re.fullmatch(r"[+-][0-9]{4}", zone):
        zone = zone[:3] + ":" + zone[3:]
    try:
        started = dt.datetime.fromisoformat(matched[1] + zone).astimezone(UTC)
    except ValueError:
        raise RetentionError("invalid_pitr_anchor") from None
    return segments[0], started


class DockerBackups:
    def __init__(self, installation, release):
        self.compose = ["docker", "compose", "--project-name", PROJECT,
                        "--env-file", os.path.join(release, ".env.production"),
                        "--env-file", os.path.join(release, ".release"),
                        "--file", os.path.join(release, "deploy", "compose.production.yaml")]
        container = self.command(self.compose + ["ps", "-q", "postgres"], 20).strip()
        if not re.fullmatch(r"[0-9a-f]{64}", container):
            raise RetentionError("postgres_container_unavailable")
        self.container = container
        # Do not inspect Config.Env: it contains production credentials.
        metadata = self.command(["docker", "inspect", "--format",
                                 '{"labels":{{json .Config.Labels}},"mounts":{{json .Mounts}},"running":{{json .State.Running}}}',
                                 container], 20)
        try:
            metadata = json.loads(metadata)
            labels = metadata["labels"]
            mounts = metadata["mounts"]
            if (metadata["running"] is not True or labels.get("com.docker.compose.project") != PROJECT
                    or labels.get("com.docker.compose.service") != "postgres"):
                raise RetentionError("unexpected_postgres_container")
            expected = {"/var/lib/postgresql": "postgres-data", BASE_ROOT: "postgres-base-backups", WAL_ROOT: "postgres-wal-archive"}
            for destination, volume in expected.items():
                matched = [mount for mount in mounts if mount.get("Destination") == destination]
                if (len(matched) != 1 or matched[0].get("Type") != "volume"
                        or matched[0].get("Name") != PROJECT + "_" + volume or matched[0].get("RW") is not True):
                    raise RetentionError("unexpected_postgres_volumes")
        except (ValueError, KeyError, TypeError):
            raise RetentionError("invalid_postgres_metadata") from None

    @staticmethod
    def command(arguments, timeout=30, input_data=None):
        try:
            result = subprocess.run(arguments, input=input_data, stdout=subprocess.PIPE,
                                    stderr=subprocess.DEVNULL, timeout=timeout, check=False)
        except (OSError, subprocess.TimeoutExpired):
            raise RetentionError("docker_operation_unavailable") from None
        if result.returncode != 0:
            raise RetentionError("docker_operation_failed")
        if len(result.stdout) > MAX_OUTPUT:
            raise RetentionError("inventory_limit_exceeded")
        try:
            return result.stdout.decode("ascii")
        except UnicodeDecodeError:
            raise RetentionError("invalid_container_inventory") from None

    def execute(self, arguments, timeout=30, input_data=None):
        return self.command(["docker", "exec", "-i", self.container, *arguments], timeout, input_data)

    def inventory(self, now):
        script = r'''
set -eu
for root in /var/lib/postgresql/base-backups /var/lib/postgresql/wal-archive; do
  test -d "$root" && test ! -L "$root"
  printf 'root\t%s\t' "$root"
  stat -c '%d:%i' "$root" | tr ':' '\t'
done
count=0
for path in /var/lib/postgresql/base-backups/base-*; do
  test -d "$path" && test ! -L "$path" || continue
  name=${path##*/}
  printf '%s\n' "$name" | grep -Eq '^base-[0-9]{8}T[0-9]{6}Z$' || continue
  count=$((count + 1)); test "$count" -le 10000
  printf 'base\t%s\t' "$name"
  stat -c '%d:%i' "$path" | tr ':\n' '\t'
  du -sk "$path" | awk '{printf "%.0f\n", $1 * 1024}'
done
for path in /var/lib/postgresql/wal-archive/*; do
  test -f "$path" && test ! -L "$path" || continue
  name=${path##*/}
  printf '%s\n' "$name" | grep -Eq '^[0-9A-F]{24}(\.gz)?$' || continue
  count=$((count + 1)); test "$count" -le 10000
  before=$(stat -c '%d:%i:%s' "$path")
  case "$name" in
    *.gz)
      # Published archives are immutable. Test the complete gzip stream before
      # using its canonical segment; a partial/corrupt gzip cannot hide a WAL
      # conflict or allow any physical cleanup to proceed.
      gzip -t "$path"
      raw=${path%.gz}
      if test -e "$raw" || test -L "$raw"; then
        test -f "$raw" && test ! -L "$raw"
        raw_before=$(stat -c '%d:%i:%s' "$raw")
        gzip -dc "$path" | cmp -s "$raw" -
        test "$(stat -c '%d:%i:%s' "$raw")" = "$raw_before"
      fi
      test ! -L "$path" && test "$(stat -c '%d:%i:%s' "$path")" = "$before"
      ;;
    *)
      # Do not silently accept a canonical raw/gzip pair when the gzip entry
      # is a symlink or a directory that the regular-file scan would ignore.
      if test -e "$path.gz" || test -L "$path.gz"; then
        test -f "$path.gz" && test ! -L "$path.gz"
      fi
      ;;
  esac
  printf 'wal\t%s\t' "$name"
  printf '%s\n' "$before" | tr ':' '\t'
done
'''
        # Integrity checks stream decompressed bytes inside the container;
        # neither WAL content nor an unbounded payload enters this process.
        # A timed 16 MiB segment every five minutes means tens of GiB must be
        # decompressed even when the stored archive is small. Allow a bounded
        # integrity-check budget distinct from short metadata probes.
        output = self.execute(["sh", "-ec", script], 900)
        records, roots = [], {}
        for line in output.splitlines():
            fields = line.split("\t")
            if len(fields) == 4 and fields[0] == "root" and fields[1] in {BASE_ROOT, WAL_ROOT}:
                if fields[1] in roots or not all(field.isdigit() for field in fields[2:]):
                    raise RetentionError("invalid_container_inventory")
                roots[fields[1]] = (int(fields[2]), int(fields[3]))
            elif (len(fields) == 5 and fields[0] in {"base", "wal"}
                  and all(re.fullmatch(r"[0-9]+", field) for field in fields[2:])):
                kind, name = fields[:2]
                if kind == "base":
                    match = BASE.fullmatch(name)
                    instant = timestamp(match[1]) if match else None
                    if instant is None:
                        continue
                elif not WAL_ARCHIVE.fullmatch(name):
                    raise RetentionError("invalid_container_inventory")
                records.append(Record(name, int(fields[2]), int(fields[3]), int(fields[4]), kind))
            else:
                raise RetentionError("invalid_container_inventory")
            if len(records) > MAX_RECORDS:
                raise RetentionError("inventory_limit_exceeded")
        if set(roots) != {BASE_ROOT, WAL_ROOT} or len({(r.kind, r.name) for r in records}) != len(records):
            raise RetentionError("invalid_container_inventory")
        self.roots = roots
        return records

    def label(self, name):
        if not BASE.fullmatch(name):
            raise RetentionError("invalid_pitr_anchor")
        path = BASE_ROOT + "/" + name
        label = self.execute(["sh", "-ec", 'test ! -L "$1/backup_label" && test -f "$1/backup_label" && test "$(stat -c %s "$1/backup_label")" -le 8192 && cat "$1/backup_label"', "sh", path])
        return parse_label(label)

    def verify(self, name):
        if not BASE.fullmatch(name):
            raise RetentionError("invalid_pitr_anchor")
        self.execute(["pg_verifybackup", BASE_ROOT + "/" + name], 120)

    def remove(self, records):
        if not records:
            return 0, 0
        # Names and identities are passed on stdin, never interpolated as code.
        # The database's live data/pg_wal trees cannot enter this fixed program.
        lines = []
        for record in records:
            matcher = BASE if record.kind == "base" else WAL_ARCHIVE
            if record.kind not in {"base", "wal"} or not matcher.fullmatch(record.name):
                raise RetentionError("invalid_rotation_plan")
            lines.append(f"{record.kind}\t{record.name}\t{record.device}\t{record.inode}\t{record.size}\n")
        base_id = ":".join(map(str, self.roots[BASE_ROOT]))
        wal_id = ":".join(map(str, self.roots[WAL_ROOT]))
        script = r'''
set -eu
test ! -L /var/lib/postgresql/base-backups
test ! -L /var/lib/postgresql/wal-archive
test "$(stat -c '%d:%i' /var/lib/postgresql/base-backups)" = "$1"
test "$(stat -c '%d:%i' /var/lib/postgresql/wal-archive)" = "$2"
tab=$(printf '\t')
while IFS="$tab" read -r kind name device inode bytes; do
  case "$kind" in
    base)
      printf '%s\n' "$name" | grep -Eq '^base-[0-9]{8}T[0-9]{6}Z$'
      path=/var/lib/postgresql/base-backups/$name
      test -d "$path" && test ! -L "$path"
      test "$(stat -c '%d:%i' "$path")" = "$device:$inode"
      rm -rf -- "$path"
      ;;
    wal)
      printf '%s\n' "$name" | grep -Eq '^[0-9A-F]{24}(\.gz)?$'
      path=/var/lib/postgresql/wal-archive/$name
      test -f "$path" && test ! -L "$path"
      test "$(stat -c '%d:%i:%s' "$path")" = "$device:$inode:$bytes"
      rm -f -- "$path"
      ;;
    *) exit 1 ;;
  esac
done
'''
        self.execute(["sh", "-ec", script, "sh", base_id, wal_id], 90, "".join(lines).encode("ascii"))
        return len(records), sum(record.size for record in records)


def run(arguments):
    installation, release, configuration = accepted_release(arguments.installation_dir)
    lock_fd = acquire_lock(installation, arguments.lock_fd)
    backup_dir = os.path.join(installation, "backups")
    now = utc_now()
    logical_fd = None
    try:
        # The deployment could change current while we were acquiring its lock.
        # Read the accepted release and public policy again under exclusivity.
        installation, release, configuration = accepted_release(installation)
        logical_days = days(arguments.logical_retention_days or configuration.get("BACKUP_RETENTION_DAYS"), 365)
        pitr_days = days(arguments.pitr_retention_days or configuration.get("PITR_RETENTION_DAYS"), 90)
        logical_fd, logical, ignored = logical_inventory(backup_dir, now)
        obsolete = logical_plan(logical, ignored, arguments.protect_generation)
        emit("inventory", scope="logical", files=len(logical), bytes=sum(r.size for r in logical),
             removable_files=len(obsolete), removable_bytes=sum(r.size for r in obsolete),
             ignored_entries=len(ignored), local_generations_per_kind=2, configured_retention_days=logical_days)
        if arguments.mode != "inspect":
            removed, reclaimed = remove_logical(backup_dir, logical_fd, obsolete)
            emit("rotation", scope="logical", removed_files=removed, reclaimed_bytes=reclaimed)
        if arguments.mode != "cleanup_logical":
            docker = DockerBackups(installation, release)
            physical = docker.inventory(now)
            clock_skew = any(record.kind == "base" and timestamp(BASE.fullmatch(record.name)[1]) > now + dt.timedelta(minutes=5)
                             for record in physical)
            if clock_skew and arguments.mode == "cleanup":
                emit("inventory", scope="pitr", bases=sum(r.kind == "base" for r in physical),
                     archived_segments=sum(r.kind == "wal" for r in physical), bytes=sum(r.size for r in physical),
                     physical_clock_skew=True)
                raise RetentionError("backup_clock_skew")
            labels = {record.name: docker.label(record.name) for record in physical if record.kind == "base"}
            obsolete_bases, anchor = physical_plan(physical, now, pitr_days,
                                                   {name: value[1] for name, value in labels.items()},
                                                   arguments.protect_generation)
            wal_anchor_name = wal_anchor(physical, obsolete_bases, labels, anchor)
            segment = labels[wal_anchor_name][0] if wal_anchor_name else None
            obsolete_wal = wal_plan(physical, segment) if segment else []
            emit("inventory", scope="pitr", bases=sum(r.kind == "base" for r in physical),
                 archived_segments=sum(r.kind == "wal" for r in physical), bytes=sum(r.size for r in physical),
                 removable_bases=len(obsolete_bases), removable_archived_segments=len(obsolete_wal),
                 configured_retention_days=pitr_days, retained_bases=sum(r.kind == "base" for r in physical) - len(obsolete_bases),
                 anchor_available=anchor is not None,
                 physical_clock_skew=clock_skew)
            if arguments.mode == "cleanup":
                if anchor:
                    docker.verify(anchor)
                    if wal_anchor_name != anchor:
                        docker.verify(wal_anchor_name)
                    # Re-scan after verification: never delete a concurrently
                    # replaced directory, and fail closed if inventory changed.
                    refreshed = {(r.kind, r.name): r for r in docker.inventory(now)}
                    planned = obsolete_bases + obsolete_wal + [r for r in physical if r.name in {anchor, wal_anchor_name}]
                    if any(refreshed.get((r.kind, r.name)) != r for r in planned):
                        raise RetentionError("physical_backups_changed_during_rotation")
                    if any(docker.label(name) != labels[name] for name in {anchor, wal_anchor_name}):
                        raise RetentionError("physical_backups_changed_during_rotation")
                    removed, reclaimed = docker.remove(obsolete_bases + obsolete_wal)
                    emit("rotation", scope="pitr", removed_entries=removed, reclaimed_bytes=reclaimed,
                         retained_bases=sum(r.kind == "base" for r in physical) - len(obsolete_bases))
                else:
                    emit("rotation", scope="pitr", removed_entries=0, reclaimed_bytes=0, retained_bases=0)
        free = os.statvfs(installation)
        emit("complete", mode=arguments.mode, filesystem_free_bytes=free.f_bavail * free.f_frsize)
    finally:
        if logical_fd is not None:
            os.close(logical_fd)
        # An inherited descriptor belongs to the caller. Closing our copy at
        # process exit leaves its open-file-description lock in place.
        if arguments.lock_fd is None:
            os.close(lock_fd)


def main():
    parser = argparse.ArgumentParser(description="Inspect or rotate recognized MaxPosty local backups")
    parser.add_argument("installation_dir")
    parser.add_argument("mode", choices=["inspect", "cleanup", "cleanup_logical"], default="inspect", nargs="?")
    parser.add_argument("--lock-fd", type=int)
    parser.add_argument("--logical-retention-days")
    parser.add_argument("--pitr-retention-days")
    parser.add_argument("--protect-generation")
    arguments = parser.parse_args()
    if arguments.lock_fd is not None and arguments.lock_fd < 3:
        emit("blocked", reason="invalid_inherited_lock")
        return 1
    if arguments.protect_generation is not None and timestamp(arguments.protect_generation) is None:
        emit("blocked", reason="invalid_protected_generation")
        return 1
    try:
        run(arguments)
    except RetentionError as error:
        emit("blocked", reason=error.code)
        return 1
    except (OSError, ValueError):
        emit("blocked", reason="retention_operation_failed")
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
