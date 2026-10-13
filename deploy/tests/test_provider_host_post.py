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
import select
import shutil
import socket
import socketserver
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


@contextlib.contextmanager
def tls_relay_fixture(names=('api.exa.ai', 'api.tavily.com'), status=200):
    """A trusted synthetic origin behind an opaque, loopback-only TCP relay."""
    with tempfile.TemporaryDirectory(prefix='provider-relay-tls-') as directory:
        root = Path(directory)
        def openssl(*args):
            subprocess.run(['openssl', *args], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                           check=True, timeout=10)
        openssl('req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-days', '1', '-subj', '/CN=Synthetic Provider CA',
                '-keyout', str(root / 'ca.key'), '-out', str(root / 'ca.pem'),
                '-addext', 'basicConstraints=critical,CA:TRUE')
        openssl('req', '-new', '-newkey', 'rsa:2048', '-nodes', '-subj', '/CN=Synthetic Origin',
                '-keyout', str(root / 'origin.key'), '-out', str(root / 'origin.csr'))
        (root / 'extensions').write_text('subjectAltName=' + ','.join('DNS:' + name for name in names))
        openssl('x509', '-req', '-in', str(root / 'origin.csr'), '-CA', str(root / 'ca.pem'),
                '-CAkey', str(root / 'ca.key'), '-CAcreateserial', '-days', '1',
                '-extfile', str(root / 'extensions'), '-out', str(root / 'origin.pem'))
        requests, server_names, tls_prefixes = [], [], []
        class Origin(http.server.BaseHTTPRequestHandler):
            def do_POST(self):
                body = self.rfile.read(int(self.headers.get('Content-Length', '0')))
                requests.append({'path': self.path, 'host': self.headers.get('Host'),
                                 'exa_key': self.headers.get('x-api-key'), 'bearer': self.headers.get('Authorization'),
                                 'proxy_auth': self.headers.get('Proxy-Authorization'), 'body': json.loads(body)})
                self.send_response(status)
                if status == 302:
                    self.send_header('Location', 'https://credential-trap.invalid/' + PRIVATE)
                # No body is sent. A body read would wait until timeout instead
                # of returning immediately after the verified response headers.
                self.send_header('Content-Length', '1048576')
                self.end_headers()
            def log_message(self, *args):
                pass
        origin = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Origin)
        server_context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        server_context.load_cert_chain(root / 'origin.pem', root / 'origin.key')
        server_context.set_servername_callback(lambda _socket, name, _context: server_names.append(name))
        origin.socket = server_context.wrap_socket(origin.socket, server_side=True)
        original_dial = socket.create_connection
        class Relay(socketserver.BaseRequestHandler):
            def handle(self):
                with original_dial(origin.server_address, timeout=3) as upstream:
                    peers = (self.request, upstream)
                    first = True
                    while True:
                        ready, _, _ = select.select(peers, [], [], 3)
                        if not ready:
                            return
                        for source in ready:
                            try:
                                data = source.recv(16384)
                            except ConnectionResetError:
                                return
                            if not data:
                                return
                            if first and source is self.request:
                                tls_prefixes.append(data[:5]); first = False
                            try:
                                (upstream if source is self.request else self.request).sendall(data)
                            except (BrokenPipeError, ConnectionResetError):
                                return
        relay = socketserver.ThreadingTCPServer(('127.0.0.1', 0), Relay)
        relay.daemon_threads = True
        threads = [threading.Thread(target=server.serve_forever, daemon=True) for server in (origin, relay)]
        for thread in threads:
            thread.start()
        context = ssl.create_default_context(cafile=str(root / 'ca.pem'))
        context.minimum_version = ssl.TLSVersion.TLSv1_2
        try:
            yield {'origin': origin.server_address, 'relay': relay.server_address, 'context': context,
                   'requests': requests, 'sni': server_names, 'tls_prefixes': tls_prefixes, 'dial': original_dial}
        finally:
            for server in (relay, origin):
                server.shutdown(); server.server_close()
            for thread in threads:
                thread.join(timeout=2)


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
    def test_legacy_payload_and_tls_relay_authorities_are_validated_before_network(self):
        self.assertEqual(remote.credentials(json.dumps(KEYS).encode()), KEYS)
        for url, expected in [('tls://relay.example:443', ('relay.example', 443)),
                              ('tls://127.0.0.1:8443', ('127.0.0.1', 8443)),
                              ('tls://[2001:db8::1]:443', ('2001:db8::1', 443))]:
            with self.subTest(url=url):
                payload = {**KEYS, 'exa_proxy_url': url}
                self.assertEqual(remote.credentials(json.dumps(payload).encode()), payload)
                self.assertEqual(remote.relay_address(url), expected)
        invalid = [None, False, 'tls://relay.example', 'tls://relay.example:0', 'tls://relay.example:65536',
                   'tls://relay.example:443/', 'tls://relay.example:443?', 'tls://relay.example:443#',
                   'tls://user:SYNTHETIC-PROXY-PRIVATE@relay.example:443', 'tls://@relay.example:443',
                   'tls://[::1]suffix:443', 'tls://[::1]:443\nINJECTED=private']
        for url in invalid:
            data = json.dumps({**KEYS, 'exa_proxy_url': url}).encode()
            output = io.StringIO()
            with self.subTest(url=url), patch.object(sys, 'argv', ['validation']), \
                    patch.object(sys, 'stdin', type('Input', (), {'buffer': io.BytesIO(data)})()), \
                    patch.object(remote.urllib.request, 'build_opener') as opener, contextlib.redirect_stdout(output):
                self.assertEqual(remote.main(), 1)
                opener.assert_not_called()
            self.assertEqual([json.loads(line)['result'] for line in output.getvalue().splitlines()], ['invalid_input'] * 2)
            self.assertNotIn('SYNTHETIC-PROXY-PRIVATE', output.getvalue())
        with self.assertRaises(ValueError):
            remote.credentials(json.dumps({'exa': None, 'tavily': KEYS['tavily'],
                                           'exa_proxy_url': 'tls://relay.example:443'}).encode())

    def test_connect_proxy_probe_is_explicitly_unsupported_without_network_or_secret_echo(self):
        for url in ['http://relay.example:3128', 'https://user:SYNTHETIC-PROXY-PRIVATE@relay.example:443']:
            output = io.StringIO()
            with patch.object(sys, 'argv', ['validation']), \
                    patch.object(sys, 'stdin', type('Input', (), {'buffer': io.BytesIO(
                        json.dumps({**KEYS, 'exa_proxy_url': url}).encode())})()), \
                    patch.object(remote.urllib.request, 'build_opener') as opener, contextlib.redirect_stdout(output):
                self.assertEqual(remote.main(), 1)
                opener.assert_not_called()
            self.assertEqual([json.loads(line) for line in output.getvalue().splitlines()], remote.unsupported_proxy_records())
            self.assertNotIn('SYNTHETIC-PROXY-PRIVATE', output.getvalue())

    @unittest.skipUnless(shutil.which('openssl'), 'local synthetic TLS fixture requires OpenSSL')
    def test_tls_relay_retains_origin_sni_certificate_and_headers_with_tavily_direct(self):
        with tls_relay_fixture() as fixture:
            addresses = []
            def dial(address, timeout, source_address):
                addresses.append(address)
                target = fixture['origin'] if address == ('api.tavily.com', 443) else address
                self.assertIn(address, [fixture['relay'], ('api.tavily.com', 443)])
                return fixture['dial'](target, timeout, source_address)
            url = 'tls://%s:%s' % fixture['relay']
            with patch.dict(os.environ, {'HTTPS_PROXY': 'http://127.0.0.1:1'}, clear=True), \
                    patch.object(remote.ssl, 'create_default_context', return_value=fixture['context']), \
                    patch.object(remote.socket, 'create_connection', side_effect=dial):
                self.assertEqual(remote.probe('exa', KEYS['exa'], url), remote.result('exa', status=200))
                self.assertEqual(remote.probe('tavily', KEYS['tavily'], url), remote.result('tavily', status=200))
            self.assertEqual(addresses, [fixture['relay'], ('api.tavily.com', 443)])
            self.assertEqual(fixture['sni'], ['api.exa.ai', 'api.tavily.com'])
            self.assertEqual([item['host'] for item in fixture['requests']], ['api.exa.ai', 'api.tavily.com'])
            exa, tavily = fixture['requests']
            self.assertEqual(exa['exa_key'], KEYS['exa']); self.assertIsNone(exa['bearer'])
            self.assertEqual(tavily['bearer'], 'Bearer ' + KEYS['tavily']); self.assertIsNone(tavily['exa_key'])
            self.assertTrue(all(item['proxy_auth'] is None for item in fixture['requests']))
            self.assertEqual([item['path'] for item in fixture['requests']], ['/search', '/search'])
            self.assertTrue(fixture['tls_prefixes'][0].startswith(b'\x16\x03'))

    @unittest.skipUnless(shutil.which('openssl'), 'local synthetic TLS fixture requires OpenSSL')
    def test_tls_relay_rejects_trusted_wrong_origin_hostname_without_direct_fallback(self):
        with tls_relay_fixture(names=('relay.example',)) as fixture:
            addresses = []
            def dial(address, timeout, source_address):
                addresses.append(address)
                return fixture['dial'](address, timeout, source_address)
            with patch.object(remote.ssl, 'create_default_context', return_value=fixture['context']), \
                    patch.object(remote.socket, 'create_connection', side_effect=dial):
                value = remote.probe('exa', KEYS['exa'], 'tls://%s:%s' % fixture['relay'])
            self.assertEqual(value, remote.result('exa', 'tls'))
            self.assertEqual(fixture['sni'], ['api.exa.ai'])
            self.assertEqual(fixture['requests'], [])
            self.assertEqual(addresses, [fixture['relay']])

    @unittest.skipUnless(shutil.which('openssl'), 'local synthetic TLS fixture requires OpenSSL')
    def test_tls_relay_redirect_is_not_followed_and_http_status_remains_typed(self):
        with tls_relay_fixture(status=302) as fixture:
            with patch.object(remote.ssl, 'create_default_context', return_value=fixture['context']):
                value = remote.probe('exa', KEYS['exa'], 'tls://%s:%s' % fixture['relay'])
            self.assertEqual(value, remote.result('exa', status=302))
            self.assertEqual(len(fixture['requests']), 1)
            self.assertNotIn(PRIVATE, json.dumps(value))

    def test_tls_relay_refusal_does_not_retry_direct_origin(self):
        address = ('relay.example', 443)
        with patch.object(remote.socket, 'create_connection', side_effect=ConnectionRefusedError(
                errno.ECONNREFUSED, PRIVATE)) as dial:
            value = remote.probe('exa', KEYS['exa'], 'tls://%s:%s' % address)
        self.assertEqual(value, remote.result('exa', 'connection_refused'))
        self.assertEqual([call.args[0] for call in dial.call_args_list], [address])
        self.assertNotIn(PRIVATE, json.dumps(value))

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
            {**KEYS, "extra": PRIVATE}, {"exa": KEYS['exa']}, {"exa": None, "tavily": ""},
            {"exa": "", "tavily": ""}, {**KEYS, "exa": False},
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

    def test_optional_missing_key_skips_http_and_emits_only_explicit_missing_status(self):
        for provider in remote.PROVIDERS:
            for missing in (None, ""):
                keys = {name: KEYS[name] if name == provider else missing for name in remote.PROVIDERS}
                output = io.StringIO()
                with patch.object(sys, 'argv', ['validation']), \
                        patch.object(sys, 'stdin', type('Input', (), {'buffer': io.BytesIO(json.dumps(keys).encode())})()), \
                        patch.object(remote, 'probe', return_value=remote.result(provider, status=200)) as probe, \
                        contextlib.redirect_stdout(output):
                    self.assertEqual(remote.main(), 0)
                    probe.assert_called_once_with(provider, KEYS[provider])
                records = [json.loads(line) for line in output.getvalue().splitlines()]
                self.assertEqual(records, [remote.result(name, status=200) if name == provider
                                           else remote.result(name, 'credential_missing') for name in remote.PROVIDERS])
                for key in KEYS.values():
                    self.assertNotIn(key, output.getvalue())

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
    def test_proxy_is_selected_exa_only_encrypted_stdin_and_never_remote_argv_or_output(self):
        proxy = 'tls://relay.example:443'
        for selected in ('exa', 'tavily'):
            environment = {selected.upper() + '_API_KEY': KEYS[selected], 'EXA_PROXY_URL': proxy}
            records = [remote.result(provider, status=200) if provider == selected else remote.result(provider, 'credential_missing')
                       for provider in remote.PROVIDERS]
            output = io.StringIO()
            with patch.dict(os.environ, environment, clear=True), patch.object(sys, 'argv', ['runner']), \
                    patch.object(runner, 'invoke', return_value=(records, 0)) as invoke, contextlib.redirect_stdout(output):
                self.assertEqual(runner.main(), 0)
            source, payload = invoke.call_args.args
            decoded = json.loads(payload)
            self.assertEqual(decoded, {'exa': KEYS['exa'] if selected == 'exa' else None,
                                       'tavily': KEYS['tavily'] if selected == 'tavily' else None,
                                       **({'exa_proxy_url': proxy} if selected == 'exa' else {})})
            self.assertNotIn(proxy, source); self.assertNotIn(proxy, runner.remote_command(source))
            for secret in (*KEYS.values(), proxy):
                self.assertNotIn(secret, output.getvalue())

    def test_malformed_or_unsupported_proxy_fails_before_ssh(self):
        for proxy, expected in [('tls://user:SYNTHETIC-PROXY-PRIVATE@relay.example:443', 'ssh_failed'),
                                ('tls://relay.example:443/path', 'ssh_failed'),
                                ('https://relay.example:443', 'unsupported_proxy')]:
            output = io.StringIO()
            with patch.dict(os.environ, {'EXA_API_KEY': KEYS['exa'], 'EXA_PROXY_URL': proxy}, clear=True), \
                    patch.object(sys, 'argv', ['runner']), patch.object(runner, 'invoke') as invoke, \
                    contextlib.redirect_stdout(output):
                self.assertEqual(runner.main(), 1); invoke.assert_not_called()
            self.assertEqual(json.loads(output.getvalue().splitlines()[0])['result'], expected)
            self.assertNotIn('SYNTHETIC-PROXY-PRIVATE', output.getvalue())
            self.assertNotIn(KEYS['exa'], output.getvalue())

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
        with patch.dict(os.environ, {}, clear=True), patch.object(runner.subprocess, 'Popen', return_value=process) as popen:
            records, code = runner.invoke('print("reviewed source")', payload)
        command = popen.call_args.args[0]
        self.assertEqual(command[-2], 'maxposty-deploy@178.159.94.83')
        self.assertNotIn('77.91.94.235', json.dumps(command))
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

    def test_reviewed_backend_target_overrides_preserve_pinned_ssh(self):
        environment = {'VPS_HOST': 'backend.example.com', 'VPS_PORT': '2222', 'VPS_USER': 'backend-deploy'}
        stdout = b''.join(json.dumps(remote.result(provider, status=200)).encode() + b'\n' for provider in remote.PROVIDERS)
        class Process:
            def __init__(self):
                self.stdin, self.stdout = io.BytesIO(), io.BytesIO(stdout)
            def poll(self): return 0
            def wait(self, timeout): return 0
        with patch.dict(os.environ, environment, clear=True), \
                patch.object(runner.subprocess, 'Popen', return_value=Process()) as popen:
            runner.invoke('print("reviewed source")', json.dumps(KEYS).encode())
        command = popen.call_args.args[0]
        self.assertEqual(command[-2], 'backend-deploy@backend.example.com')
        self.assertEqual(command[command.index('-p') + 1], '2222')
        self.assertIn('StrictHostKeyChecking=yes', command)
        self.assertIn('IdentitiesOnly=yes', command)
        self.assertIn('UserKnownHostsFile=', ' '.join(command))

    def test_invalid_target_variables_fail_before_ssh_or_secret_output(self):
        invalid = {'VPS_HOST': ('', '-option', 'host.example;command', 'host..example', 'https://host.example', 'host\n' + PRIVATE),
                   'VPS_PORT': ('', '0', '022', '65536', '22;command'),
                   'VPS_USER': ('', '-option', 'root@host.example', 'user\n' + PRIVATE)}
        for name, values in invalid.items():
            for value in values:
                output = io.StringIO()
                with patch.dict(os.environ, {'EXA_API_KEY': KEYS['exa'], name: value}, clear=True), \
                        patch.object(sys, 'argv', ['runner']), patch.object(runner, 'invoke') as invoke, \
                        contextlib.redirect_stdout(output):
                    self.assertEqual(runner.main(), 1)
                    invoke.assert_not_called()
                for secret in (*KEYS.values(), PRIVATE):
                    self.assertNotIn(secret, output.getvalue())

    def test_http_200_required_for_every_configured_provider_and_missing_is_explicit(self):
        success = [remote.result(provider, status=200) for provider in remote.PROVIDERS]
        missing = [remote.result('exa', status=200), remote.result('tavily', 'credential_missing')]
        cases = [(KEYS, success, 0, 0), ({'exa': KEYS['exa']}, missing, 0, 0),
                 ({'exa': KEYS['exa'], 'tavily': ''}, missing, 0, 0), (KEYS, success, 1, 1),
                 (KEYS, missing, 0, 1), ({'exa': KEYS['exa']}, success, 0, 1)]
        for provider in remote.PROVIDERS:
            for record in (remote.result(provider, status=403), remote.result(provider, status=429),
                           remote.result(provider, 'tls'), remote.result(provider, 'timeout')):
                cases.append((KEYS, [record if name == provider else remote.result(name, status=200)
                                     for name in remote.PROVIDERS], 0, 1))
        for keys, records, ssh_code, expected in cases:
            environment = {name.upper() + '_API_KEY': value for name, value in keys.items()}
            output = io.StringIO()
            with patch.dict(os.environ, environment, clear=True), patch.object(sys, 'argv', ['runner']), \
                    patch.object(runner, 'invoke', return_value=(records, ssh_code)), contextlib.redirect_stdout(output):
                self.assertEqual(runner.main(), expected)
            self.assertEqual([json.loads(line) for line in output.getvalue().splitlines()], records)
            for key in KEYS.values():
                self.assertNotIn(key, output.getvalue())

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
        self.assertIn("options: [exa, tavily]", workflow)
        self.assertIn("default: exa", workflow)
        self.assertIn("VPS_HOST: ${{ vars.VPS_HOST || '178.159.94.83' }}", workflow)
        self.assertIn("VPS_PORT: ${{ vars.VPS_PORT || '22' }}", workflow)
        self.assertIn("VPS_USER: ${{ vars.VPS_USER || 'maxposty-deploy' }}", workflow)
        self.assertIn("EXA_API_KEY: ${{ inputs.provider == 'exa' && secrets.EXA_API_KEY || '' }}", workflow)
        self.assertIn("EXA_PROXY_URL: ${{ inputs.provider == 'exa' && secrets.EXA_PROXY_URL || '' }}", workflow)
        self.assertIn("TAVILY_API_KEY: ${{ inputs.provider == 'tavily' && secrets.TAVILY_API_KEY || '' }}", workflow)
        for forbidden in ('  push:', '  schedule:', 'OPENAI_API_KEY', 'upload-artifact',
                          'ssh-keyscan', '.env.production', 'scp ', 'contents: write', 'packages: write'):
            self.assertNotIn(forbidden, workflow)


if __name__ == '__main__':
    unittest.main()
