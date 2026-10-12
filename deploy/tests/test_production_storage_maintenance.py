"""Offline protection and command tests; no Docker daemon or network needed."""
import contextlib
import copy
import datetime
import importlib.util
import io
import json
import sys
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

SCRIPT = Path(__file__).parents[1] / "maintain-production-storage.py"
SPEC = importlib.util.spec_from_file_location("production_storage_maintenance", SCRIPT)
maintenance = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(maintenance)

NOW = datetime.datetime(2026, 10, 13, 10, tzinfo=datetime.timezone.utc)
OLD_ID = "sha256:" + "a" * 64
OTHER_ID = "sha256:" + "b" * 64
CANDIDATE = "ghcr.io/artemmakolov1/backend-max@sha256:" + "c" * 64
OWN_REF = "ghcr.io/artemmakolov1/backend-max@sha256:" + "d" * 64
MONITOR_REF = "ghcr.io/artemmakolov1/maxposty-prometheus@sha256:" + "e" * 64


def image():
    return {"Id": OLD_ID, "RepoTags": ["ghcr.io/artemmakolov1/backend-max:old"],
            "RepoDigests": [OWN_REF], "Created": "2026-10-10T10:00:00Z", "Size": 12345,
            "Config": {"Labels": {"org.opencontainers.image.source": "https://github.com/ArtemMakolov1/backend-max"}}}


class SelectionTests(unittest.TestCase):
    def classify(self, value=None, ids=(), refs=()):
        return maintenance.classify_image(value or image(), set(ids), set(refs), NOW)

    def test_valid_backend_and_monitoring_images_are_eligible(self):
        self.assertEqual(self.classify(), "eligible")
        own = image()
        own["RepoTags"] = []
        own["RepoDigests"] = [MONITOR_REF]
        self.assertEqual(self.classify(own), "eligible")

    def test_all_container_refs_and_current_rollback_candidate_refs_preserved(self):
        self.assertEqual(self.classify(ids=[OLD_ID]), "container_or_release_referenced")
        for reference in (OWN_REF, image()["RepoTags"][0]):
            self.assertEqual(self.classify(refs=[reference]), "release_or_candidate_referenced")
        own = image()
        own["RepoDigests"].append(CANDIDATE)
        self.assertEqual(self.classify(own, refs=[CANDIDATE]), "release_or_candidate_referenced")

    def test_mixed_foreign_tags_and_digests_block_entire_image(self):
        for tag in ("private.example/team:old", "ghcr.io/other/backend-max:old", "ghcr.io/artemmakolov1/unrelated:old"):
            own = image()
            own["RepoTags"].append(tag)
            self.assertEqual(self.classify(own), "foreign_tag")
        for digest in ("private.example/team@sha256:" + "f" * 64, "ghcr.io/artemmakolov1/unrelated@sha256:" + "f" * 64):
            own = image()
            own["RepoDigests"].append(digest)
            self.assertEqual(self.classify(own), "missing_or_foreign_digest")

    def test_no_digest_dangling_local_or_foreign_source_not_eligible(self):
        for digests in ([], None):
            own = image()
            own["RepoDigests"] = digests
            self.assertEqual(self.classify(own), "missing_or_foreign_digest")
        for config in ({}, None, {"Labels": None}, {"Labels": {"org.opencontainers.image.source": "https://github.com/other/backend-max"}}):
            own = image()
            own["Config"] = config
            self.assertEqual(self.classify(own), "foreign_source")

    def test_age_boundary_and_invalid_or_future_timestamps(self):
        own = image()
        own["Created"] = (NOW - datetime.timedelta(hours=24)).isoformat()
        self.assertEqual(self.classify(own), "eligible")
        for delta in (datetime.timedelta(hours=23, minutes=59), datetime.timedelta(hours=-1)):
            own["Created"] = (NOW - delta).isoformat()
            self.assertEqual(self.classify(own), "younger_than_24h")
        for created in ("invalid", None, "2026-10-10T10:00:00"):
            own["Created"] = created
            self.assertNotEqual(self.classify(own), "eligible")

    def test_invalid_image_ids_cannot_become_command_arguments(self):
        for value in ("-f", "sha256:short", "a" * 64, "sha256:" + "f" * 64 + ";touch"):
            own = image()
            own["Id"] = value
            self.assertEqual(self.classify(own), "invalid_image_id")

    def test_image_inspection_without_labels_remains_safe_and_ineligible(self):
        unlabeled = image()
        unlabeled["Config"] = {"Labels": {"org.opencontainers.image.source": None}}
        with patch.object(maintenance, "run", return_value=json.dumps(unlabeled)) as command:
            inspected = maintenance.inspect_image(OLD_ID)
        self.assertEqual(self.classify(inspected), "foreign_source")
        template = command.call_args.args[0][4]
        self.assertIn("{{if .Config}}{{if .Config.Labels}}", template)
        self.assertEqual(command.call_args.kwargs["operation"], "image_inspect")

    def test_failed_command_exposes_only_fixed_operation_and_code(self):
        with self.assertRaises(maintenance.MaintenanceError) as caught:
            maintenance.run([sys.executable, "-c", "import sys; sys.stderr.write('PRIVATE_FIXTURE_ERROR'); sys.exit(1)"], operation="image_inventory")
        self.assertEqual(str(caught.exception), "command_failed")
        self.assertEqual(caught.exception.operation, "image_inventory")
        self.assertNotIn("PRIVATE_FIXTURE_ERROR", str(caught.exception))


class ProtectionTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="maxpostystorage")
        self.installation = Path(self.temp.name)
        (self.installation / "releases").mkdir()
        self.current = self.release("1" * 40, CANDIDATE, MONITOR_REF)
        (self.installation / "current").symlink_to(self.current)
        (self.installation / ".deploy.lock").touch()

    def tearDown(self):
        self.temp.cleanup()

    def release(self, commit, backend, monitor):
        release = self.installation / "releases" / commit
        (release / "deploy").mkdir(parents=True)
        (release / ".release").write_text("BACKEND_IMAGE=" + backend + "\n", encoding="utf-8")
        (release / "deploy" / "compose.production.yaml").write_text("services:\n  monitor:\n    image: " + monitor + "\n", encoding="utf-8")
        return release

    def test_every_retained_release_and_next_metadata_is_protected_without_env_reads(self):
        rollback = self.release("2" * 40, OWN_REF, MONITOR_REF)
        (rollback / ".release.next").write_text("BACKEND_IMAGE=" + CANDIDATE + "\n", encoding="utf-8")
        (rollback / ".env.production").write_text("PRIVATE_PASSWORD=do-not-read\n", encoding="utf-8")
        self.assertEqual(maintenance.release_refs(self.installation), {CANDIDATE, OWN_REF, MONITOR_REF})

    def test_external_current_and_symlink_metadata_fail_closed(self):
        (self.current / ".release").unlink()
        (self.current / ".release").symlink_to(self.current / ".env.production")
        with self.assertRaises((OSError, maintenance.MaintenanceError)):
            maintenance.release_refs(self.installation)
        (self.installation / "current").unlink()
        (self.installation / "current").symlink_to(self.installation)
        with self.assertRaises(maintenance.MaintenanceError):
            maintenance.release_refs(self.installation)

    def test_actual_tagged_digest_compose_pins_preserve_rollback_images(self):
        tagged = "ghcr.io/artemmakolov1/maxposty-grafana:nightly-20261004-prometheus.2@sha256:" + "f" * 64
        self.release("3" * 40, OWN_REF, tagged)
        refs = maintenance.release_refs(self.installation)
        self.assertTrue({tagged, tagged.split("@")[0], "ghcr.io/artemmakolov1/maxposty-grafana@sha256:" + "f" * 64}.issubset(refs))
        rollback_image = image()
        rollback_image["RepoTags"] = [tagged.split("@")[0]]
        rollback_image["RepoDigests"] = ["ghcr.io/artemmakolov1/maxposty-grafana@sha256:" + "f" * 64]
        self.assertEqual(maintenance.classify_image(rollback_image, set(), refs, NOW), "release_or_candidate_referenced")

    def test_stopped_container_ids_are_protected(self):
        container = "f" * 64
        calls = []
        def fake_run(argv, limit=maintenance.MAX_OUTPUT, missing_ok=False, operation="docker_command"):
            calls.append(argv)
            return container + "\n" if argv[1:3] == ["container", "ls"] else OTHER_ID + "\n"
        with patch.object(maintenance, "run", fake_run):
            self.assertEqual(maintenance.container_image_ids(), {OTHER_ID})
        self.assertIn("--all", calls[0])
        self.assertEqual(calls[1], ["docker", "container", "inspect", "--format", "{{.Image}}", container])

    def test_local_oci_platform_id_is_protected_by_resolved_index_reference(self):
        with patch.object(maintenance, "container_image_ids", return_value=set()), \
                patch.object(maintenance, "run", return_value=OTHER_ID + "\n"):
            ids, refs = maintenance.protection(self.installation, CANDIDATE, [image()])
        self.assertIn(OTHER_ID, ids)
        self.assertIn(CANDIDATE, refs)

    def test_missing_candidate_resolution_fails_closed(self):
        with patch.object(maintenance, "container_image_ids", return_value=set()), \
                patch.object(maintenance, "run", return_value=None):
            with self.assertRaisesRegex(maintenance.MaintenanceError, "candidate_image_not_available"):
                maintenance.protection(self.installation, CANDIDATE, [image()])

    def call_main(self, approved=OLD_ID, protection_results=None, remote=True):
        output = io.StringIO()
        commands = []
        def fake_run(argv, limit=maintenance.MAX_OUTPUT, missing_ok=False, operation="docker_command"):
            commands.append(argv)
            return "Deleted\n"
        protections = protection_results or [(set(), set()), (set(), set())]
        with contextlib.redirect_stdout(output), \
                patch.object(maintenance, "report_storage"), \
                patch.object(maintenance, "image_ids", return_value=[OLD_ID]), \
                patch.object(maintenance, "inspect_image", side_effect=lambda _: copy.deepcopy(image())), \
                patch.object(maintenance, "protection", side_effect=protections), \
                patch.object(maintenance, "public_digest_exists", return_value=remote), \
                patch.object(maintenance, "run", side_effect=fake_run):
            maintenance.main([str(self.installation), "cleanup_unused_images", CANDIDATE, approved])
        return commands, [json.loads(line) for line in output.getvalue().splitlines()]

    def test_approved_verified_image_removed_only_by_no_force_argv(self):
        commands, records = self.call_main()
        self.assertEqual(commands, [["docker", "image", "rm", "--no-prune", OLD_ID]])
        self.assertTrue(any(record["event"] == "image_removed" for record in records))

    def test_revalidation_new_stopped_container_reference_prevents_removal(self):
        with self.assertRaisesRegex(maintenance.MaintenanceError, "image_protection_changed"):
            self.call_main(protection_results=[(set(), set()), ({OLD_ID}, set())])

    def test_unverified_public_digest_and_unreviewed_ids_prevent_removal(self):
        for approved, remote in ((OLD_ID, False), (OTHER_ID, True)):
            with self.subTest(approved=approved, remote=remote):
                with self.assertRaisesRegex(maintenance.MaintenanceError, "approved_image_is_not_eligible"):
                    self.call_main(approved=approved, remote=remote)

    def test_inspect_never_invokes_image_removal(self):
        with patch.object(maintenance, "report_storage"), \
                patch.object(maintenance, "image_ids", return_value=[]), \
                patch.object(maintenance, "protection", return_value=(set(), set())), \
                patch.object(maintenance, "run") as command, contextlib.redirect_stdout(io.StringIO()):
            maintenance.main([str(self.installation)])
        command.assert_not_called()

    def test_cleanup_requires_explicit_candidate_and_approved_ids(self):
        for candidate, approved in (("", OLD_ID), (CANDIDATE, "")):
            with self.assertRaisesRegex(maintenance.MaintenanceError, "cleanup_requires_candidate_and_approved_ids"):
                maintenance.main([str(self.installation), "cleanup_unused_images", candidate, approved])


if __name__ == "__main__":
    unittest.main()
