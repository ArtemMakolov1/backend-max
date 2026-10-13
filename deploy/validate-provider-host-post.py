"""One fixed POST per configured source; never read or print a response body."""
import contextlib
import errno
import http.client
import ipaddress
import json
import re
import signal
import socket
import ssl
import sys
import urllib.error
import urllib.parse
import urllib.request

PROVIDERS = ("exa", "tavily")
MAX_INPUT_BYTES = 16384
QUERY = "public information about the solar system"
RESULTS = frozenset(("http", "dns", "tls", "timeout", "no_route", "connection_refused",
                     "connection_reset", "unknown", "invalid_input", "credential_missing", "ssh_failed",
                     "unsupported_proxy"))
EXA_ENDPOINT = "https://api.exa.ai/search"
HOSTNAME = re.compile(r"[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*")


class UnsupportedProxy(ValueError):
    """The status-only host probe supports direct traffic or a TLS relay."""


def relay_address(raw):
    if not isinstance(raw, str) or len(raw) > 4096 or any(c in raw for c in "\r\n\t$\\\"'"):
        raise ValueError()
    raw = raw.strip()
    if not raw:
        return None
    if any(ord(c) <= 32 or ord(c) > 126 for c in raw):
        raise ValueError()
    if raw.startswith(('http://', 'https://')):
        raise UnsupportedProxy()
    if not re.fullmatch(r"tls://[^/?#@]+", raw):
        raise ValueError()
    url = urllib.parse.urlsplit(raw)
    if url.netloc.startswith('['):
        closing = url.netloc.find(']')
        if closing < 0 or not re.fullmatch(r':[0-9]+', url.netloc[closing + 1:]):
            raise ValueError()
    host = url.hostname
    if not host or len(host) > 253 or '%' in host or url.port is None or not 1 <= url.port <= 65535:
        raise ValueError()
    try:
        ipaddress.ip_address(host)
    except ValueError:
        if not HOSTNAME.fullmatch(host):
            raise ValueError() from None
    return host, url.port


@contextlib.contextmanager
def deadline(seconds):
    def expired(_signum, _frame):
        raise TimeoutError()
    previous = signal.signal(signal.SIGALRM, expired)
    signal.alarm(seconds)
    try:
        yield
    finally:
        signal.alarm(0)
        signal.signal(signal.SIGALRM, previous)


def credentials(data):
    if not isinstance(data, bytes) or len(data) > MAX_INPUT_BYTES:
        raise ValueError()
    def unique_object(pairs):
        value = {}
        for key, item in pairs:
            if key in value:
                raise ValueError()
            value[key] = item
        return value
    value = json.loads(data, object_pairs_hook=unique_object)
    if not isinstance(value, dict) or set(value) not in (set(PROVIDERS), {*PROVIDERS, "exa_proxy_url"}):
        raise ValueError()
    if any(key is not None and (not isinstance(key, str) or
           (key != "" and not re.fullmatch(r"[!-~]{1,2048}", key))) for key in (value[name] for name in PROVIDERS)):
        raise ValueError()
    if not any(value[name] for name in PROVIDERS):
        raise ValueError()
    if "exa_proxy_url" in value:
        proxy = value["exa_proxy_url"]
        if not isinstance(proxy, str) or (proxy and not value["exa"]):
            raise ValueError()
        relay_address(proxy)
    return value


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


class ExaRelayHTTPSHandler(urllib.request.HTTPSHandler):
    def __init__(self, address, context):
        super().__init__(context=context)
        self.address = address

    def https_open(self, req):
        if req.full_url != EXA_ENDPOINT:
            raise ValueError()
        def connection(host, **kwargs):
            if host != 'api.exa.ai':
                raise ValueError()
            value = http.client.HTTPSConnection(host, **kwargs)
            # HTTPSConnection still uses its origin host for Host, SNI and
            # certificate checks. Only this connection's TCP dial is replaced.
            value._create_connection = lambda _origin, timeout, source_address: socket.create_connection(
                self.address, timeout, source_address)
            return value
        return self.do_open(connection, req, context=self._context)


def request(provider, key):
    if provider == "exa":
        url = EXA_ENDPOINT
        body = {"query": QUERY, "type": "auto", "numResults": 1, "contents": {"highlights": True}}
        auth = {"x-api-key": key}
    elif provider == "tavily":
        url = "https://api.tavily.com/search"
        body = {"query": QUERY, "search_depth": "basic", "max_results": 1}
        auth = {"Authorization": "Bearer " + key}
    else:
        raise ValueError()
    return urllib.request.Request(url, json.dumps(body).encode("ascii"),
                                  {"Content-Type": "application/json", **auth}, method="POST")


def classify(error):
    reason = error.reason if isinstance(error, urllib.error.URLError) else error
    if isinstance(reason, socket.gaierror):
        return "dns"
    if isinstance(reason, ssl.SSLError):
        return "tls"
    if isinstance(reason, TimeoutError):
        return "timeout"
    if isinstance(reason, OSError):
        return {errno.ENETUNREACH: "no_route", errno.EHOSTUNREACH: "no_route",
                errno.ECONNREFUSED: "connection_refused", errno.ECONNRESET: "connection_reset"}.get(reason.errno, "unknown")
    return "unknown"


def result(provider, kind="unknown", status=None):
    valid = type(status) is int and 100 <= status <= 599
    return {"provider": provider, "scope": "host", "result": "http" if valid else kind,
            "http_status": status if valid else None}


def probe(provider, key, exa_proxy_url=""):
    try:
        address = relay_address(exa_proxy_url) if provider == 'exa' else None
        with deadline(20):  # Includes DNS, TLS, and response-header waits.
            context = ssl.create_default_context()
            context.minimum_version = ssl.TLSVersion.TLSv1_2
            context.verify_mode = ssl.CERT_REQUIRED
            context.check_hostname = True
            handler = ExaRelayHTTPSHandler(address, context) if address else urllib.request.HTTPSHandler(context=context)
            opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect(), handler)
            with opener.open(request(provider, key), timeout=15) as response:
                return result(provider, status=response.status)
    except UnsupportedProxy:
        return result(provider, "unsupported_proxy")
    except ValueError:
        return result(provider, "invalid_input")
    except urllib.error.HTTPError as error:
        status = error.code
        error.close()  # No read(), headers, reason, or provider body escapes.
        return result(provider, status=status)
    except Exception as error:
        return result(provider, classify(error))


def emit(records):
    for record in records:
        print(json.dumps(record, separators=(",", ":")), flush=True)


def unsupported_proxy_records():
    return [result('exa', 'unsupported_proxy'), result('tavily', 'invalid_input')]


def main():
    keys = {}
    try:
        if len(sys.argv) != 1 or sys.version_info < (3, 9):
            raise ValueError()
        with deadline(5):
            keys = credentials(sys.stdin.buffer.read(MAX_INPUT_BYTES + 1))
    except UnsupportedProxy:
        emit(unsupported_proxy_records())
        return 1
    except Exception:
        emit([result(provider, "invalid_input") for provider in PROVIDERS])
        return 1
    try:
        emit([(probe(provider, keys[provider], keys['exa_proxy_url'])
               if provider == 'exa' and keys.get('exa_proxy_url') else probe(provider, keys[provider]))
              if keys[provider] else result(provider, "credential_missing")
              for provider in PROVIDERS])
        return 0
    finally:
        keys.clear()


if __name__ == "__main__":
    sys.exit(main())
