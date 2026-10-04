"""One fixed authenticated POST per source; never read or print a response body."""
import contextlib
import errno
import json
import re
import signal
import socket
import ssl
import sys
import urllib.error
import urllib.request

PROVIDERS = ("exa", "tavily")
MAX_INPUT_BYTES = 16384
QUERY = "public information about the solar system"
RESULTS = frozenset(("http", "dns", "tls", "timeout", "no_route", "connection_refused",
                     "connection_reset", "unknown", "invalid_input", "ssh_failed"))


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
    if not isinstance(value, dict) or set(value) != set(PROVIDERS):
        raise ValueError()
    if any(not isinstance(key, str) or not re.fullmatch(r"[!-~]{1,2048}", key) for key in value.values()):
        raise ValueError()
    return value


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def request(provider, key):
    if provider == "exa":
        url = "https://api.exa.ai/search"
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


def probe(provider, key):
    try:
        with deadline(20):  # Includes DNS, TLS, and response-header waits.
            context = ssl.create_default_context()
            context.verify_mode = ssl.CERT_REQUIRED
            context.check_hostname = True
            opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect(),
                                                  urllib.request.HTTPSHandler(context=context))
            with opener.open(request(provider, key), timeout=15) as response:
                return result(provider, status=response.status)
    except urllib.error.HTTPError as error:
        status = error.code
        error.close()  # No read(), headers, reason, or provider body escapes.
        return result(provider, status=status)
    except Exception as error:
        return result(provider, classify(error))


def emit(records):
    for record in records:
        print(json.dumps(record, separators=(",", ":")), flush=True)


def main():
    keys = {}
    try:
        if len(sys.argv) != 1 or sys.version_info < (3, 9):
            raise ValueError()
        with deadline(5):
            keys = credentials(sys.stdin.buffer.read(MAX_INPUT_BYTES + 1))
    except Exception:
        emit([result(provider, "invalid_input") for provider in PROVIDERS])
        return 1
    try:
        emit([probe(provider, keys[provider]) for provider in PROVIDERS])
        return 0
    finally:
        keys.clear()


if __name__ == "__main__":
    sys.exit(main())
