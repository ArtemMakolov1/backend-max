"""Exercise the actual portable PostgreSQL archive/restore programs offline."""
import gzip
from pathlib import Path
import os
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[2]
ARCHIVE = ROOT / "docker/postgres/archive-wal.sh"
RESTORE = ROOT / "docker/postgres/restore-wal.sh"
CONVERT = ROOT / "docker/postgres/compress-legacy-wal.sh"
NAME = "000000010000000000000012"


class WALArchivingTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.directory = Path(self.temporary.name)
        self.archive = self.directory / "archive"
        self.archive.mkdir()
        self.source = self.directory / "input"
        self.content = b"PostgreSQL WAL fixture\0" + bytes(128 * 1024)
        self.source.write_bytes(self.content)

    def run_script(self, script, *arguments, succeeds=True):
        result = subprocess.run([str(script), *map(str, arguments)], capture_output=True, check=False)
        if succeeds:
            self.assertEqual(result.returncode, 0, result.stderr.decode())
        else:
            self.assertNotEqual(result.returncode, 0)
        return result

    def pack(self, name=NAME, content=None):
        target = self.archive / (name + ".gz")
        target.write_bytes(gzip.compress(self.content if content is None else content, mtime=0))
        return target

    def archive_source(self, succeeds=True):
        return self.run_script(ARCHIVE, self.source, NAME, self.archive, succeeds=succeeds)

    def test_new_segment_is_compressed_and_retry_preserves_identical_archive(self):
        self.archive_source()
        packed = self.archive / (NAME + ".gz")
        self.assertEqual(gzip.decompress(packed.read_bytes()), self.content)
        self.assertLess(packed.stat().st_size, len(self.content) // 10)
        self.assertFalse((self.archive / NAME).exists())
        original = (packed.stat().st_ino, packed.stat().st_mtime_ns, packed.read_bytes())
        self.archive_source()
        self.assertEqual(original, (packed.stat().st_ino, packed.stat().st_mtime_ns, packed.read_bytes()))
        self.assertEqual(list(self.archive.iterdir()), [packed])

    def test_different_existing_compressed_archive_is_never_replaced(self):
        packed = self.pack(content=b"different timeline content")
        original = packed.read_bytes()
        self.archive_source(succeeds=False)
        self.assertEqual(packed.read_bytes(), original)

    def test_legacy_raw_retry_and_matching_mixed_archive_are_accepted(self):
        raw = self.archive / NAME
        raw.write_bytes(self.content)
        self.archive_source()
        self.assertFalse((self.archive / (NAME + ".gz")).exists())
        self.pack()
        self.archive_source()
        self.assertEqual(raw.read_bytes(), self.content)

    def test_raw_or_mixed_mismatch_and_corrupt_gzip_fail_closed(self):
        raw = self.archive / NAME
        raw.write_bytes(b"different")
        self.archive_source(succeeds=False)
        raw.write_bytes(self.content)
        packed = self.pack(content=b"different")
        self.archive_source(succeeds=False)
        packed.write_bytes(gzip.compress(self.content)[:-4])
        self.archive_source(succeeds=False)
        self.assertEqual(raw.read_bytes(), self.content)

    def test_history_backup_and_promotion_partial_files_remain_plain_and_idempotent(self):
        for name in ("00000002.history", NAME + ".00000028.backup", NAME + ".partial"):
            with self.subTest(name=name):
                self.run_script(ARCHIVE, self.source, name, self.archive)
                self.run_script(ARCHIVE, self.source, name, self.archive)
                self.assertEqual((self.archive / name).read_bytes(), self.content)
                self.assertFalse((self.archive / (name + ".gz")).exists())
                destination = self.directory / "restored-history"
                self.run_script(RESTORE, name, destination, self.archive)
                self.assertEqual(destination.read_bytes(), self.content)

    def test_rejects_unexpected_names_and_symlinks(self):
        for name in ("../escape", NAME + ".partial.tmp", "", NAME.lower() + "extra"):
            self.run_script(ARCHIVE, self.source, name, self.archive, succeeds=False)
        (self.archive / (NAME + ".gz")).symlink_to(self.source)
        self.archive_source(succeeds=False)
        self.assertEqual(self.source.read_bytes(), self.content)

    def test_restore_reads_raw_gzip_or_matching_both(self):
        raw = self.archive / NAME
        raw.write_bytes(self.content)
        destination = self.directory / "restored"
        for mode in ("raw", "mixed", "gzip"):
            with self.subTest(mode=mode):
                if mode == "mixed":
                    self.pack()
                if mode == "gzip":
                    raw.unlink()
                self.run_script(RESTORE, "--verify", NAME, self.archive)
                self.run_script(RESTORE, NAME, destination, self.archive)
                self.assertEqual(destination.read_bytes(), self.content)

    def test_restore_never_uses_raw_to_hide_corrupt_or_mismatched_gzip(self):
        raw = self.archive / NAME
        raw.write_bytes(self.content)
        packed = self.pack(content=b"different")
        destination = self.directory / "restored"
        self.run_script(RESTORE, NAME, destination, self.archive, succeeds=False)
        self.run_script(RESTORE, "--verify", NAME, self.archive, succeeds=False)
        self.assertFalse(destination.exists())
        packed.write_bytes(gzip.compress(self.content)[:-4])
        self.run_script(RESTORE, NAME, destination, self.archive, succeeds=False)
        self.assertFalse(destination.exists())

    def test_partial_never_substitutes_for_a_completed_restore_segment(self):
        partial = self.archive / (NAME + ".partial")
        partial.write_bytes(self.content)
        destination = self.directory / "completed-restore"
        self.run_script(RESTORE, NAME, destination, self.archive, succeeds=False)
        self.assertFalse(destination.exists())
        self.run_script(CONVERT, self.archive, ARCHIVE)
        self.assertEqual(partial.read_bytes(), self.content)
        self.assertEqual(list(self.archive.iterdir()), [partial])

    def test_converter_verifies_before_unlink_and_is_idempotent(self):
        raw = self.archive / NAME
        raw.write_bytes(self.content)
        history = self.archive / "00000002.history"
        history.write_bytes(b"timeline history")
        partial = self.archive / (NAME + ".tmp")
        partial.write_bytes(b"unfinished")
        result = self.run_script(CONVERT, self.archive, ARCHIVE)
        self.assertIn(b"converted=1", result.stdout)
        self.assertFalse(raw.exists())
        self.assertEqual(gzip.decompress((self.archive / (NAME + ".gz")).read_bytes()), self.content)
        self.assertEqual(history.read_bytes(), b"timeline history")
        self.assertEqual(partial.read_bytes(), b"unfinished")
        result = self.run_script(CONVERT, self.archive, ARCHIVE)
        self.assertIn(b"converted=0", result.stdout)

    def test_converter_preserves_raw_if_existing_gzip_is_different_or_corrupt(self):
        raw = self.archive / NAME
        raw.write_bytes(self.content)
        packed = self.pack(content=b"different")
        self.run_script(CONVERT, self.archive, ARCHIVE, succeeds=False)
        self.assertEqual(raw.read_bytes(), self.content)
        packed.write_bytes(b"invalid gzip")
        self.run_script(CONVERT, self.archive, ARCHIVE, succeeds=False)
        self.assertEqual(raw.read_bytes(), self.content)

    def test_failed_compression_keeps_legacy_source_and_publishes_no_final(self):
        fake_bin = self.directory / "bin"
        fake_bin.mkdir()
        fake_gzip = fake_bin / "gzip"
        fake_gzip.write_text("#!/bin/sh\nprintf partial\nexit 1\n")
        fake_gzip.chmod(0o700)
        raw = self.archive / NAME
        raw.write_bytes(self.content)
        result = subprocess.run([str(CONVERT), str(self.archive), str(ARCHIVE)],
                                env={**os.environ, "PATH": str(fake_bin) + os.pathsep + os.environ["PATH"]},
                                capture_output=True, check=False)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(raw.read_bytes(), self.content)
        self.assertEqual(list(self.archive.iterdir()), [raw])


if __name__ == "__main__":
    unittest.main()
