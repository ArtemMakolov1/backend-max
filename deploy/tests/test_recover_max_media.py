"""Offline regression tests for exact-byte MAX recovery; no provider writes."""
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import socket
import tempfile
import time
from types import SimpleNamespace
import unittest
from unittest.mock import patch

SPEC = importlib.util.spec_from_file_location("recover_max_media", Path(__file__).parents[1] / "recover-max-media.py")
recovery = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(recovery)


class FakeTransport:
    def __init__(self, responses):
        self.responses = responses
        self.calls = []

    def request(self, method, url, headers=None, body=None, limit=recovery.MAX_JSON_BYTES, sink=None, max_trust=False):
        self.calls.append((method, url, dict(headers or {}), body))
        response = self.responses.pop(0)
        status, payload = response
        if isinstance(payload, dict):
            payload = json.dumps(payload).encode()
        if len(payload) > limit:
            raise recovery.RecoveryError("Provider response exceeds its recovery size bound")
        if sink is not None:
            sink.write(payload)
            payload = b""
        return status, payload


class RecoveryTests(unittest.TestCase):
    def setUp(self):
        self.old_umask = os.umask(0o077)
        self.directory = tempfile.TemporaryDirectory()
        self.state = Path(self.directory.name)
        self.payload = b"original-image-bytes"
        self.key = hashlib.sha256(self.payload).hexdigest() + ".png"
        self.targets = {self.key: {"size": len(self.payload), "mime": "image/png"}}
        self.env = {"MAX_BOT_TOKEN": "private-max-secret", "S3_HOST": "https://" + recovery.R2_HOST,
                    "S3_BUCKET": recovery.R2_BUCKET, "S3_REGION": "auto",
                    "S3_ACCESS_KEY": "private-access", "S3_SECRET_KEY": "private-secret"}

    def tearDown(self):
        self.directory.cleanup()
        os.umask(self.old_umask)

    def manifest(self, payload=None):
        (self.state / self.key).write_bytes(self.payload if payload is None else payload)
        (self.state / "manifest.json").write_text(json.dumps({"version": 1, "targets": self.targets, "matches": [self.key]}))

    def test_inventory_matches_original_and_rejects_transformed_bytes_without_r2_calls(self):
        message = {"recipient": {"chat_id": -42}, "body": {"mid": "mid.test", "attachments": [
            {"type": "image", "payload": {"token": "opaque-do-not-decode", "url": "https://cdn.example/original?private=signed"}},
            {"type": "image", "payload": {"url": "https://cdn.example/transformed"}}]}}
        transport = FakeTransport([(200, message), (200, self.payload), (200, b"transformed---------"[:len(self.payload)])])
        result = recovery.inventory(transport, self.env, self.state, self.targets, {"mid.test": "-42"})
        self.assertEqual(result["matched_keys"], 1)
        self.assertEqual(result["candidates_nonmatching"], 1)
        self.assertEqual((self.state / self.key).read_bytes(), self.payload)
        self.assertEqual({call[0] for call in transport.calls}, {"GET"})
        self.assertFalse(any(recovery.R2_HOST in call[1] for call in transport.calls))
        self.assertEqual(transport.calls[0][2], {"Authorization": "private-max-secret"})
        self.assertEqual(transport.calls[1][2], {})
        self.assertNotIn("private", json.dumps(result))
        self.assertNotIn("opaque", (self.state / "manifest.json").read_text())

    def test_provider_message_must_match_current_channel(self):
        transport = FakeTransport([(200, {"recipient": {"chat_id": -99}, "body": {"mid": "mid.test"}})])
        with self.assertRaises(recovery.RecoveryError):
            recovery.inventory(transport, self.env, self.state, self.targets, {"mid.test": "-42"})
        self.assertEqual(len(transport.calls), 1)

    def test_cached_wrong_bytes_block_every_r2_request(self):
        self.manifest(b"incorrect")
        transport = FakeTransport([])
        with self.assertRaises(recovery.RecoveryError):
            recovery.apply(transport, self.env, self.state, self.targets)
        self.assertEqual(transport.calls, [])

    def test_changed_ownership_blocks_every_r2_request(self):
        self.manifest()
        transport = FakeTransport([])
        with self.assertRaises(recovery.RecoveryError):
            recovery.apply(transport, self.env, self.state, {})
        self.assertEqual(transport.calls, [])

    def test_apply_conditionally_creates_and_verifies_exact_original(self):
        self.manifest()
        transport = FakeTransport([(404, b""), (200, b""), (200, self.payload)])
        result = recovery.apply(transport, self.env, self.state, self.targets)
        self.assertEqual([call[0] for call in transport.calls], ["HEAD", "PUT", "GET"])
        self.assertEqual(transport.calls[1][2]["if-none-match"], "*")
        self.assertEqual(transport.calls[1][2]["content-type"], "image/png")
        self.assertEqual(transport.calls[1][2]["cache-control"], "private, no-store")
        self.assertEqual(transport.calls[1][3], self.payload)
        self.assertEqual(result["uploaded"], 1)
        self.assertEqual(result["verified_bytes"], len(self.payload))

    def test_existing_conflicting_object_is_never_overwritten(self):
        self.manifest()
        transport = FakeTransport([(200, b""), (200, b"different-image-data"[:len(self.payload)])])
        with self.assertRaises(recovery.RecoveryError):
            recovery.apply(transport, self.env, self.state, self.targets)
        self.assertEqual([call[0] for call in transport.calls], ["HEAD", "GET"])

    def test_conditional_write_race_only_accepts_exact_competing_object(self):
        self.manifest()
        transport = FakeTransport([(404, b""), (412, b""), (200, self.payload)])
        result = recovery.apply(transport, self.env, self.state, self.targets)
        self.assertEqual(result["already_present"], 1)
        self.assertEqual(result["uploaded"], 0)

    def test_mixed_public_private_dns_and_tunnel_addresses_are_rejected(self):
        for address in ("127.0.0.1", "10.0.0.1", "169.254.169.254", "::1", "::ffff:127.0.0.1", "2002:7f00:1::", "224.0.0.1"):
            with self.subTest(address=address), patch.object(socket, "getaddrinfo", return_value=[
                (socket.AF_INET, socket.SOCK_STREAM, 6, "", ("8.8.8.8", 443)),
                (socket.AF_INET, socket.SOCK_STREAM, 6, "", (address, 443))]):
                with self.assertRaises(recovery.RecoveryError):
                    recovery.public_addresses("cdn.example")

    def test_url_policy_rejects_non_https_credentials_ports_and_control_characters(self):
        for url in ("http://cdn.example/file", "https://secret@cdn.example/file", "https://cdn.example:8443/file",
                    "https://cdn.example/file#fragment", "https://cdn.example/\r\nheader", "https://cdn.example\\@localhost/file"):
            with self.subTest(url=url), self.assertRaises(recovery.RecoveryError):
                recovery.validate_url(url)

    def test_inventory_requires_identical_positive_ready_metadata(self):
        base = {"assets": [{"key": self.key, "size": len(self.payload), "max_size": len(self.payload), "ready": True}],
                "messages": [{"id": "mid.test", "chat_id": "-42"}], "missing_metadata": 0}
        self.assertEqual(recovery.validate_inventory(base)[0], self.targets)
        for field, value in (("size", 0), ("max_size", 999), ("ready", False)):
            invalid = json.loads(json.dumps(base))
            invalid["assets"][0][field] = value
            with self.subTest(field=field), self.assertRaises(recovery.RecoveryError):
                recovery.validate_inventory(invalid)

    def test_https_connection_uses_pinned_public_ip_with_original_tls_hostname(self):
        raw, wrapped = object(), object()
        import ssl
        connection = recovery.PinnedHTTPSConnection("cdn.example", "8.8.8.8", ssl.create_default_context())
        calls = []
        connection._context = SimpleNamespace(wrap_socket=lambda sock, server_hostname: calls.append((sock, server_hostname)) or wrapped)
        with patch.object(socket, "create_connection", return_value=raw) as create:
            connection.connect()
        create.assert_called_once_with(("8.8.8.8", 443), timeout=20)
        self.assertEqual(calls, [(raw, "cdn.example")])
        self.assertIs(connection.sock, wrapped)

    def test_redirect_is_rejected_without_second_request_or_error_body_read(self):
        from unittest.mock import Mock
        transport = recovery.Transport.__new__(recovery.Transport)
        transport.system_context, transport.max_context = object(), object()
        transport.deadline, transport.bytes_received = time.monotonic() + 10, 0
        connection = Mock()
        connection.getresponse.return_value.status = 302
        with patch.object(recovery, "public_addresses", return_value=["8.8.8.8"]), patch.object(recovery, "PinnedHTTPSConnection", return_value=connection) as factory:
            status, body = transport.request("GET", "https://cdn.example/signed-private-url")
        self.assertEqual((status, body), (302, b""))
        factory.assert_called_once_with("cdn.example", "8.8.8.8", transport.system_context)
        connection.request.assert_called_once_with("GET", "/signed-private-url", body=None, headers={"Accept-Encoding": "identity"})
        connection.getresponse.return_value.read.assert_not_called()
        connection.close.assert_called_once()

    def test_response_size_bound_applies_before_download(self):
        from unittest.mock import Mock
        transport = recovery.Transport.__new__(recovery.Transport)
        transport.system_context, transport.max_context = object(), object()
        transport.deadline, transport.bytes_received = time.monotonic() + 10, 0
        connection = Mock()
        response = connection.getresponse.return_value
        response.status = 200
        response.getheader.side_effect = lambda name, default=None: {"Content-Length": "1024", "Content-Encoding": "identity"}.get(name, default)
        with patch.object(recovery, "public_addresses", return_value=["8.8.8.8"]), patch.object(recovery, "PinnedHTTPSConnection", return_value=connection), self.assertRaises(recovery.RecoveryError):
            transport.request("GET", "https://cdn.example/file", limit=8)
        response.read.assert_not_called()
        connection.close.assert_called_once()

    def test_cumulative_network_budget_rejects_second_response_even_if_each_fits(self):
        from unittest.mock import Mock
        transport = recovery.Transport.__new__(recovery.Transport)
        transport.system_context, transport.max_context = object(), object()
        transport.deadline, transport.bytes_received = time.monotonic() + 10, 0
        connection = Mock()
        response = connection.getresponse.return_value
        response.status = 200
        response.getheader.side_effect = lambda name, default=None: {"Content-Length": "6", "Content-Encoding": "identity"}.get(name, default)
        response.read.side_effect = [b"123456", b""]
        with patch.object(recovery, "MAX_TOTAL_BYTES", 10), patch.object(recovery, "public_addresses", return_value=["8.8.8.8"]), patch.object(recovery, "PinnedHTTPSConnection", return_value=connection):
            self.assertEqual(transport.request("GET", "https://cdn.example/first", limit=8), (200, b"123456"))
            self.assertEqual(transport.bytes_received, 6)
            with self.assertRaises(recovery.RecoveryError):
                transport.request("GET", "https://cdn.example/second", limit=8)
        # The second individually-valid body is blocked before reading it.
        self.assertEqual(response.read.call_count, 2)

    def test_max_read_only_get_recovers_after_rate_limit_and_transient_server_error(self):
        transport = FakeTransport([(429, b""), (503, b""), (200, {"body": {"mid": "mid.test"}})])
        with patch.object(recovery.time, "sleep") as sleep:
            status, body = recovery.api_get(transport, "private-max-secret", "/messages/mid.test")
        self.assertEqual(status, 200)
        self.assertEqual(body["body"]["mid"], "mid.test")
        self.assertEqual([call[0] for call in transport.calls], ["GET", "GET", "GET"])
        self.assertEqual([call.args[0] for call in sleep.call_args_list], [1, 4])

    def test_max_get_retry_budget_is_bounded_and_authentication_never_retries(self):
        transport = FakeTransport([(503, b""), (503, b""), (503, b"")])
        with patch.object(recovery.time, "sleep"):
            self.assertEqual(recovery.api_get(transport, "private-max-secret", "/messages/mid.test"), (503, None))
        self.assertEqual(len(transport.calls), 3)
        denied = FakeTransport([(401, b"")])
        with patch.object(recovery.time, "sleep") as sleep, self.assertRaises(recovery.RecoveryError):
            recovery.api_get(denied, "private-max-secret", "/messages/mid.test")
        sleep.assert_not_called()
        self.assertEqual(len(denied.calls), 1)

    def test_ambiguous_conditional_put_is_not_retried(self):
        self.manifest()
        transport = FakeTransport([(404, b""), (503, b"")])
        with self.assertRaises(recovery.RecoveryError):
            recovery.apply(transport, self.env, self.state, self.targets)
        self.assertEqual([call[0] for call in transport.calls], ["HEAD", "PUT"])


if __name__ == "__main__":
    unittest.main()
