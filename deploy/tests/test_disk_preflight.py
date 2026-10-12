import json
import http.server
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import threading
import unittest


SCRIPT = Path(__file__).resolve().parents[1] / "deploy-production.sh"
SOURCE = SCRIPT.read_text().split("<<'DISK_PREFLIGHT_PY'\n", 1)[1].split("\nDISK_PREFLIGHT_PY\n", 1)[0]
REF = "ghcr.io/example/backend@sha256:" + "a" * 64

FAKE = r'''#!/usr/bin/env python3
import json, os, sys, time
from pathlib import Path
c = json.loads(os.environ['PREFLIGHT_FIXTURE'])
a = sys.argv[1:]
name = Path(sys.argv[0]).name
with open(os.environ['PREFLIGHT_LOG'], 'a') as f:
    f.write(json.dumps([name, *a]) + '\n')
def emit(value):
    print(value)
    sys.exit()
def df(args, role):
    if c.get('bad_df') == role:
        emit('malformed PRIVATE_SENTINEL')
    inode = '-Pi' in args
    amount = c.get(role + ('_inodes' if inode else '_kib'), 1000000 if inode else 100000000)
    emit('Filesystem blocks used available capacity mount\n/dev/' + role + ' 999999999 0 ' + str(amount) + ' 0% /')
def stat(role):
    emit(c.get(role + '_device', 1))
if name == 'df':
    df(a, 'docker' if a[-1] == '/docker' else 'backup')
if name == 'stat':
    stat('docker' if a[-1] == '/docker' else 'backup')
if a[0] == 'info':
    emit(c.get('root', '/docker') if '.DockerRootDir' in a[2] else c.get('architecture', 'x86_64'))
if a[0] == 'compose':
    if a[-2:] == ['config', '--images']:
        emit(c.get('refs', 'ghcr.io/example/backend@sha256:' + 'a' * 64))
    if a[-3:] == ['ps', '-aq', 'postgres']:
        emit(c.get('pg_id', 'b' * 64))
if a[:2] == ['volume', 'ls']:
    emit('maxposty-backend_postgres-data' if c.get('existing_volume') else '')
if a[:2] == ['image', 'inspect']:
    if c.get('inspect_error'):
        print('Error response from daemon: permission denied PRIVATE_SENTINEL', file=sys.stderr)
        sys.exit(1)
    if c.get('missing'):
        print('Error response from daemon: No such image: ' + a[-1], file=sys.stderr)
        sys.exit(1)
    emit(c.get('local_image', 'sha256:' + 'c' * 64 + ' linux amd64'))
if a[:2] == ['manifest', 'inspect']:
    if c.get('oversized'):
        emit('X' * 300000)
    if c.get('bad_manifest'):
        emit('{invalid PRIVATE_SENTINEL')
    if 'manifest' in c:
        emit(json.dumps(c['manifest']))
    def entry(arch, size, variant=None):
        platform = {'architecture': arch, 'os': 'linux'}
        if variant: platform['variant'] = variant
        return {'Descriptor': {'platform': platform}, 'OCIManifest': {'layers': [{'size': size}]}}
    emit(json.dumps([entry('amd64', 100000000), entry('arm64', 200000000, 'v8'), entry('unknown', 1000000000)]))
if a[0] == 'inspect':
    emit(c.get('running', 'true') if '.State.Running' in a[2] else c.get('mount', 'volume:maxposty-backend_postgres-data'))
if a[0] == 'exec':
    if a[2] == 'df': df(a, 'postgres')
    if a[2] == 'stat': stat('postgres')
    if c.get('size_error'):
        print('psql: connection failed PRIVATE_SENTINEL', file=sys.stderr)
        sys.exit(1)
    emit(c.get('db_size', 100000000))
print('Unexpected metadata command PRIVATE_SENTINEL', file=sys.stderr)
sys.exit(1)
'''


class DiskPreflightTests(unittest.TestCase):
    def run_fixture(self, configuration=None, phase="before-pull", bootstrap=False, installation="/install"):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for name in ("docker", "df", "stat"):
                command = root / name
                command.write_text(FAKE)
                command.chmod(0o755)
            log = root / "commands.jsonl"
            environment = dict(os.environ, PATH=str(root) + os.pathsep + os.environ["PATH"],
                               PREFLIGHT_FIXTURE=json.dumps(configuration or {}), PREFLIGHT_LOG=str(log))
            result = subprocess.run([sys.executable, "-c", SOURCE, phase, installation, "/next", "/next/env", "/next/release",
                                     "" if bootstrap else "/old", "/old/env", "/old/release"],
                                    env=environment, capture_output=True, text=True, timeout=10)
            commands = [json.loads(line) for line in log.read_text().splitlines()]
        self.assertNotIn("PRIVATE_SENTINEL", result.stdout + result.stderr)
        self.assertFalse(any("pull" in command or "rm" in command or "pg_dump" in command or "stop" in command for command in commands))
        return result, commands

    def assert_blocked(self, fixture, reason=None, **kwargs):
        result, commands = self.run_fixture(fixture, **kwargs)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("blocked:", result.stderr)
        if reason:
            self.assertIn(reason, result.stderr)
        return result, commands

    def test_cached_images_and_shared_filesystem_reserve_pass(self):
        result, commands = self.run_fixture()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.count("required_bytes=661870912"), 3)
        self.assertFalse(any("manifest" in command for command in commands))
        query = next(command[-1] for command in commands if command[:2] == ["docker", "exec"] and "psql" in command[-1])
        self.assertIn("default_transaction_read_only=on", query)
        self.assertIn("statement_timeout=5000", query)
        self.assertIn('PGPASSWORD="$POSTGRES_PASSWORD"', query)

    def test_each_filesystem_low_bytes_blocks(self):
        for role in ("docker", "postgres", "backup"):
            with self.subTest(role=role):
                self.assert_blocked({role + "_kib": 100}, role + " filesystem")

    def test_each_filesystem_low_inodes_blocks(self):
        for role in ("docker", "postgres", "backup"):
            with self.subTest(role=role):
                self.assert_blocked({role + "_inodes": 1}, role + " filesystem")

    def test_snapshot_is_only_charged_to_actual_backup_filesystem(self):
        result, _ = self.run_fixture({"backup_device": 2})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.count("required_bytes=536870912"), 2)
        self.assertEqual(result.stdout.count("required_bytes=661870912"), 1)

    def test_existing_backup_mount_is_measured_instead_of_parent_directory(self):
        with tempfile.TemporaryDirectory() as directory:
            backup = Path(directory) / "backups"
            backup.mkdir()
            result, commands = self.run_fixture(installation=directory)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn(["df", "-Pk", str(backup)], commands)
            self.assertNotIn(["df", "-Pk", directory], commands)
            backup.rmdir()
            backup.write_text("not a directory")
            self.assert_blocked({}, "backup destination", installation=directory)

    def test_missing_image_budget_selects_native_not_attestation_layers(self):
        result, _ = self.run_fixture({"missing": True})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("required_bytes=961870912", result.stdout)
        self.assertIn("required_inodes=12288", result.stdout)
        self.assert_blocked({"missing": True, "docker_kib": 800000}, "docker filesystem")

    def test_arm64_variant_is_supported_and_wrong_cached_arch_is_reserved(self):
        result, _ = self.run_fixture({"architecture": "aarch64"})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("required_bytes=1261870912", result.stdout)

    def test_after_pull_reserves_snapshot_but_no_duplicate_image_growth(self):
        result, commands = self.run_fixture(phase="after-pull")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(any("manifest" in command for command in commands))
        self.assert_blocked({"missing": True}, "still missing", phase="after-pull")
        self.assert_blocked({"architecture": "aarch64"}, "still missing", phase="after-pull")

    def test_unknown_metadata_and_private_probe_errors_fail_closed(self):
        for fixture in ({"bad_df": "postgres"}, {"db_size": "unknown"}, {"size_error": True},
                        {"inspect_error": True}, {"root": ""}, {"architecture": "unknown"},
                        {"pg_id": ""}, {"running": "false"}, {"mount": "bind:private"}):
            with self.subTest(fixture=fixture):
                self.assert_blocked(fixture)

    def test_invalid_large_or_wrong_platform_manifests_fail_closed(self):
        for fixture in ({"bad_manifest": True}, {"oversized": True}, {"manifest": []},
                        {"manifest": [{"Descriptor": {"platform": {"architecture": "arm64", "os": "linux"}}}]},
                        {"manifest": [{"Descriptor": {"platform": {"architecture": "amd64", "os": "linux"}},
                                       "OCIManifest": {"layers": [{"size": -1}]}}]}):
            with self.subTest(fixture=fixture):
                self.assert_blocked(dict(fixture, missing=True))

    def test_empty_mutable_or_overlong_image_list_fails_closed(self):
        for refs in ("", "postgres:latest", "\n".join("ghcr.io/example/backend" + str(i) + "@sha256:" + "a" * 64 for i in range(33))):
            self.assert_blocked({"refs": refs}, "image")

    def test_bootstrap_checks_existing_volume_and_uses_small_snapshot_floor(self):
        result, commands = self.run_fixture(bootstrap=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("required_bytes=603979776", result.stdout)
        self.assertFalse(any("exec" in command for command in commands))
        self.assert_blocked({"existing_volume": True}, "existing PostgreSQL data", bootstrap=True)

    def test_preflight_order_protects_services_and_rollback(self):
        script = SCRIPT.read_text()
        self.assertLess(script.index('flock -n 9'), script.index('disk_preflight before-pull'))
        pull = script.index('compose "$release_dir" "$next_env" "$next_release" pull')
        before = script.index('disk_preflight before-pull')
        after = script.index('disk_preflight after-pull')
        mutation = script.index('\nstack_mutated=true', after)
        freeze = script.index('stop backend', mutation)
        self.assertLess(before, pull)
        self.assertLess(pull, after)
        self.assertLess(after, mutation)
        self.assertLess(mutation, freeze)

    @unittest.skipUnless(shutil.which("docker"), "Docker CLI not installed; daemon not required")
    def test_actual_docker_cli_image_fields_and_missing_error_format(self):
        fixture = {"Id": "sha256:" + "c" * 64, "Os": "linux", "Architecture": "amd64",
                   "Config": {"Env": ["PRIVATE_SENTINEL"]}}
        class Handler(http.server.BaseHTTPRequestHandler):
            def do_HEAD(self):
                self.send_response(200)
                self.send_header("API-Version", "1.49")
                self.end_headers()
            def do_GET(self):
                payload = json.dumps(fixture).encode()
                self.send_response(404 if "message" in fixture else 200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(payload)))
                self.end_headers()
                self.wfile.write(payload)
            def log_message(self, *_):
                pass
        server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            with tempfile.TemporaryDirectory() as clean_home:
                command = [shutil.which("docker"), "--config", clean_home, "--host", "tcp://127.0.0.1:" + str(server.server_port),
                           "image", "inspect", "--format", "{{.ID}} {{.Os}} {{.Architecture}}", REF]
                environment = {"HOME": clean_home, "PATH": "/usr/bin:/bin", "DOCKER_API_VERSION": "1.49"}
                result = subprocess.run(command, capture_output=True, text=True, env=environment, timeout=10)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(result.stdout.strip(), "sha256:" + "c" * 64 + " linux amd64")
                self.assertNotIn("PRIVATE_SENTINEL", result.stdout)
                fixture.clear()
                fixture["message"] = "No such image: " + REF
                result = subprocess.run(command, capture_output=True, text=True, env=environment, timeout=10)
                self.assertNotEqual(result.returncode, 0)
                self.assertRegex(result.stderr.strip(), r"^(?:Error response from daemon:|Error:) No such image: [^\r\n]+$")
        finally:
            server.shutdown()
            server.server_close()
            thread.join(timeout=2)


if __name__ == "__main__":
    unittest.main()
