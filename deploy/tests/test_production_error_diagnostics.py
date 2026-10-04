"""Offline regression tests: raw logs never cross the diagnostic boundary."""
import contextlib
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

SCRIPT = Path(__file__).parents[1] / "diagnose-production-errors.py"
SPEC = importlib.util.spec_from_file_location("production_error_diagnostics", SCRIPT)
diagnostic = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(diagnostic)

SECRETS = (
    "sk-proj-SYNTHETIC-DO-NOT-PRINT", "MAX_BOT_TOKEN=synthetic-private-token",
    "https://provider.example/private?token=synthetic-signed-secret",
    "private-sql-row-content", "private-channel-post", "private-provider-request",
    "private-constraint-suffix", "private-column-name",
    "private-provider-host.example", "10.20.30.40", "192.0.2.55",
)


def record(error, **extra):
    return {"time": "2026-10-04T20:08:21.123456789Z", "msg": "request failed", "error": error, **extra}


def provider_record(**extra):
    return record("; ".join(SECRETS), msg="OpenAI research request failed", **extra)


class SanitizerTests(unittest.TestCase):
    def assert_safe(self, value):
        rendered = json.dumps(value)
        for secret in SECRETS:
            self.assertNotIn(secret, rendered)
        keys = {"time", "category", "sqlstate", "table", "constraint", "prefix_chain", "transport_kind"}
        if value["category"] in ("provider_http", "provider_result"):
            keys |= {"provider_status", "provider_code", "provider_reason"}
            self.assertTrue(value["provider_status"] is None or (
                type(value["provider_status"]) is int and value["provider_status"] in diagnostic.PROVIDER_HTTP_STATUSES))
            self.assertTrue(value["provider_code"] is None or value["provider_code"] in diagnostic.PROVIDER_CODES)
            self.assertIn(value["provider_reason"], (None, "unsupported_region"))
        self.assertEqual(set(value), keys)
        self.assertTrue(value["sqlstate"] is None or value["sqlstate"] in diagnostic.SQLSTATES)
        self.assertTrue(value["table"] is None or value["table"] in diagnostic.TABLES)
        self.assertTrue(value["constraint"] is None or value["constraint"] in diagnostic.CONSTRAINTS)
        self.assertTrue(all(prefix in diagnostic.PREFIXES for prefix in value["prefix_chain"]))
        self.assertTrue(value["transport_kind"] is None or value["transport_kind"] in diagnostic.TRANSPORT_KINDS)

    def test_candidate_permission_and_fk_errors_are_distinguished_from_cache(self):
        candidate = diagnostic.sanitize_record(record(
            'ERROR: permission denied for table content_discovery_candidates (SQLSTATE 42501)'))
        self.assertEqual(candidate["category"], "discovery_candidates_database")
        self.assertEqual(candidate["table"], "content_discovery_candidates")
        self.assertEqual(candidate["sqlstate"], "42501")
        for constraint, expected_table in (
            ("content_discovery_candidates_check", "content_discovery_candidates"),
            ("content_discovery_candidates_workspace_id_channel_id_fkey", "content_discovery_candidates"),
            ("content_analysis_cache_result_json_check", "content_analysis_cache"),
            ("content_discovery_draft_opera_workspace_id_actor_user_id_c_fkey", "content_discovery_draft_operations"),
        ):
            with self.subTest(constraint=constraint):
                safe = diagnostic.sanitize_record(record(
                    f'ERROR: new row violates constraint "{constraint}" (SQLSTATE 23514)'))
                self.assertEqual(safe["table"], expected_table)
                self.assertEqual(safe["constraint"], constraint)
                self.assert_safe(safe)

    def test_arbitrary_fields_and_sql_row_contents_are_never_copied(self):
        error = ('ERROR: new row for relation "content_analysis_cache" violates check constraint '
                 '"content_analysis_cache_result_json_check"; failing row contains (' + ", ".join(SECRETS) + ') (SQLSTATE 23514)')
        safe = diagnostic.sanitize_record(record(error, body=SECRETS, request=SECRETS,
                                               content=SECRETS, env=SECRETS, api_key=SECRETS[0]))
        self.assertEqual(safe["category"], "content_analysis_cache_database")
        self.assert_safe(safe)

    def test_unknown_sqlstates_tables_constraints_and_prefixes_stay_unknown(self):
        for error in (
            'ERROR: permission denied for table content_discovery_candidates (SQLSTATE ABCDE)',
            'private-provider-request: ERROR: permission denied for table content_analysis_cache (SQLSTATE 42501)',
        ):
            safe = diagnostic.sanitize_record(record(error))
            self.assertEqual((safe["category"], safe["sqlstate"], safe["table"]), ("unknown", None, None))
            self.assert_safe(safe)
        safe = diagnostic.sanitize_record(record(
            'ERROR: new row for relation "content_analysis_cache_private_column_name" violates constraint '
            '"content_discovery_candidates_private_constraint_suffix" (SQLSTATE 23514)'))
        self.assertEqual((safe["table"], safe["constraint"]), (None, None))
        self.assert_safe(safe)

    def test_transport_and_json_decode_print_static_prefix_only(self):
        for prefix, category in (
            ("call OpenAI Responses API", "provider_transport"),
            ("read OpenAI Responses response", "provider_response_read"),
            ("decode OpenAI Responses response", "provider_response_json_decode"),
        ):
            with self.subTest(prefix=prefix):
                safe = diagnostic.sanitize_record(record(prefix + ": " + "; ".join(SECRETS)))
                self.assertEqual(safe["category"], category)
                self.assertEqual(safe["prefix_chain"], [prefix])
                self.assert_safe(safe)
        # A provider body that resembles PostgreSQL must not be promoted to DB diagnostics.
        safe = diagnostic.sanitize_record(record(
            'decode OpenAI Responses response: ERROR: permission denied for table content_analysis_cache (SQLSTATE 42501)'))
        self.assertEqual(safe["category"], "provider_response_json_decode")
        self.assertEqual((safe["sqlstate"], safe["table"]), (None, None))

    def test_transport_subtypes_discard_private_urls_hosts_addresses_and_certificate_details(self):
        for reason, expected in (
            ("dial tcp: lookup private-provider-host.example on 10.20.30.40:53: no such host", "dns"),
            ("dial tcp: lookup private-provider-host.example on 10.20.30.40:53: server misbehaving", "dns"),
            ("dial tcp: lookup private-provider-host.example: read udp 10.20.30.40:123->192.0.2.55:53: i/o timeout", "dns"),
            ("tls: failed to verify certificate: x509: certificate is valid for private-provider-host.example, not private-channel-post", "tls_certificate_verification"),
            ("x509: certificate signed by unknown authority private-provider-request", "tls_certificate_verification"),
            ("net/http: TLS handshake timeout", "tls_handshake_timeout"),
            ("remote error: tls: handshake failure", "tls_handshake_failure"),
            ("dial tcp 192.0.2.55:443: connect: connection refused", "connection_refused"),
            ("read tcp 10.20.30.40:123->192.0.2.55:443: read: connection reset by peer", "connection_reset"),
            ("dial tcp 192.0.2.55:443: i/o timeout", "connect_timeout"),
            ("dial tcp 192.0.2.55:443: connect: connection timed out", "connect_timeout"),
            ("dial tcp 192.0.2.55:443: connect: cannot assign requested address", "connect_error"),
            ("dial tcp 192.0.2.55:443: connect: no route to host", "no_route"),
            ("dial tcp 192.0.2.55:443: connect: network is unreachable", "no_route"),
            ("EOF", "eof"),
            ("unexpected EOF", "eof"),
            ("context deadline exceeded (Client.Timeout exceeded while awaiting headers)", "deadline_exceeded"),
            ("net/http: request canceled (Client.Timeout exceeded while awaiting headers)", "deadline_exceeded"),
            ("context canceled", "request_canceled"),
            ("net/http: timeout awaiting response headers", "response_header_timeout"),
            ("read tcp 10.20.30.40:123->192.0.2.55:443: read: i/o timeout", "read_timeout"),
            ("write tcp 10.20.30.40:123->192.0.2.55:443: write: i/o timeout", "write_timeout"),
            ("private-provider-request " + SECRETS[0], "unknown"),
        ):
            with self.subTest(expected=expected, reason=reason):
                error = 'call OpenAI Responses API: Post "' + SECRETS[2] + '": ' + reason
                safe = diagnostic.sanitize_record(record(error, url=SECRETS[2], response=SECRETS))
                self.assertEqual(safe["transport_kind"], expected)
                self.assertEqual(safe["category"], "provider_transport")
                self.assertEqual((safe["sqlstate"], safe["table"], safe["constraint"]), (None, None, None))
                self.assert_safe(safe)

    def test_transport_subtype_ignores_reason_like_private_url_and_unknown_wrappers(self):
        for quoted_url in (
            'https://private-provider-host.example/net/http: TLS handshake timeout?token=' + SECRETS[0],
            'https://private-provider-host.example/connection refused?token=' + SECRETS[0],
            r'https://private-provider-host.example/escaped\"certificate\\path?token=' + SECRETS[0],
        ):
            safe = diagnostic.sanitize_record(record('call OpenAI Responses API: Post "' + quoted_url + '": EOF'))
            self.assertEqual(safe["transport_kind"], "eof")
            self.assert_safe(safe)
        for suffix in (
            "private-provider-request: net/http: TLS handshake timeout",
            "private-provider-request: EOF",
            "net/http: TLS handshake timeout\n" + SECRETS[0],
            "dial tcp: lookup private-provider-host.example: unrecognized private-provider-request",
        ):
            safe = diagnostic.sanitize_record(record("call OpenAI Responses API: " + suffix))
            self.assertEqual(safe["transport_kind"], "unknown")
            self.assert_safe(safe)

    def test_transport_classification_applies_only_to_source_owned_transport_prefix(self):
        for error in (
            "net/http: TLS handshake timeout",
            "read OpenAI Responses response: EOF",
            "decode OpenAI Responses response: context canceled",
            "private-provider-request: call OpenAI Responses API: EOF",
            'ERROR: permission denied for table content_analysis_cache (SQLSTATE 42501)',
        ):
            safe = diagnostic.sanitize_record(record(error))
            self.assertIsNone(safe["transport_kind"])
            self.assert_safe(safe)
        safe = diagnostic.sanitize_record(record("call OpenAI Responses API: context deadline exceeded"))
        self.assertEqual((safe["category"], safe["transport_kind"]), ("deadline_exceeded", "deadline_exceeded"))
        self.assert_safe(safe)

    def test_static_wrapped_source_prefix_chain_is_bounded(self):
        safe = diagnostic.sanitize_record(record(
            'list content discovery publications: lock workspace for MAX history write: '
            'ERROR: permission denied for table workspaces (SQLSTATE 42501)'))
        self.assertEqual(safe["prefix_chain"], ["list content discovery publications", "lock workspace for MAX history write"])
        self.assertEqual(safe["table"], "workspaces")
        safe = diagnostic.sanitize_record(record("call OpenAI Responses API: " * 100 + SECRETS[0]))
        self.assertEqual(len(safe["prefix_chain"]), 4)
        self.assert_safe(safe)

    def test_non_failure_messages_or_non_string_errors_are_not_exposed(self):
        for item in (None, [], {"msg": "OpenAI image request failed", "error": SECRETS[0]},
                     {"msg": "request validation failed", "error": SECRETS[0]}):
            self.assertIsNone(diagnostic.sanitize_record(item))
        safe = diagnostic.sanitize_record(record({"message": SECRETS}))
        self.assertEqual(safe["category"], "unknown")
        self.assert_safe(safe)

    def test_exact_research_logger_reports_only_known_status_and_code(self):
        for status in diagnostic.PROVIDER_HTTP_STATUSES:
            for code in diagnostic.PROVIDER_CODES:
                with self.subTest(status=status, code=code):
                    safe = diagnostic.sanitize_record(provider_record(
                        status=status, code=code, request_id=SECRETS[5], body=SECRETS,
                        response=SECRETS, url=SECRETS[2], api_key=SECRETS[0]))
                    self.assertEqual(safe["category"], "provider_http")
                    self.assertEqual((safe["provider_status"], safe["provider_code"]), (status, code))
                    self.assertEqual((safe["sqlstate"], safe["table"], safe["constraint"], safe["transport_kind"]),
                                     (None, None, None, None))
                    self.assertEqual(safe["prefix_chain"], [])
                    self.assertIsNone(safe["provider_reason"])
                    self.assert_safe(safe)

    def test_provider_codes_are_exact_enums_and_private_values_are_discarded(self):
        for code in (None, {}, [], 400, True, *SECRETS, "model_not_found " + SECRETS[0],
                     "model_not_found\n" + SECRETS[0], "MODEL_NOT_FOUND", " model_not_found", "unrecognized_error"):
            with self.subTest(code=code):
                safe = diagnostic.sanitize_record(provider_record(status=400, code=code, request_id=SECRETS[5]))
                self.assertEqual(safe["provider_status"], 400)
                self.assertIsNone(safe["provider_code"])
                self.assert_safe(safe)

    def test_provider_http_status_requires_an_allowlisted_integer(self):
        for status in (None, {}, [], True, False, 400.0, "400", SECRETS[0], 0, 200, 301, -1, 499, 999):
            with self.subTest(status=status):
                safe = diagnostic.sanitize_record(provider_record(status=status, code="missing_citations"))
                self.assertIsNone(safe["provider_status"])
                self.assertEqual(safe["provider_code"], "missing_citations")
                self.assertEqual(safe["category"], "provider_result" if type(status) is int and status == 0 else "provider_http")
                self.assert_safe(safe)

    def test_provider_fields_are_ignored_for_every_other_logger_message(self):
        for msg in ("OpenAI image request failed", "MAX request failed", "content source retrieval failed",
                    "OpenAI research request failed ", "OpenAI research request failed\n" + SECRETS[0], SECRETS[0]):
            self.assertIsNone(diagnostic.sanitize_record(record(SECRETS[0], msg=msg, status=400, code="model_not_found")))
        safe = diagnostic.sanitize_record(record("context canceled", status=400, code="model_not_found"))
        self.assertEqual(safe["category"], "request_canceled")
        self.assertNotIn("provider_status", safe)
        self.assertNotIn("provider_code", safe)
        self.assert_safe(safe)

    def test_current_logger_without_code_does_not_infer_it_from_private_error(self):
        safe = diagnostic.sanitize_record(provider_record(status=403, request_id=SECRETS[5]))
        self.assertEqual(safe["provider_status"], 403)
        self.assertIsNone(safe["provider_code"])
        self.assert_safe(safe)
        for error in ("model_not_found", "missing_citations", "ERROR: permission denied for table content_analysis_cache (SQLSTATE 42501)"):
            safe = diagnostic.sanitize_record(record(error, msg="OpenAI research request failed", status=0))
            self.assertEqual(safe["category"], "provider_result")
            self.assertIsNone(safe["provider_status"])
            self.assertIsNone(safe["provider_code"])
            self.assert_safe(safe)

    def test_region_reason_requires_403_and_the_complete_canonical_message(self):
        canonical = "Country, region, or territory not supported"
        for error in (canonical, canonical.upper(), " \n" + canonical + "\t "):
            safe = diagnostic.sanitize_record(record(error, msg="OpenAI research request failed", status=403,
                                                     request_id=SECRETS[5], body=SECRETS, url=SECRETS[2]))
            self.assertEqual(safe["provider_reason"], "unsupported_region")
            self.assertEqual(safe["provider_status"], 403)
            self.assertIsNone(safe["provider_code"])
            self.assert_safe(safe)
        for error in (*SECRETS, canonical + " " + SECRETS[0], SECRETS[0] + " " + canonical,
                      canonical + "\n" + SECRETS[2], "unsupported_country_region_territory", canonical + ".",
                      {"message": canonical}, [canonical], None):
            with self.subTest(error=error):
                safe = diagnostic.sanitize_record(record(error, msg="OpenAI research request failed", status=403,
                                                         code=canonical + " " + SECRETS[0], request_id=SECRETS[5]))
                self.assertIsNone(safe["provider_reason"])
                self.assertIsNone(safe["provider_code"])
                self.assert_safe(safe)
        for status in (0, 400, 401, 404, 429, "403", 403.0, True, None):
            safe = diagnostic.sanitize_record(record(canonical, msg="OpenAI research request failed", status=status))
            self.assertIsNone(safe["provider_reason"])
            self.assert_safe(safe)

    def test_timestamps_are_validated_and_reserialized(self):
        self.assertEqual(diagnostic.safe_time("2026-10-04T23:08:21+03:00"), "2026-10-04T20:08:21Z")
        for value in (SECRETS[0], None, {}, "2026-19-99T00:00:00Z", "2026-10-04T20:08:21Z\nprivate-channel-post",
                      "2026-10-04T20:08:21", "9999-12-31T23:59:59-23:00"):
            self.assertIsNone(diagnostic.safe_time(value))

    def test_malformed_utf8_deep_json_and_oversized_lines_are_skipped(self):
        output = []
        reader = diagnostic.BoundedDiagnostics(output.append)
        reader.feed(b'not json\n{"bad":"\xff"}\n' + b'[' * 2000 + b']' * 2000 + b'\n')
        reader.feed(b'X' * (diagnostic.MAX_LINE_BYTES + 1))
        reader.feed(b'\n' + json.dumps(record("context deadline exceeded")).encode())
        summary = reader.finish()
        self.assertEqual(len(output), 1)
        self.assertEqual(output[0]["category"], "deadline_exceeded")
        self.assertEqual(summary["request_failures"], 1)
        self.assert_safe(output[0])

    def test_line_and_byte_caps_terminate_input_without_raw_output(self):
        output = []
        reader = diagnostic.BoundedDiagnostics(output.append)
        self.assertFalse(reader.feed(b'{}\n' * (diagnostic.MAX_LINES + 10)))
        self.assertEqual(reader.finish()["lines_read"], diagnostic.MAX_LINES)
        self.assertTrue(reader.truncated)
        reader = diagnostic.BoundedDiagnostics(output.append)
        self.assertFalse(reader.feed(b'X' * (diagnostic.MAX_BYTES + 100)))
        self.assertEqual(reader.finish()["bytes_read"], diagnostic.MAX_BYTES)
        self.assertTrue(reader.truncated)
        self.assertEqual(output, [])

    def test_stream_chunk_boundaries_do_not_change_sanitization(self):
        payload = json.dumps(record("decode OpenAI Responses response: " + SECRETS[0])).encode() + b'\n'
        output = []
        reader = diagnostic.BoundedDiagnostics(output.append)
        for byte in payload:
            reader.feed(bytes([byte]))
        self.assertEqual(reader.finish()["request_failures"], 1)
        self.assert_safe(output[0])


class InvocationTests(unittest.TestCase):
    def test_actual_remote_entrypoint_reads_only_inspect_labels_and_bounded_logs(self):
        with tempfile.TemporaryDirectory(prefix="diagnostic-") as directory:
            root = Path(directory)
            release = root / "releases" / ("a" * 40)
            (release / "deploy").mkdir(parents=True)
            (release / "deploy" / "compose.production.yaml").write_text("name: maxposty-backend\n")
            (root / "current").symlink_to(release)
            commands = root / "docker-commands.jsonl"
            fake = root / "docker"
            transport_record = json.dumps(record(
                'call OpenAI Responses API: Post "' + SECRETS[2] + '": net/http: TLS handshake timeout', body=SECRETS))
            fake.write_text("#!/usr/bin/env python3\nimport json,sys\nfrom pathlib import Path\n"
                            "root=Path(__file__).parent\n"
                            "with (root/'docker-commands.jsonl').open('a') as log: log.write(json.dumps(sys.argv[1:])+'\\n')\n"
                            "if sys.argv[1]=='inspect': print('maxposty-backend|backend|'+'a'*40+'|'+'b'*64)\n"
                            "elif sys.argv[1]=='logs':\n"
                            f" print({json.dumps(record('ERROR: permission denied for table content_discovery_candidates (SQLSTATE 42501)', body=SECRETS))!r})\n"
                            f" print({json.dumps(record('decode OpenAI Responses response: ' + SECRETS[0]))!r},file=sys.stderr)\n"
                            f" print({transport_record!r},file=sys.stderr)\n"
                            f" print({json.dumps(provider_record(status=400, code='model_not_found', request_id=SECRETS[5], response=SECRETS))!r},file=sys.stderr)\n"
                            f" print({json.dumps(record('Country, region, or territory not supported', msg='OpenAI research request failed', status=403, request_id=SECRETS[5], response=SECRETS))!r},file=sys.stderr)\n"
                            f" print({SECRETS[2]!r},file=sys.stderr)\n"
                            "else: sys.exit(91)\n")
            fake.chmod(0o700)
            result = subprocess.run([sys.executable, str(SCRIPT), directory], capture_output=True, text=True,
                                    env={**os.environ, "PATH": str(root) + os.pathsep + os.environ.get("PATH", "")}, timeout=10)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(result.stderr, "")
            for secret in SECRETS:
                self.assertNotIn(secret, result.stdout)
            output = [json.loads(line) for line in result.stdout.splitlines()]
            self.assertEqual(len(output), 6)
            transport = [item for item in output[:-1] if item["category"] == "provider_transport"]
            self.assertEqual(len(transport), 1)
            self.assertEqual(transport[0]["transport_kind"], "tls_handshake_timeout")
            provider = [item for item in output[:-1] if item["category"] == "provider_http"]
            self.assertEqual(len(provider), 2)
            self.assertEqual((provider[0]["provider_status"], provider[0]["provider_code"]), (400, "model_not_found"))
            self.assertEqual((provider[1]["provider_status"], provider[1]["provider_code"], provider[1]["provider_reason"]),
                             (403, None, "unsupported_region"))
            self.assertEqual(output[-1]["request_failures"], 5)
            self.assertEqual(output[-1]["read_status"], "complete")
            calls = [json.loads(line) for line in commands.read_text().splitlines()]
            self.assertEqual([call[0] for call in calls], ["inspect", "logs", "inspect"])
            self.assertEqual(calls[1], ["logs", "--since=1h", "--tail=2000", "b" * 64])
            self.assertNotIn(".Config.Env", json.dumps(calls))

    def test_deploy_race_discards_every_sanitized_record_before_output(self):
        safe = diagnostic.sanitize_record(record('ERROR: permission denied for table content_analysis_cache (SQLSTATE 42501)'))
        provider = diagnostic.sanitize_record(provider_record(status=403, code="unsupported_country_region_territory"))
        output = io.StringIO()
        with patch.object(sys, 'argv', ['diagnostic', '/opt/maxposty/backend']), \
                patch.object(diagnostic, 'validate_target', side_effect=['a' * 64, 'b' * 64]), \
                patch.object(diagnostic, 'read_diagnostics', return_value=([safe, provider], {'read_status': 'complete'})), \
                contextlib.redirect_stdout(output):
            self.assertEqual(diagnostic.main(), 1)
        self.assertEqual(json.loads(output.getvalue()), {'read_status': 'unavailable'})
        self.assertNotIn('content_analysis_cache', output.getvalue())
        self.assertNotIn('provider_http', output.getvalue())

    def test_log_read_timeout_stops_only_the_log_client(self):
        process = subprocess.Popen([sys.executable, '-c', 'import time; print("private-provider-request", flush=True); time.sleep(10)'],
                                   stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
        with patch.object(diagnostic.subprocess, 'Popen', return_value=process), patch.object(diagnostic, 'READ_TIMEOUT', 0.05):
            output, summary = diagnostic.read_diagnostics('a' * 64)
        self.assertEqual(output, [])
        self.assertEqual(summary['read_status'], 'timeout')
        self.assertIsNotNone(process.poll())
        self.assertTrue(process.stdout.closed)

    def test_entrypoint_rejects_arbitrary_arguments_without_tracebacks_or_commands(self):
        for arguments in ([], ["/tmp/../../etc"], ["/tmp/'private-provider-request"], ["/tmp", "arbitrary-command"]):
            result = subprocess.run([sys.executable, str(SCRIPT), *arguments], capture_output=True, text=True, timeout=5)
            self.assertEqual(result.returncode, 1)
            self.assertEqual(json.loads(result.stdout), {"read_status": "unavailable"})
            self.assertEqual(result.stderr, "")

    def test_workflow_is_manual_main_only_and_has_no_raw_artifacts_or_deploy(self):
        workflow = SCRIPT.parents[1] / ".github" / "workflows" / "diagnose-production-errors.yml"
        text = workflow.read_text()
        self.assertIn("  workflow_dispatch:\n", text)
        self.assertIn("github.ref == 'refs/heads/main'", text)
        self.assertIn("github.repository == 'ArtemMakolov1/backend-max'", text)
        self.assertIn("name: production", text)
        self.assertIn("StrictHostKeyChecking=yes", text)
        self.assertIn("VPS_SSH_KNOWN_HOSTS", text)
        self.assertIn("persist-credentials: false", text)
        self.assertIn("group: maxposty-backend-production-diagnostics", text)
        self.assertIn("<deploy/diagnose-production-errors.py 2>/dev/null", text)
        for disallowed in ("  push:", "  schedule:", "    inputs:", "packages: write", "contents: write", "actions: write",
                           "upload-artifact", "ssh-keyscan", "run-from-ci", "docker exec", "docker compose", "scp "):
            self.assertNotIn(disallowed, text)
        self.assertNotIn(".env.production", SCRIPT.read_text())


if __name__ == "__main__":
    unittest.main()
