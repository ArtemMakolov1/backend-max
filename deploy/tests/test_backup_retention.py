#!/usr/bin/env python3
"""Deletion bounds, recovery-anchor safety and failed-upload regression tests."""

import argparse
import contextlib
import datetime as dt
import fcntl
import gzip
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest import mock


ROOT = Path(__file__).resolve().parents[2]
HELPER = ROOT / "deploy/backup/rotate-production-backups.py"
SPEC = importlib.util.spec_from_file_location("backup_retention", HELPER)
retention = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = retention
SPEC.loader.exec_module(retention)
NOW = dt.datetime(2026, 10, 13, 12, tzinfo=dt.timezone.utc)


def base(day, index=None):
    return retention.Record(f"base-202610{day:02}T120000Z", 1, index or day, 1024, "base")


def wal(segment, timeline=1, compressed=False):
    suffix = ".gz" if compressed else ""
    return retention.Record(f"{timeline:08X}00000000{segment:08X}" + suffix,
                            2, segment + (10000 if compressed else 0), 16, "wal")


class Fixture:
    def setUp(self):
        self.clock = mock.patch.object(retention, "utc_now", return_value=NOW)
        self.clock.start()
        self.addCleanup(self.clock.stop)
        self.temporary = tempfile.TemporaryDirectory()
        self.directory = Path(self.temporary.name).resolve()
        self.installation = self.directory / "backend"
        self.installation.mkdir()
        (self.installation / ".deploy.lock").touch(mode=0o600)
        self.backups = self.installation / "backups"
        self.backups.mkdir()
        self.release = self.installation / "releases" / ("a" * 40)
        (self.release / "deploy").mkdir(parents=True)
        (self.installation / "current").symlink_to("releases/" + "a" * 40)
        (self.release / "deploy/compose.production.yaml").write_text("name: maxposty-backend\n")
        (self.release / ".release").write_text("BACKEND_IMAGE=ghcr.io/example/backend@sha256:" + "b" * 64 + "\n")
        (self.release / ".env.production").write_text(
            "AUTH_BOOTSTRAP_MODE=false\nBACKUP_RETENTION_DAYS=14\nPITR_RETENTION_DAYS=7\n"
            "UNRELATED_PRIVATE_VALUE=must-never-appear-in-output\nSMTP_FROM_NAME=Тест\n", encoding="utf-8")

    def tearDown(self):
        self.temporary.cleanup()

    def files(self, day, scheduled=False):
        stamp = f"202610{day:02}T000000Z"
        names = [f"postgres-{stamp}.dump"]
        if scheduled:
            names += [f"media-{stamp}.tar.gz", f"backup-{stamp}.sha256"]
        for name in names:
            (self.backups / name).write_bytes(b"published validated source fixture\n")
        if scheduled:
            digest = hashlib.sha256(b"published validated source fixture\n").hexdigest()
            (self.backups / names[-1]).write_text("".join(f"{digest}  {name}\n" for name in names[:-1]))
        return names

    def arguments(self, mode="cleanup_logical"):
        return argparse.Namespace(installation_dir=str(self.installation), mode=mode,
                                  lock_fd=None, logical_retention_days=None, pitr_retention_days=None,
                                  protect_generation=None)


class LogicalTests(Fixture, unittest.TestCase):
    def test_keeps_two_standalone_and_two_complete_scheduled_generations(self):
        for day in range(1, 7):
            self.files(day, scheduled=day % 2 == 0)
        fd, records, ignored = retention.logical_inventory(str(self.backups), NOW)
        try:
            plan = retention.logical_plan(records, ignored)
            self.assertEqual(len(plan), 4)
            retention.remove_logical(str(self.backups), fd, plan)
        finally:
            os.close(fd)
        self.assertEqual(len(list(self.backups.iterdir())), 8)
        self.assertTrue((self.backups / "postgres-20261003T000000Z.dump").exists())
        self.assertTrue((self.backups / "media-20261004T000000Z.tar.gz").exists())

    def test_many_same_day_deploys_are_bounded_without_waiting_15_days(self):
        for hour in range(12):
            (self.backups / f"postgres-20261013T{hour:02}0000Z.dump").write_bytes(b"verified")
        fd, records, ignored = retention.logical_inventory(str(self.backups), NOW)
        try:
            self.assertEqual(len(retention.logical_plan(records, ignored)), 10)
        finally:
            os.close(fd)

    def test_current_generation_is_preserved_after_clock_moves_backward(self):
        for day in range(1, 7):
            self.files(day, scheduled=day % 2 == 0)
        fd, records, ignored = retention.logical_inventory(str(self.backups), NOW)
        try:
            plan = retention.logical_plan(records, ignored, "20261001T000000Z")
            self.assertNotIn("postgres-20261001T000000Z.dump", [r.name for r in plan])
            self.assertEqual(len(plan), 3)
        finally:
            os.close(fd)

    def test_partial_generation_symlinks_temporary_and_unknown_are_preserved(self):
        for day in range(1, 6):
            self.files(day)
        protected = self.backups / "media-20261001T000000Z.tar.gz"
        protected.write_bytes(b"")
        outside = self.directory / "outside"
        outside.write_bytes(b"keep")
        (self.backups / "media-20261002T000000Z.tar.gz").symlink_to(outside)
        (self.backups / "postgres-20261001T000000Z.dump.tmp").write_bytes(b"partial")
        (self.backups / "manual.dump").write_bytes(b"unknown")
        fd, records, ignored = retention.logical_inventory(str(self.backups), NOW)
        try:
            plan = retention.logical_plan(records, ignored)
            self.assertEqual([r.name for r in plan], ["postgres-20261003T000000Z.dump"])
            retention.remove_logical(str(self.backups), fd, plan)
        finally:
            os.close(fd)
        self.assertEqual(outside.read_bytes(), b"keep")
        self.assertTrue(protected.exists())

    def test_truncated_manifest_and_temporary_siblings_never_count_as_complete(self):
        for day in range(1, 6):
            self.files(day, scheduled=True)
        manifest = self.backups / "backup-20261005T000000Z.sha256"
        manifest.write_text("a" * 64 + "  postgres-20261005T000000Z.dump\n")
        (self.backups / "media-20261004T000000Z.tar.gz.tmp").write_bytes(b"interrupted")
        fd, records, ignored = retention.logical_inventory(str(self.backups), NOW)
        try:
            plan = retention.logical_plan(records, ignored)
            self.assertEqual({r.name for r in plan}, {
                "postgres-20261001T000000Z.dump", "media-20261001T000000Z.tar.gz", "backup-20261001T000000Z.sha256"})
        finally:
            os.close(fd)

    def test_manifest_wrong_timestamp_duplicate_paths_and_extra_lines_are_partial(self):
        names = self.files(1, scheduled=True)
        manifest = self.backups / names[-1]
        variants = ["a" * 64 + "  postgres-20261001T000000Z.dump\n" * 2,
                    "a" * 64 + "  ../postgres-20261001T000000Z.dump\n" + "b" * 64 + "  media-20261001T000000Z.tar.gz\n",
                    "a" * 64 + "  postgres-20261002T000000Z.dump\n" + "b" * 64 + "  media-20261001T000000Z.tar.gz\n",
                    "a" * 64 + "  postgres-20261001T000000Z.dump\n" + "b" * 64 + "  media-20261001T000000Z.tar.gz\nextra\n"]
        for content in variants:
            with self.subTest(content=content):
                manifest.write_text(content)
                fd, records, ignored = retention.logical_inventory(str(self.backups), NOW)
                try:
                    self.assertIn(names[-1], ignored)
                    self.assertEqual(retention.logical_plan(records, ignored), [])
                finally:
                    os.close(fd)

    def test_inode_replacement_is_rejected_before_any_delete(self):
        for day in range(1, 5):
            self.files(day)
        fd, records, ignored = retention.logical_inventory(str(self.backups), NOW)
        try:
            plan = retention.logical_plan(records, ignored)
            target = self.backups / plan[1].name
            replacement = self.backups / "replacement"
            replacement.write_bytes(b"replacement")
            replacement.replace(target)
            with self.assertRaisesRegex(retention.RetentionError, "backup_changed_during_rotation"):
                retention.remove_logical(str(self.backups), fd, plan)
            self.assertTrue((self.backups / plan[0].name).exists())
        finally:
            os.close(fd)

    def test_symlinked_backup_directory_is_rejected(self):
        self.backups.rmdir()
        self.backups.symlink_to(self.directory)
        with self.assertRaises(retention.RetentionError):
            retention.logical_inventory(str(self.backups), NOW)

    def test_replaced_backup_directory_is_rejected(self):
        for day in range(1, 5):
            self.files(day)
        fd, records, ignored = retention.logical_inventory(str(self.backups), NOW)
        try:
            self.backups.rename(self.installation / "retired")
            self.backups.mkdir()
            with self.assertRaisesRegex(retention.RetentionError, "backup_directory_changed"):
                retention.remove_logical(str(self.backups), fd, retention.logical_plan(records, ignored))
        finally:
            os.close(fd)

    def test_invalid_dates_wrong_extensions_empty_future_are_not_deleted(self):
        for name in ["postgres-20260230T000000Z.dump", "media-20261001T000000Z.dump",
                     "postgres-20991231T000000Z.dump", "backup-20261001T000000Z.sha256"]:
            (self.backups / name).write_bytes(b"" if name.startswith("backup") else b"keep")
        fd, records, ignored = retention.logical_inventory(str(self.backups), NOW)
        try:
            self.assertEqual(records, [])
            self.assertEqual(len(ignored), 4)
        finally:
            os.close(fd)

    def test_inspect_is_default_and_prints_no_private_environment_values(self):
        before = sorted(str(path.relative_to(self.installation)) for path in self.installation.rglob("*"))
        with mock.patch.object(retention, "DockerBackups", EmptyDocker), contextlib.redirect_stdout(io.StringIO()) as output:
            retention.run(self.arguments("inspect"))
        self.assertNotIn("must-never", output.getvalue())
        self.assertIn('"mode": "inspect"', output.getvalue())
        self.assertEqual(before, sorted(str(path.relative_to(self.installation)) for path in self.installation.rglob("*")))

    def test_policy_is_reread_under_lock_after_accepted_release_race(self):
        original_lock = retention.acquire_lock

        def acquire_then_change_policy(installation, inherited):
            locked = original_lock(installation, inherited)
            (self.release / ".env.production").write_text("AUTH_BOOTSTRAP_MODE=false\nBACKUP_RETENTION_DAYS=14\nPITR_RETENTION_DAYS=30\n")
            return locked

        with mock.patch.object(retention, "acquire_lock", side_effect=acquire_then_change_policy), \
                mock.patch.object(retention, "DockerBackups", EmptyDocker), contextlib.redirect_stdout(io.StringIO()) as output:
            retention.run(self.arguments("inspect"))
        pitr = [json.loads(line) for line in output.getvalue().splitlines() if json.loads(line).get("scope") == "pitr"]
        self.assertEqual(pitr[0]["configured_retention_days"], 30)

    def test_fifo_metadata_fails_without_waiting_for_a_writer(self):
        fifo = self.directory / "metadata-fifo"
        os.mkfifo(fifo)
        with self.assertRaisesRegex(retention.RetentionError, "invalid_release_metadata"):
            retention.bounded_file(str(fifo))

    def test_logical_only_rotation_requires_no_docker_or_database_access(self):
        for day in range(1, 6):
            self.files(day)
        with mock.patch.object(retention, "DockerBackups", side_effect=AssertionError("must not invoke docker")), contextlib.redirect_stdout(io.StringIO()):
            retention.run(self.arguments())
        self.assertEqual(len(list(self.backups.iterdir())), 2)

    def test_unaccepted_release_and_outside_current_link_are_rejected(self):
        (self.installation / "current").unlink()
        (self.installation / "current").symlink_to(self.directory)
        with self.assertRaisesRegex(retention.RetentionError, "invalid_accepted_release"):
            retention.accepted_release(str(self.installation))

    def test_environment_file_symlink_and_bootstrap_are_rejected(self):
        env = self.release / ".env.production"
        env.write_text("AUTH_BOOTSTRAP_MODE=true\n")
        with self.assertRaisesRegex(retention.RetentionError, "production_release_required"):
            retention.accepted_release(str(self.installation))
        env.unlink()
        env.symlink_to(self.directory / "private")
        with self.assertRaises(retention.RetentionError):
            retention.accepted_release(str(self.installation))

    def test_numeric_fields_are_whitelisted_and_bounded(self):
        for invalid in ["0", "366", "-1", "14;rm", "01", "secret"]:
            with self.subTest(invalid=invalid), self.assertRaises(retention.RetentionError):
                retention.days(invalid, 365)
        self.assertEqual(retention.days("14", 365), 14)

    def test_lock_contention_and_wrong_inherited_descriptor_fail_closed(self):
        lock = retention.acquire_lock(str(self.installation))
        try:
            with self.assertRaisesRegex(retention.RetentionError, "deployment_lock_busy"):
                retention.acquire_lock(str(self.installation))
            unrelated = os.open(self.directory / "unrelated", os.O_CREAT | os.O_RDWR, 0o600)
            try:
                with self.assertRaisesRegex(retention.RetentionError, "invalid_inherited_lock"):
                    retention.acquire_lock(str(self.installation), unrelated)
            finally:
                os.close(unrelated)
        finally:
            os.close(lock)

    def test_inherited_lock_keeps_parent_lock_held(self):
        lock = retention.acquire_lock(str(self.installation))
        try:
            self.assertEqual(retention.acquire_lock(str(self.installation), lock), lock)
            arguments = self.arguments()
            arguments.lock_fd = lock
            with contextlib.redirect_stdout(io.StringIO()):
                retention.run(arguments)
            os.fstat(lock)
            with self.assertRaisesRegex(retention.RetentionError, "deployment_lock_busy"):
                retention.acquire_lock(str(self.installation))
        finally:
            os.close(lock)


class EmptyDocker:
    def __init__(self, *arguments):
        pass

    def inventory(self, now):
        return []


class RecoveryTests(unittest.TestCase):
    def test_keeps_every_base_inside_window_and_nearest_cutoff_anchor(self):
        records = [base(day) for day in range(1, 14)]
        obsolete, anchor = retention.physical_plan(records, NOW, 7)
        kept = {r.name for r in records} - {r.name for r in obsolete}
        self.assertEqual(kept, {base(day).name for day in range(6, 14)})
        self.assertEqual(anchor, base(6).name)

    def test_keeps_oldest_available_when_full_window_does_not_yet_exist(self):
        records = [base(day) for day in range(8, 14)]
        obsolete, anchor = retention.physical_plan(records, NOW, 7)
        self.assertEqual(obsolete, [])
        self.assertEqual(anchor, base(8).name)

    def test_daily_cleanup_advances_boundary_without_pinning_an_old_base(self):
        records = [base(day) for day in range(1, 14)]
        for elapsed in range(10):
            if elapsed:
                records.append(base(13 + elapsed))
            now = NOW + dt.timedelta(days=elapsed)
            obsolete, anchor = retention.physical_plan(records, now, 7)
            self.assertEqual(anchor, base(6 + elapsed).name)
            removed = {record.name for record in obsolete}
            records = [record for record in records if record.name not in removed]
            self.assertEqual({record.name for record in records},
                             {base(day).name for day in range(6 + elapsed, 14 + elapsed)})

    def test_repeated_same_day_backups_keep_tomorrows_boundary_base(self):
        records = [base(day) for day in range(1, 14)]
        for minute in [10, 20, 30]:
            records.append(retention.Record(f"base-20261013T12{minute:02}00Z", 1, 100 + minute, 1024, "base"))
            obsolete, _ = retention.physical_plan(records, NOW + dt.timedelta(minutes=minute), 7)
            records = [record for record in records if record not in obsolete]
        obsolete, anchor = retention.physical_plan(records, NOW + dt.timedelta(days=1), 7)
        self.assertEqual(anchor, base(7).name)
        self.assertEqual({record.name for record in obsolete}, {base(6).name})

    def test_latest_two_survive_sparse_recovery_window(self):
        records = [base(day) for day in [1, 2, 3]]
        obsolete, anchor = retention.physical_plan(records, NOW, 7)
        self.assertEqual([record.name for record in obsolete], [base(1).name])
        self.assertEqual(anchor, base(2).name)

    def test_actual_start_after_cutoff_uses_earlier_base(self):
        records = [base(day) for day in range(1, 14)]
        starts = {r.name: retention.timestamp(retention.BASE.fullmatch(r.name)[1]) for r in records}
        starts[base(6).name] += dt.timedelta(minutes=10)
        obsolete, anchor = retention.physical_plan(records, NOW, 7, starts)
        self.assertEqual(anchor, base(5).name)
        self.assertNotIn(base(5).name, {r.name for r in obsolete})

    def test_current_physical_base_is_protected_after_clock_moves_backward(self):
        records = [base(day) for day in range(1, 14)]
        obsolete, anchor = retention.physical_plan(records, NOW, 7, protected_generation="20261001T120000Z")
        self.assertNotIn(base(1).name, {r.name for r in obsolete})
        self.assertEqual(anchor, base(1).name)

    def test_wal_prunes_only_same_timeline_segments_strictly_before_anchor(self):
        records = [wal(1), wal(9), wal(10), wal(11), wal(1, timeline=2)]
        obsolete = retention.wal_plan(records, wal(10).name)
        self.assertEqual([r.name for r in obsolete], [wal(1).name, wal(9).name])
        with self.assertRaises(retention.RetentionError):
            retention.wal_plan(records, "../../pg_wal")

    def test_mixed_gzip_wal_uses_canonical_segment_for_boundary_and_timeline(self):
        records = [wal(9), wal(9, compressed=True), wal(10), wal(10, compressed=True),
                   wal(11, compressed=True), wal(1, timeline=2, compressed=True)]
        obsolete = retention.wal_plan(records, wal(10).name)
        self.assertEqual([record.name for record in obsolete], [wal(9).name, wal(9, compressed=True).name])
        for invalid in [wal(10).name + ".gz.tmp", "../" + wal(9).name + ".gz", wal(9).name.lower() + ".gz.gz"]:
            with self.subTest(invalid=invalid), self.assertRaises(retention.RetentionError):
                retention.wal_plan([retention.Record(invalid, 1, 2, 3, "wal")], wal(10).name)

    def test_clock_rollback_wal_bound_preserves_every_retained_base_start(self):
        records = [base(day) for day in [1, 6, 12, 13]] + [wal(10), wal(15), wal(20), wal(30)]
        labels = {base(1).name: (wal(30).name, NOW - dt.timedelta(days=12)),
                  base(6).name: (wal(15).name, NOW - dt.timedelta(days=7)),
                  base(12).name: (wal(20).name, NOW - dt.timedelta(days=1)),
                  base(13).name: (wal(40).name, NOW)}
        obsolete, anchor = retention.physical_plan(records, NOW, 7, {n: v[1] for n, v in labels.items()}, "20261001T120000Z")
        self.assertEqual(anchor, base(1).name)
        wal_anchor = retention.wal_anchor(records, obsolete, labels, anchor)
        self.assertEqual(wal_anchor, base(6).name)
        deleted = retention.wal_plan(records, labels[wal_anchor][0])
        self.assertEqual([r.name for r in deleted], [wal(10).name])

    def test_anchor_parser_accepts_utc_and_offset_and_rejects_ambiguous_time(self):
        prefix = f"START WAL LOCATION: 0/A000000 (file {wal(10).name})\n"
        segment, instant = retention.parse_label(prefix + "START TIME: 2026-10-06 12:00:00 UTC\n")
        self.assertEqual((segment, instant), (wal(10).name, NOW - dt.timedelta(days=7)))
        self.assertEqual(retention.parse_label(prefix + "START TIME: 2026-10-06 15:00:00 +03\n")[1], instant)
        for suffix in ["START TIME: yesterday UTC\n", "START TIME: 2026-10-06 12:00:00 CEST\n",
                       "START TIME: 2026-10-06 12:00:00 UTC\nSTART TIME: 2026-10-06 12:00:00 UTC\n"]:
            with self.subTest(suffix=suffix), self.assertRaises(retention.RetentionError):
                retention.parse_label(prefix + suffix)

    def test_invalid_container_project_or_volume_guard(self):
        valid_mounts = [{"Destination": destination, "Name": "maxposty-backend_" + name, "Type": "volume", "RW": True}
                        for destination, name in [("/var/lib/postgresql", "postgres-data"),
                        (retention.BASE_ROOT, "postgres-base-backups"), (retention.WAL_ROOT, "postgres-wal-archive")]]
        metadata = {"labels": {"com.docker.compose.project": "another-project", "com.docker.compose.service": "postgres"},
                    "running": True, "mounts": valid_mounts}
        with mock.patch.object(retention.DockerBackups, "command", side_effect=["a" * 64, json.dumps(metadata)]):
            with self.assertRaisesRegex(retention.RetentionError, "unexpected_postgres_container"):
                retention.DockerBackups("/safe", "/safe/releases/" + "a" * 40)
        metadata["labels"]["com.docker.compose.project"] = "maxposty-backend"
        metadata["mounts"][1]["Name"] = "unrelated-backups"
        with mock.patch.object(retention.DockerBackups, "command", side_effect=["a" * 64, json.dumps(metadata)]):
            with self.assertRaisesRegex(retention.RetentionError, "unexpected_postgres_volumes"):
                retention.DockerBackups("/safe", "/safe/releases/" + "a" * 40)

    def test_inventory_is_bounded_and_ignores_unknown_files(self):
        docker = retention.DockerBackups.__new__(retention.DockerBackups)
        output = f"root\t{retention.BASE_ROOT}\t1\t1\nroot\t{retention.WAL_ROOT}\t2\t2\nbase\t{base(6).name}\t1\t6\t1024\nwal\t{wal(10).name}\t2\t10\t16\n"
        with mock.patch.object(docker, "execute", return_value=output):
            records = docker.inventory(NOW)
        self.assertEqual(len(records), 2)
        with mock.patch.object(docker, "execute", return_value=output + "wal\t../pg_wal\t1\t3\t16\n"):
            with self.assertRaises(retention.RetentionError):
                docker.inventory(NOW)

    def test_no_live_pgdata_deletion_and_only_planned_names_sent_on_stdin(self):
        docker = retention.DockerBackups.__new__(retention.DockerBackups)
        docker.roots = {retention.BASE_ROOT: (1, 1), retention.WAL_ROOT: (2, 2)}
        with mock.patch.object(docker, "execute", return_value="") as execute:
            docker.remove([base(5), wal(9)])
        arguments, timeout, data = execute.call_args.args
        self.assertIn(base(5).name.encode(), data)
        self.assertNotIn("/var/lib/postgresql/data", arguments[2])
        self.assertNotIn("pg_wal", arguments[2])
        self.assertIn("stat -c '%d:%i'", arguments[2])
        invalid = retention.Record("../data", 1, 2, 3, "base")
        with self.assertRaises(retention.RetentionError):
            docker.remove([invalid])


class OrchestrationTests(Fixture, unittest.TestCase):
    def container_shell(self):
        """Run the actual fixed container shell against disposable local paths."""
        base_root = self.directory / "container/base-backups"
        wal_root = self.directory / "container/wal-archive"
        for directory in [base_root, wal_root]:
            directory.mkdir(parents=True)
        binary = self.directory / "shell-bin"
        binary.mkdir()
        stat_binary = binary / "stat"
        stat_binary.write_text("#!" + sys.executable + "\nimport os,sys\nf=sys.argv[2];s=os.stat(sys.argv[3]);"
                               "print(f.replace('%d',str(s.st_dev)).replace('%i',str(s.st_ino)).replace('%s',str(s.st_size)))\n")
        stat_binary.chmod(0o755)
        docker = retention.DockerBackups.__new__(retention.DockerBackups)

        def execute(arguments, timeout=30, input_data=None):
            relocated = [argument.replace(retention.BASE_ROOT, str(base_root)).replace(retention.WAL_ROOT, str(wal_root))
                         for argument in arguments]
            result = subprocess.run(relocated, input=input_data, capture_output=True, timeout=timeout, check=False,
                                    env=dict(os.environ, PATH=str(binary) + os.pathsep + os.environ["PATH"]))
            if result.returncode:
                raise retention.RetentionError("docker_operation_failed")
            return result.stdout.decode("ascii").replace(str(base_root), retention.BASE_ROOT).replace(str(wal_root), retention.WAL_ROOT)

        return docker, execute, base_root, wal_root

    def test_actual_shell_mixed_archives_preserve_anchor_and_remove_only_expired_copies(self):
        docker, execute, _, wal_root = self.container_shell()
        data = b"completed WAL archive fixture" * 100
        for segment, compressed in [(9, False), (9, True), (10, False), (10, True), (11, True)]:
            target = wal_root / wal(segment, compressed=compressed).name
            target.write_bytes(gzip.compress(data) if compressed else data)
        other_timeline = wal_root / wal(1, timeline=2, compressed=True).name
        other_timeline.write_bytes(gzip.compress(data))
        temporary = wal_root / (wal(8, compressed=True).name + ".tmp")
        temporary.write_bytes(b"incomplete gzip")
        history = wal_root / "00000002.history.gz"
        history.write_bytes(b"unknown files are never interpreted or removed")
        with mock.patch.object(docker, "execute", side_effect=execute):
            records = docker.inventory(NOW)
            self.assertEqual(len(records), 6)
            obsolete = retention.wal_plan(records, wal(10).name)
            self.assertEqual({record.name for record in obsolete}, {wal(9).name, wal(9, compressed=True).name})
            count, reclaimed = docker.remove(obsolete)
            self.assertEqual(count, 2)
            self.assertEqual(reclaimed, len(data) + len(gzip.compress(data)))
        self.assertEqual({path.name for path in wal_root.iterdir()},
                         {wal(10).name, wal(10, compressed=True).name, wal(11, compressed=True).name,
                          other_timeline.name, temporary.name, history.name})

    def test_actual_shell_rejects_corrupt_gzip_before_any_cleanup(self):
        for corruption in [b"not gzip", gzip.compress(b"WAL fixture")[:-4]]:
            with self.subTest(corruption=corruption):
                # Each subcase has a separate fixture but exercises the real
                # gzip parser and CRC/trailer checks, not a mocked validator.
                with tempfile.TemporaryDirectory(dir=self.directory) as case:
                    original = self.directory
                    self.directory = Path(case)
                    try:
                        docker, execute, base_root, wal_root = self.container_shell()
                        retained = base_root / base(6).name
                        retained.mkdir()
                        archive = wal_root / wal(9, compressed=True).name
                        archive.write_bytes(corruption)
                        with mock.patch.object(docker, "execute", side_effect=execute):
                            with self.assertRaises(retention.RetentionError):
                                docker.inventory(NOW)
                        self.assertTrue(retained.is_dir())
                        self.assertEqual(archive.read_bytes(), corruption)
                    finally:
                        self.directory = original

    def test_actual_shell_rejects_divergent_raw_and_compressed_same_segment(self):
        docker, execute, _, wal_root = self.container_shell()
        raw = wal_root / wal(9).name
        compressed = wal_root / wal(9, compressed=True).name
        raw.write_bytes(b"first cluster WAL")
        compressed.write_bytes(gzip.compress(b"different cluster WAL"))
        with mock.patch.object(docker, "execute", side_effect=execute):
            with self.assertRaises(retention.RetentionError):
                docker.inventory(NOW)
        self.assertTrue(raw.is_file())
        self.assertTrue(compressed.is_file())

    def test_actual_shell_does_not_follow_same_segment_symlink(self):
        docker, execute, _, wal_root = self.container_shell()
        outside = self.directory / "operator-file"
        outside.write_bytes(b"must not enter the archive inventory")
        raw = wal_root / wal(9).name
        raw.symlink_to(outside)
        (wal_root / wal(9, compressed=True).name).write_bytes(gzip.compress(outside.read_bytes()))
        with mock.patch.object(docker, "execute", side_effect=execute):
            with self.assertRaises(retention.RetentionError):
                docker.inventory(NOW)
        self.assertTrue(raw.is_symlink())
        self.assertEqual(outside.read_bytes(), b"must not enter the archive inventory")

    def test_actual_shell_rejects_raw_pair_with_symlinked_gzip_counterpart(self):
        docker, execute, _, wal_root = self.container_shell()
        outside = self.directory / "operator-file.gz"
        outside.write_bytes(gzip.compress(b"WAL fixture"))
        raw = wal_root / wal(9).name
        raw.write_bytes(b"WAL fixture")
        packed = wal_root / wal(9, compressed=True).name
        packed.symlink_to(outside)
        with mock.patch.object(docker, "execute", side_effect=execute):
            with self.assertRaises(retention.RetentionError):
                docker.inventory(NOW)
        self.assertTrue(raw.is_file())
        self.assertTrue(packed.is_symlink())
        self.assertEqual(gzip.decompress(outside.read_bytes()), b"WAL fixture")

    def test_future_native_base_after_clock_rollback_blocks_physical_and_wal_deletion(self):
        calls = []

        class FutureDocker:
            def __init__(self, *arguments):
                pass

            def inventory(self, now):
                return [retention.Record("base-20991231T120000Z", 1, 1, 1024, "base"), wal(1)]

            def label(self, name):
                calls.append("label")
                raise AssertionError("must block before using a future base")

            def remove(self, records):
                calls.append("remove")

        with mock.patch.object(retention, "DockerBackups", FutureDocker), contextlib.redirect_stdout(io.StringIO()):
            with self.assertRaisesRegex(retention.RetentionError, "backup_clock_skew"):
                retention.run(self.arguments("cleanup"))
        self.assertEqual(calls, [])

    def test_actual_container_shell_inventory_and_deletion_preserve_live_and_unknown_paths(self):
        base_root = self.directory / "container/base-backups"
        wal_root = self.directory / "container/wal-archive"
        live = self.directory / "container/live-data"
        for directory in [base_root, wal_root, live]:
            directory.mkdir(parents=True)
        (live / "important").write_bytes(b"never delete")
        for day in [5, 6, 12, 13]:
            target = base_root / base(day).name
            target.mkdir()
            (target / "PG_VERSION").write_text("18")
        (base_root / base(5).name / "tablespace-link").symlink_to(live)
        (base_root / "base-20261004T120000Z").symlink_to(live)
        (base_root / ".base-20261013T120000Z.tmp").mkdir()
        (base_root / "operator-data").mkdir()
        for segment in [1, 10, 11]:
            (wal_root / wal(segment).name).write_bytes(b"WAL!")
        (wal_root / "00000002.history").write_bytes(b"timeline history")
        (wal_root / (wal(12).name + ".tmp")).write_bytes(b"partial archive")
        (wal_root / wal(2).name).symlink_to(live / "important")
        binary = self.directory / "shell-bin"
        binary.mkdir()
        stat_binary = binary / "stat"
        stat_binary.write_text("#!" + sys.executable + "\nimport os,sys\nf=sys.argv[2];s=os.stat(sys.argv[3]);"
                               "print(f.replace('%d',str(s.st_dev)).replace('%i',str(s.st_ino)).replace('%s',str(s.st_size)))\n")
        stat_binary.chmod(0o755)
        docker = retention.DockerBackups.__new__(retention.DockerBackups)

        def execute(arguments, timeout=30, input_data=None):
            relocated = [argument.replace(retention.BASE_ROOT, str(base_root)).replace(retention.WAL_ROOT, str(wal_root))
                         for argument in arguments]
            result = subprocess.run(relocated, input=input_data, capture_output=True, timeout=timeout, check=False,
                                    env=dict(os.environ, PATH=str(binary) + os.pathsep + os.environ["PATH"]))
            if result.returncode:
                raise retention.RetentionError("docker_operation_failed")
            return result.stdout.decode("ascii").replace(str(base_root), retention.BASE_ROOT).replace(str(wal_root), retention.WAL_ROOT)

        with mock.patch.object(docker, "execute", side_effect=execute):
            records = docker.inventory(NOW)
            self.assertEqual(sum(r.kind == "base" for r in records), 4)
            self.assertEqual(sum(r.kind == "wal" for r in records), 3)
            obsolete_bases, anchor = retention.physical_plan(records, NOW, 7)
            self.assertEqual(anchor, base(6).name)
            obsolete_wal = retention.wal_plan(records, wal(10).name)
            count, _ = docker.remove(obsolete_bases + obsolete_wal)
            self.assertEqual(count, 2)
        self.assertEqual((live / "important").read_bytes(), b"never delete")
        self.assertFalse((base_root / base(5).name).exists())
        self.assertTrue((base_root / ".base-20261013T120000Z.tmp").is_dir())
        self.assertTrue((wal_root / "00000002.history").exists())
        self.assertTrue((wal_root / wal(10).name).exists())

    def test_anchor_verification_precedes_physical_removal_and_failure_blocks_it(self):
        calls = []

        class OfflineDocker:
            def __init__(self, *arguments):
                pass

            def inventory(self, now):
                calls.append("inventory")
                return [base(day) for day in range(1, 14)] + [wal(1), wal(10)]

            def label(self, name):
                return wal(10).name, retention.timestamp(retention.BASE.fullmatch(name)[1])

            def verify(self, name):
                calls.append("verify")

            def remove(self, records):
                calls.append("remove")
                return len(records), sum(r.size for r in records)

        with mock.patch.object(retention, "DockerBackups", OfflineDocker), contextlib.redirect_stdout(io.StringIO()):
            retention.run(self.arguments("cleanup"))
        self.assertLess(calls.index("verify"), calls.index("remove"))
        calls.clear()
        with mock.patch.object(OfflineDocker, "verify", side_effect=retention.RetentionError("docker_operation_failed")), \
                mock.patch.object(retention, "DockerBackups", OfflineDocker), contextlib.redirect_stdout(io.StringIO()):
            with self.assertRaises(retention.RetentionError):
                retention.run(self.arguments("cleanup"))
        self.assertNotIn("remove", calls)

    def test_failed_offsite_hook_still_runs_exit_rotation_with_inherited_lock(self):
        release = self.release
        (release / "deploy/backup").mkdir()
        shutil.copy(ROOT / "deploy/backup/run-scheduled-backup.sh", release / "deploy/backup/run-scheduled-backup.sh")
        validator = release / "deploy/validate-production-env.sh"
        validator.write_text("#!/bin/sh\nexit 0\n")
        validator.chmod(0o755)
        helper = release / "deploy/backup/rotate-production-backups.py"
        helper.write_text("import os,sys\nassert os.fstat(9).st_ino==os.stat(sys.argv[1]+'/.deploy.lock').st_ino\n"
                          "with open(os.environ['TEST_ROTATION_EVENTS'],'a') as h: h.write('rotation\\n')\n")
        (self.installation / "hooks").mkdir()
        hook = self.installation / "hooks/after-backup"
        hook.write_text("#!/bin/sh\nexit 42\n")
        hook.chmod(0o755)
        binary = self.directory / "bin"
        binary.mkdir()
        docker = binary / "docker"
        docker.write_text("""#!/bin/sh
case "$*" in
  *'pg_dump --format=custom'*) printf 'verified dump fixture';;
  *'pg_restore --list'*|*'pg_restore --exit-on-error'*) cat >/dev/null;;
  *'media-backup'*) tar -czf - --files-from /dev/null;;
  *'SELECT pg_walfile_name'*|*'SELECT last_archived_wal'*) printf '00000001000000000000000A\\n';;
  *'SELECT 1'*) printf '1\\n';;
  *) exit 0;;
esac
""")
        docker.chmod(0o755)
        if shutil.which("flock") is None:
            flock = binary / "flock"
            flock.write_text("#!/bin/sh\nexit 0\n")
            flock.chmod(0o755)
        environment = dict(os.environ, PATH=str(binary) + os.pathsep + os.environ["PATH"],
                           TEST_ROTATION_EVENTS=str(self.directory / "events"))
        result = subprocess.run(["bash", str(self.installation / "current/deploy/backup/run-scheduled-backup.sh"),
                                 "example/backend"], input=b"test-short-lived-token-0123456789\n", capture_output=True,
                                env=environment, timeout=20, check=False)
        self.assertEqual(result.returncode, 42, result.stderr.decode())
        self.assertEqual((self.directory / "events").read_text().splitlines(), ["rotation", "rotation"])
        self.assertFalse((self.installation / "runtime/metrics/backup-success.prom").exists())
        self.assertIn("maxposty_backup_last_attempt_success 0", (self.installation / "runtime/metrics/backup-attempt.prom").read_text())
        self.assertFalse(list(self.backups.glob("*.tmp")))


if __name__ == "__main__":
    unittest.main()
