"""Offline only: synthetic credentials, local TLS and a fake SSH boundary."""
import contextlib
import errno
import http.server
import importlib.util
import io
import json
import os
from pathlib import Path
import shlex
import shutil
import socket
import ssl
import subprocess
import sys
import tempfile
import threading
import time
import unittest
import urllib.error
import urllib.request
from unittest.mock import patch

ROOT = Path(__file__).parents[2]


def module(name, filename):
    spec = importlib.util.spec_from_file_location(name, ROOT / "deploy" / filename)
    value = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(value)
    return value


remote = module("remote_provider_validation", "validate-provider-host-post.py")
runner = module("runner_provider_validation", "run-provider-host-post.py")
KEYS = {"exa": "SYNTHETIC-EXA-PRIVATE", "tavily": "SYNTHETIC-TAVILY-PRIVATE"}
PRIVATE = "PRIVATE-RESPONSE-REQUEST-ID-URL"


class Response:
    def __init__(self, status):
        self.status, self.closed = status, False
    def __enter__(self):
        return self
    def __exit__(self, *args):
        self.close()
    def read(self, *args):
        raise AssertionError("A response body was read")
    def close(self):
        self.closed = True


class ProviderTests(unittest.TestCase):
    def test_deadline_interrupts_blocked_input_and_restores_signal_handler(self):
        import signal
        reader, writer = os.pipe()
        previous, start = signal.getsignal(signal.SIGALRM), time.monotonic()
        try:
            with self.assertRaises(TimeoutError), remote.deadline(1):
                os.read(reader, 1)
            self.assertLess(time.monotonic() - start, 2)
            self.assertIs(signal.getsignal(signal.SIGALRM), previous)
        finally:
            os.close(reader); os.close(writer)

    def test_fixed_requests_have_one_result_and_only_matching_header_credentials(self):
        for provider in remote.PROVIDERS:
            req = remote.request(provider, KEYS[provider])
            self.assertEqual(req.full_url, "https://api.exa.ai/search" if provider == "exa" else "https://api.tavily.com/search")
            self.assertEqual(req.get_method(), "POST")
            body = json.loads(req.data)
            self.assertEqual(body, {"query": remote.QUERY, "type": "auto", "numResults": 1, "contents": {"highlights": True}}
                             if provider == "exa" else {"query": remote.QUERY, "search_depth": "basic", "max_results": 1})
            headers = dict((k.lower(), v) for k, v in req.header_items())
            self.assertEqual(headers, {"content-type": "application/json", **({"x-api-key": KEYS[provider]}
                             if provider == "exa" else {"authorization": "Bearer " + KEYS[provider]})})
            self.assertNotIn(KEYS[provider], req.data.decode())
        with self.assertRaises(ValueError):
            remote.request("arbitrary", KEYS["exa"])

    def test_all_input_validation_finishes_before_any_http_call_or_secret_output(self):
        invalid = [b'', b'not-json', b'[]', b'x' * (remote.MAX_INPUT_BYTES + 1),
                   b'{"exa":"one","exa":"two","tavily":"three"}']
        invalid += [json.dumps(item).encode() for item in (
            {**KEYS, "extra": PRIVATE}, {"exa": KEYS['exa']}, {**KEYS, "exa": None},
            {**KEYS, "exa": 'key\nInjected: value'}, {**KEYS, "exa": 'key\x00'},
            {**KEYS, "exa": 'ключ'}, {**KEYS, "exa": 'x' * 2049})]
        for data in invalid:
            output = io.StringIO()
            with self.subTest(data=data[:20]), patch.object(sys, 'argv', ['validation']), \
                    patch.object(sys, 'stdin', type('Input', (), {'buffer': io.BytesIO(data)})()), \
                    patch.object(remote.urllib.request, 'build_opener') as opener, contextlib.redirect_stdout(output):
                self.assertEqual(remote.main(), 1)
                opener.assert_not_called()
            self.assertEqual([json.loads(line)['result'] for line in output.getvalue().splitlines()], ['invalid_input'] * 2)
            for value in (*KEYS.values(), PRIVATE):
                self.assertNotIn(value, output.getvalue())

    def test_valid_entrypoint_makes_exactly_two_calls_and_emits_only_statuses(self):
        calls = []
        class Opener:
            def open(self, req, timeout):
                calls.append((req, timeout))
                return Response(200)
        output = io.StringIO()
        with patch.object(sys, 'argv', ['validation']), \
                patch.object(sys, 'stdin', type('Input', (), {'buffer': io.BytesIO(json.dumps(KEYS).encode())})()), \
                patch.object(remote.urllib.request, 'build_opener', return_value=Opener()), contextlib.redirect_stdout(output):
            self.assertEqual(remote.main(), 0)
        self.assertEqual(len(calls), 2)
        self.assertEqual([call[1] for call in calls], [15, 15])
        self.assertEqual([json.loads(line) for line in output.getvalue().splitlines()],
                         [remote.result(provider, status=200) for provider in remote.PROVIDERS])
        for value in (*KEYS.values(), PRIVATE):
            self.assertNotIn(value, output.getvalue())

    def test_tls_proxy_redirect_policy_and_http_errors_never_read_bodies(self):
        for status in (200, 401, 402, 429, 503, True, '200', 600):
            response = Response(status)
            class Opener:
                def open(self, req, timeout):
                    return response
            with patch.object(remote.urllib.request, 'build_opener', return_value=Opener()) as builder:
                value = remote.probe('exa', KEYS['exa'])
            self.assertTrue(response.closed)
            handlers = builder.call_args.args
            self.assertEqual(handlers[0].proxies, {})
            self.assertIsInstance(handlers[1], remote.NoRedirect)
            self.assertEqual(handlers[2]._context.verify_mode, ssl.CERT_REQUIRED)
            self.assertTrue(handlers[2]._context.check_hostname)
            self.assertEqual(value, remote.result('exa', status=status))
        body = Response(None)
        error = urllib.error.HTTPError('https://private.example/' + PRIVATE, 403, PRIVATE, {'private': PRIVATE}, body)
        class FailingOpener:
            def open(self, req, timeout):
                raise error
        with patch.object(remote.urllib.request, 'build_opener', return_value=FailingOpener()):
            value = remote.probe('exa', KEYS['exa'])
        self.assertTrue(body.closed)
        self.assertEqual(value, remote.result('exa', status=403))
        self.assertNotIn(PRIVATE, json.dumps(value))

    def test_transport_enums_use_exception_types_not_raw_provider_text(self):
        for reason, expected in ((socket.gaierror(PRIVATE), 'dns'), (ssl.SSLError(PRIVATE), 'tls'),
                                 (TimeoutError(PRIVATE), 'timeout'), (OSError(errno.ENETUNREACH, PRIVATE), 'no_route'),
                                 (OSError(errno.ECONNREFUSED, PRIVATE), 'connection_refused'),
                                 (OSError(errno.ECONNRESET, PRIVATE), 'connection_reset'), (Exception(PRIVATE), 'unknown')):
            self.assertEqual(remote.classify(urllib.error.URLError(reason)), expected)

    def test_redirect_endpoint_is_never_contacted_or_sent_credentials(self):
        calls = []
        class Handler(http.server.BaseHTTPRequestHandler):
            def do_POST(self):
                calls.append(self.path)
                self.send_response(302)
                self.send_header('Location', '/credential-trap')
                self.end_headers()
            def log_message(self, *args):
                pass
        server = http.server.HTTPServer(('127.0.0.1', 0), Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            fixture = urllib.request.Request('http://127.0.0.1:%s/start' % server.server_port, b'{}',
                                             {'x-api-key': KEYS['exa']}, method='POST')
            with patch.object(remote, 'request', return_value=fixture):
                self.assertEqual(remote.probe('exa', KEYS['exa']), remote.result('exa', status=302))
            self.assertEqual(calls, ['/start'])
        finally:
            server.shutdown(); server.server_close(); thread.join(timeout=2)

    @unittest.skipUnless(shutil.which('openssl'), 'local synthetic TLS fixture requires OpenSSL')
    def test_untrusted_local_tls_certificate_fails_before_http(self):
        with tempfile.TemporaryDirectory(prefix='provider-host-tls-') as directory:
            cert, key = Path(directory) / 'cert.pem', Path(directory) / 'key.pem'
            subprocess.run(['openssl', 'req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-days', '1',
                            '-subj', '/CN=localhost', '-keyout', str(key), '-out', str(cert)],
                           stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=True, timeout=10)
            calls = []
            class Handler(http.server.BaseHTTPRequestHandler):
                def do_POST(self):
                    calls.append(self.path)
                def log_message(self, *args):
                    pass
            server = http.server.HTTPServer(('127.0.0.1', 0), Handler)
            context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
            context.load_cert_chain(cert, key)
            server.socket = context.wrap_socket(server.socket, server_side=True)
            thread = threading.Thread(target=server.serve_forever, daemon=True); thread.start()
            try:
                fixture = urllib.request.Request('https://127.0.0.1:%s/search' % server.server_port, b'{}', method='POST')
                with patch.object(remote, 'request', return_value=fixture):
                    self.assertEqual(remote.probe('exa', KEYS['exa']), remote.result('exa', 'tls'))
                self.assertEqual(calls, [])
            finally:
                server.shutdown(); server.server_close(); thread.join(timeout=2)


class RunnerTests(unittest.TestCase):
    def test_source_shell_quoting_keeps_script_one_argument_without_interpolation(self):
        source = "print(\"literal ' $() `command` \\\"\")\n"
        words = shlex.split(runner.remote_command(source))
        self.assertEqual(words, ['env', '-i', 'PATH=/usr/bin:/bin', 'timeout', '-s', 'KILL', '50', 'python3', '-c', source])

    def test_ssh_boundary_has_fixed_current_recipient_and_secrets_only_on_stdin(self):
        payload = json.dumps(KEYS).encode()
        stdout = b''.join(json.dumps(remote.result(provider, status=200)).encode() + b'\n' for provider in remote.PROVIDERS)
        class Process:
            def __init__(self):
                self.stdin, self.stdout = io.BytesIO(), io.BytesIO(stdout)
            def poll(self):
                return 0
            def wait(self, timeout):
                return 0
        class Input(io.BytesIO):
            captured = None
            def close(self):
                self.captured = self.getvalue()
                super().close()
        process = Process(); process.stdin = Input()
        with patch.object(runner.subprocess, 'Popen', return_value=process) as popen:
            records, code = runner.invoke('print("reviewed source")', payload)
        command = popen.call_args.args[0]
        self.assertEqual(command[-2], 'maxposty-deploy@77.91.94.235')
        self.assertNotIn('178.159.94.83', json.dumps(command))
        self.assertIn('StrictHostKeyChecking=yes', command)
        self.assertIn('IdentitiesOnly=yes', command)
        self.assertEqual(command[command.index('-p') + 1], '22')
        self.assertEqual(popen.call_args.kwargs['env'], {'PATH': '/usr/bin:/bin'})
        self.assertIs(popen.call_args.kwargs['stderr'], subprocess.DEVNULL)
        self.assertEqual(process.stdin.captured, payload)
        for value in KEYS.values():
            self.assertNotIn(value, json.dumps(command))
            self.assertNotIn(value, json.dumps(popen.call_args.kwargs, default=str))
            self.assertNotIn(value, json.dumps(records))
        self.assertEqual(code, 0)

    def test_untrusted_ssh_output_cannot_cross_allowlist(self):
        valid = [remote.result(provider, status=200) for provider in remote.PROVIDERS]
        bad = [b'x' * (runner.MAX_OUTPUT_BYTES + 1), PRIVATE.encode(),
               json.dumps([valid]).encode(), b'']
        for item in ({**valid[0], 'body': PRIVATE}, {**valid[0], 'http_status': True},
                     {**valid[0], 'result': PRIVATE}, {**valid[0], 'provider': PRIVATE}):
            bad.append(b'\n'.join(json.dumps(r).encode() for r in [item, valid[1]]))
        for data in bad:
            with self.subTest(data=data[:20]), self.assertRaises((ValueError, UnicodeDecodeError)):
                runner.validate_output(data)

    def test_missing_or_malformed_runner_secret_never_invokes_ssh(self):
        for environment in ({}, {'EXA_API_KEY': KEYS['exa'], 'TAVILY_API_KEY': 'invalid\nheader'}):
            output = io.StringIO()
            with patch.dict(os.environ, environment, clear=True), patch.object(sys, 'argv', ['runner']), \
                    patch.object(runner, 'invoke') as invoke, contextlib.redirect_stdout(output):
                self.assertEqual(runner.main(), 1)
                invoke.assert_not_called()
            for value in KEYS.values():
                self.assertNotIn(value, output.getvalue())

    def test_private_ssh_failure_or_response_is_never_printed(self):
        output = io.StringIO()
        with patch.dict(os.environ, {'EXA_API_KEY': KEYS['exa'], 'TAVILY_API_KEY': KEYS['tavily']}, clear=True), \
                patch.object(sys, 'argv', ['runner']), \
                patch.object(runner, 'invoke', side_effect=ValueError(PRIVATE + str(KEYS))), \
                contextlib.redirect_stdout(output):
            self.assertEqual(runner.main(), 1)
        self.assertEqual([json.loads(line)['result'] for line in output.getvalue().splitlines()], ['ssh_failed'] * 2)
        for value in (*KEYS.values(), PRIVATE):
            self.assertNotIn(value, output.getvalue())

    def test_workflow_is_reviewed_exact_main_production_manual_with_no_extra_secrets(self):
        workflow = (ROOT / '.github/workflows/validate-provider-host-post.yml').read_text()
        self.assertIn("  workflow_dispatch:\n", workflow)
        self.assertIn("github.ref == 'refs/heads/main'", workflow)
        self.assertIn("github.repository == 'ArtemMakolov1/backend-max'", workflow)
        self.assertIn("name: production", workflow)
        self.assertIn("ref: ${{ github.sha }}", workflow)
        self.assertIn("persist-credentials: false", workflow)
        self.assertIn("if: ${{ always() }}", workflow)
        for forbidden in ('  push:', '  schedule:', '    inputs:', 'OPENAI_API_KEY', 'upload-artifact',
                          'ssh-keyscan', '.env.production', 'scp ', 'contents: write', 'packages: write'):
            self.assertNotIn(forbidden, workflow)


if __name__ == '__main__':
    unittest.main()
