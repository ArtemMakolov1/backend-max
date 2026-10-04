"""Read-only, server-side allowlist for recent backend request failures.

Never print input errors, provider responses, SQL detail/rows or arbitrary JSON
fields. Unknown errors stay unknown. No log files or configuration are opened.
"""
import datetime
import ipaddress
import json
import os
from pathlib import Path
import re
import selectors
import subprocess
import sys
import time

CONTAINER = "maxposty-backend-backend-1"
MAX_BYTES = 2 * 1024 * 1024
MAX_LINES = 2000
MAX_LINE_BYTES = 65536
READ_TIMEOUT = 30
EDGE_NETWORK = "maxposty-edge"
COMMAND_TIMEOUT = 8
COMMAND_MAX_BYTES = 16384
HOST_PROVIDERS = ("exa", "tavily", "openai")
PROBE_RESULTS = frozenset(("dns", "tls", "timeout", "no_route", "unknown"))

# This subprocess runs on the host, not inside the backend. It has no inherited
# environment, credentials, redirects, proxy discovery or response-body reads.
HOST_PROBE_CODE = r'''
import errno, socket, ssl, sys, urllib.error, urllib.request
URLS = {
    "exa": "https://api.exa.ai/search",
    "tavily": "https://api.tavily.com/search",
    "openai": "https://api.openai.com/v1/models",
}
class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None
def classify(error):
    reason = error.reason if isinstance(error, urllib.error.URLError) else error
    if isinstance(reason, socket.gaierror): return "dns"
    if isinstance(reason, ssl.SSLError): return "tls"
    if isinstance(reason, TimeoutError): return "timeout"
    if isinstance(reason, OSError) and reason.errno in (errno.ENETUNREACH, errno.EHOSTUNREACH):
        return "no_route"
    return "unknown"
def probe(provider):
    if provider not in URLS: return "unknown"
    try:
        context = ssl.create_default_context()
        context.verify_mode = ssl.CERT_REQUIRED
        context.check_hostname = True
        opener = urllib.request.build_opener(
            urllib.request.ProxyHandler({}), NoRedirect(),
            urllib.request.HTTPSHandler(context=context))
        request = urllib.request.Request(URLS[provider], method="GET")
        with opener.open(request, timeout=5) as response:
            status = response.status
            return str(status) if type(status) is int and 100 <= status <= 599 else "unknown"
    except urllib.error.HTTPError as error:
        status = error.code
        error.close()
        return str(status) if type(status) is int and 100 <= status <= 599 else "unknown"
    except Exception as error:
        return classify(error)
if __name__ == "__main__":
    print(probe(sys.argv[1]) if len(sys.argv) == 2 else "unknown")
'''

# Exact source-owned strings only; the remainder of each wrapped error is
# deliberately discarded, even when it looks safe or resembles a SQL row.
PREFIXES = {
    "list content discovery publications": "discovery_samples",
    "lock workspace for MAX history write": "workspace_lock",
    "encode OpenAI Responses request": "provider_request_encoding",
    "create OpenAI Responses request": "provider_request_creation",
    "call OpenAI Responses API": "provider_transport",
    "read OpenAI Responses response": "provider_response_read",
    "decode OpenAI Responses response": "provider_response_json_decode",
    "decode structured post draft": "provider_structured_json_decode",
}
EXACT_ERRORS = {
    "invalid saved content analysis": "content_analysis_cache_decode",
    "invalid content analysis cache result": "content_analysis_cache_validation",
    "invalid discovery candidate batch": "discovery_candidates_validation",
    "invalid discovery candidate payload": "discovery_candidates_validation",
    "saved discovery candidate is invalid": "discovery_candidates_decode",
    "OpenAI Responses response is too large": "provider_response_size",
    "context deadline exceeded": "deadline_exceeded",
    "context canceled": "request_canceled",
    "sql: database is closed": "database_unavailable",
    "driver: bad connection": "database_unavailable",
}
SQLSTATES = {
    "08001", "08003", "08006", "08P01", "22001", "22021", "22023", "22P02",
    "23502", "23503", "23505", "23514", "25006", "25P02", "40001", "40P01",
    "42501", "42703", "42P01", "53300", "53400", "55P03", "57014", "XX000",
}
TABLES = frozenset((
    "content_discovery_candidates", "content_discovery_draft_operations",
    "content_analysis_cache", "channels", "workspaces", "workspace_members",
    "workspace_brand_kits", "workspace_templates", "posts", "post_attachments",
    "media_assets", "workspace_usage_counters", "users",
))
CONSTRAINTS = {
    "content_discovery_candidates_pkey": "content_discovery_candidates",
    "content_discovery_candidates_workspace_id_fkey": "content_discovery_candidates",
    "content_discovery_candidates_actor_user_id_fkey": "content_discovery_candidates",
    "content_discovery_candidates_payload_check": "content_discovery_candidates",
    "content_discovery_candidates_check": "content_discovery_candidates",
    "content_discovery_candidates_workspace_id_actor_user_id_id_key": "content_discovery_candidates",
    "content_discovery_candidates_workspace_id_channel_id_fkey": "content_discovery_candidates",
    "content_discovery_draft_operations_pkey": "content_discovery_draft_operations",
    "content_discovery_draft_opera_workspace_id_actor_user_id_c_fkey": "content_discovery_draft_operations",
    "content_analysis_cache_pkey": "content_analysis_cache",
    "content_analysis_cache_snapshot_key_check": "content_analysis_cache",
    "content_analysis_cache_claim_id_check": "content_analysis_cache",
    "content_analysis_cache_result_json_check": "content_analysis_cache",
    "content_analysis_cache_workspace_id_channel_id_fkey": "content_analysis_cache",
}
TRANSPORT_KINDS = frozenset((
    "dns", "tls_certificate_verification", "tls_handshake_timeout",
    "tls_handshake_failure", "connection_refused", "connection_reset",
    "connect_timeout", "connect_error", "no_route", "eof",
    "deadline_exceeded", "request_canceled", "response_header_timeout",
    "read_timeout", "write_timeout", "unknown",
))
# The API's exact research logger supplies status separately from the private
# error message. Zero means a response-validation failure, not an HTTP status.
PROVIDER_HTTP_STATUSES = frozenset((
    400, 401, 403, 404, 408, 409, 413, 415, 422, 429, 500, 502, 503, 504,
))
# Only canonical known provider codes and source-owned Responses validation
# codes may leave the server. Never infer a code from the private error text.
PROVIDER_CODES = {
    code: code for code in (
        "model_not_found", "invalid_api_key", "insufficient_quota",
        "unsupported_country_region_territory", "invalid_request_error",
        "unsupported_parameter", "unsupported_value", "invalid_value",
        "rate_limit_exceeded", "server_error",
        "missing_citations", "invalid_structured_output", "response_not_completed",
        "response_incomplete", "response_failed", "response_refused",
        "missing_output_text", "content_changed",
    )
}


def safe_transport_kind(error):
    """Classify Go transport failures without returning any input fragments."""
    # A net/url.Error embeds the requested URL in a quoted string. Remove only
    # that structural wrapper first, so a private URL/query cannot determine
    # the subtype. No URL, host, address or raw reason is ever returned.
    wrapped = re.fullmatch(
        r'(?:Get|Head|Post|Put|Patch|Delete|Connect|Options|Trace) "(?:[^"\\\r\n]|\\[^\r\n])*": ([^\r\n]+)', error
    )
    reason = wrapped.group(1) if wrapped else error
    if "\r" in reason or "\n" in reason:
        return "unknown"
    if re.fullmatch(r'(?:dial (?:tcp|tcp4|tcp6): )?lookup [^\r\n]+: (?:no such host|server misbehaving|i/o timeout|temporary failure in name resolution)', reason):
        return "dns"
    if reason.startswith("tls: failed to verify certificate: x509: ") or reason.startswith("x509: "):
        return "tls_certificate_verification"
    if reason == "net/http: TLS handshake timeout":
        return "tls_handshake_timeout"
    if reason.startswith("remote error: tls: ") or reason == "tls: handshake failure":
        return "tls_handshake_failure"
    if re.fullmatch(r'(?:[^\r\n]+: )?connection refused', reason):
        return "connection_refused"
    if re.fullmatch(r'(?:[^\r\n]+: )?connection reset by peer', reason):
        return "connection_reset"
    if re.fullmatch(r'(?:[^\r\n]+: )?(?:no route to host|network is unreachable)', reason):
        return "no_route"
    if re.fullmatch(r'dial (?:tcp|tcp4|tcp6) [^\r\n]+: (?:i/o timeout|connect: connection timed out)', reason):
        return "connect_timeout"
    if re.fullmatch(r'dial (?:tcp|tcp4|tcp6) [^\r\n]+: connect: [^\r\n]+', reason):
        return "connect_error"
    if reason in ("EOF", "unexpected EOF"):
        return "eof"
    if reason in (
        "context deadline exceeded",
        "context deadline exceeded (Client.Timeout exceeded while awaiting headers)",
        "net/http: request canceled (Client.Timeout exceeded while awaiting headers)",
    ):
        return "deadline_exceeded"
    if reason in ("context canceled", "net/http: request canceled"):
        return "request_canceled"
    if reason == "net/http: timeout awaiting response headers":
        return "response_header_timeout"
    if re.fullmatch(r'read (?:tcp|tcp4|tcp6) [^\r\n]+: i/o timeout', reason):
        return "read_timeout"
    if re.fullmatch(r'write (?:tcp|tcp4|tcp6) [^\r\n]+: i/o timeout', reason):
        return "write_timeout"
    return "unknown"


def safe_time(value):
    if not isinstance(value, str) or not re.fullmatch(
        r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,9})?(?:Z|[+-]\d{2}:\d{2})", value
    ):
        return None
    try:
        parsed = datetime.datetime.fromisoformat(value.replace("Z", "+00:00"))
        return parsed.astimezone(datetime.timezone.utc).isoformat().replace("+00:00", "Z")
    except (ValueError, OverflowError):
        return None


def sanitize_record(record):
    if not isinstance(record, dict):
        return None
    if record.get("msg") == "OpenAI research request failed":
        status = record.get("status")
        code = record.get("code")
        error = record.get("error")
        http_status = status if type(status) is int and status in PROVIDER_HTTP_STATUSES else None
        # One complete canonical error only: prefixes, suffixes, URLs, keys and
        # response-body fragments cannot produce a reason or cross the boundary.
        reason = "unsupported_region" if (
            http_status == 403 and isinstance(error, str)
            and error.strip().casefold() == "country, region, or territory not supported"
        ) else None
        return {
            "time": safe_time(record.get("time")),
            "category": "provider_result" if type(status) is int and status == 0 else "provider_http",
            "sqlstate": None, "table": None, "constraint": None,
            "prefix_chain": [], "transport_kind": None,
            "provider_status": http_status, "provider_reason": reason,
            "provider_code": PROVIDER_CODES.get(code) if isinstance(code, str) else None,
        }
    if record.get("msg") != "request failed":
        return None
    error = record.get("error")
    if not isinstance(error, str):
        error = ""
    remainder = error
    chain = []
    category = "unknown"
    for _ in range(4):
        prefix = next((value for value in PREFIXES if remainder.startswith(value + ": ")), None)
        if prefix is None:
            break
        chain.append(prefix)
        category = PREFIXES[prefix]
        remainder = remainder[len(prefix) + 2:]
    transport_kind = safe_transport_kind(remainder) if category == "provider_transport" else None
    category = EXACT_ERRORS.get(remainder, category)
    sqlstate = table = constraint = None
    # Only recognize the actual pgx Error() shape, never SQL-like text in a
    # transport error URL, response body, SQL DETAIL or unknown wrapper.
    match = re.fullmatch(r"ERROR: ([^\r\n]*) \(SQLSTATE ([A-Z0-9]{5})\)", remainder)
    if match and match.group(2) in SQLSTATES and category not in (
        "provider_transport", "provider_response_read", "provider_response_json_decode",
        "provider_request_creation", "provider_request_encoding", "provider_structured_json_decode",
    ):
        sqlstate = match.group(2)
        message = match.group(1)
        found_table = re.search(r'\b(?:table|relation) (?:"([a-z_]+)"|([a-z_]+)(?![a-z_0-9]))', message)
        found_constraint = re.search(r'\bconstraint "([a-z_]+)"', message)
        table_name = (found_table.group(1) or found_table.group(2)) if found_table else None
        if table_name in TABLES:
            table = table_name
        if found_constraint and found_constraint.group(1) in CONSTRAINTS:
            constraint = found_constraint.group(1)
            table = table or CONSTRAINTS[constraint]
        category = "database"
        if table == "content_analysis_cache":
            category = "content_analysis_cache_database"
        elif table == "content_discovery_candidates":
            category = "discovery_candidates_database"
        elif table == "content_discovery_draft_operations":
            category = "discovery_draft_database"
    return {
        "time": safe_time(record.get("time")), "category": category,
        "sqlstate": sqlstate, "table": table, "constraint": constraint,
        "prefix_chain": chain,
        "transport_kind": transport_kind,
    }


class BoundedDiagnostics:
    def __init__(self, emit):
        self.emit = emit
        self.bytes_read = self.lines_read = self.failures = 0
        self.buffer = bytearray()
        self.discard_line = False
        self.truncated = False
        self.warning_counts = {
            "content_source_retrieval_failed": 0,
            "openai_research_request_failed": 0,
            "openai_research_http_status_counts": {},
        }

    def feed(self, data):
        available = MAX_BYTES - self.bytes_read
        if len(data) >= available:
            self.truncated = True
        data = data[:available]
        self.bytes_read += len(data)
        for byte in data:
            if self.lines_read >= MAX_LINES:
                self.truncated = True
                break
            if byte == 10:
                self.lines_read += 1
                if not self.discard_line:
                    self._line(bytes(self.buffer))
                self.buffer.clear()
                self.discard_line = False
            elif len(self.buffer) < MAX_LINE_BYTES and not self.discard_line:
                self.buffer.append(byte)
            else:
                self.buffer.clear()
                self.discard_line = True
        return self.bytes_read < MAX_BYTES and self.lines_read < MAX_LINES

    def finish(self):
        if self.buffer and not self.discard_line and not self.truncated and self.lines_read < MAX_LINES:
            self.lines_read += 1
            self._line(bytes(self.buffer))
        self.buffer.clear()
        return {
            "window_minutes": 60, "bytes_read": self.bytes_read,
            "lines_read": self.lines_read, "request_failures": self.failures,
            "truncated": self.truncated,
            "warning_counts": self.warning_counts,
        }

    def _line(self, line):
        try:
            record = json.loads(line)
        except (ValueError, UnicodeDecodeError, RecursionError):
            return
        if isinstance(record, dict) and record.get("level") == "WARN":
            if record.get("msg") == "content source retrieval failed":
                self.warning_counts["content_source_retrieval_failed"] += 1
            elif record.get("msg") == "OpenAI research request failed":
                self.warning_counts["openai_research_request_failed"] += 1
                status = record.get("status")
                if type(status) is int and 100 <= status <= 599:
                    counts = self.warning_counts["openai_research_http_status_counts"]
                    key = str(status)
                    counts[key] = counts.get(key, 0) + 1
        safe = sanitize_record(record)
        if safe is not None:
            self.failures += 1
            self.emit(safe)


def emit(record):
    print(json.dumps(record, ensure_ascii=True, separators=(",", ":")), flush=True)


def bounded_command(arguments, max_bytes=COMMAND_MAX_BYTES, timeout=COMMAND_TIMEOUT, clean_env=False):
    """Return bounded stdout only; command errors and stderr never leave here."""
    process = None
    output = bytearray()
    complete = False
    read_status = "failed"
    try:
        process = subprocess.Popen(arguments, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                                   stderr=subprocess.DEVNULL, env={} if clean_env else None)
        with selectors.DefaultSelector() as selector:
            selector.register(process.stdout, selectors.EVENT_READ)
            deadline = time.monotonic() + timeout
            while True:
                remaining = deadline - time.monotonic()
                if remaining <= 0 or not selector.select(remaining):
                    read_status = "timeout"
                    break
                data = os.read(process.stdout.fileno(), min(4096, max_bytes + 1 - len(output)))
                if not data:
                    complete = True
                    break
                output.extend(data)
                if len(output) > max_bytes:
                    read_status = "bounded"
                    break
    except Exception:
        complete = False
    finally:
        if process is not None:
            if not complete and process.poll() is None:
                process.kill()  # Only this read-only client, never the container.
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                complete = False
                read_status = "timeout"
                process.kill()
                process.wait(timeout=5)
            if process.stdout is not None:
                process.stdout.close()
    if complete and process is not None and process.returncode == 0:
        return bytes(output), "complete"
    return None, read_status


def read_host_outbound_probes():
    results = []
    for provider in HOST_PROVIDERS:
        data, read_status = bounded_command([sys.executable, "-c", HOST_PROBE_CODE, provider], max_bytes=64,
                                            timeout=7, clean_env=True)
        result = {"provider": provider, "scope": "host", "result": "unknown", "http_status": None}
        if data is None:
            result["result"] = "timeout" if read_status == "timeout" else "unknown"
        elif re.fullmatch(rb"[1-5][0-9]{2}\n?", data):
            result.update(result="http", http_status=int(data))
        elif re.fullmatch(rb"(?:dns|tls|timeout|no_route|unknown)\n?", data):
            result["result"] = data.rstrip(b"\n").decode("ascii")
        results.append(result)
    return results


def ipv4(value):
    try:
        return ipaddress.IPv4Address(value) if isinstance(value, str) else None
    except ipaddress.AddressValueError:
        return None


def route_gateways(data):
    """Recognize only bounded BusyBox IPv4 defaults; never return route text."""
    if data is None:
        return None
    try:
        lines = data.decode("ascii").splitlines()
    except UnicodeDecodeError:
        return None
    if len(lines) > 32:
        return None
    gateways = []
    for line in lines:
        words = line.split()
        if not words or words.pop(0) != "default":
            return None
        gateway = None
        if words and words[0] == "via":
            if len(words) < 2 or ipv4(words[1]) is None:
                return None
            gateway = ipv4(words[1])
            words = words[2:]
        if len(words) < 2 or words[0] != "dev" or not re.fullmatch(r"[A-Za-z0-9_.-]{1,16}", words[1]):
            return None
        words = words[2:]
        while words:
            if words[0] == "onlink":
                words = words[1:]
                continue
            if len(words) < 2:
                return None
            name, value = words[:2]
            if not ((name == "src" and ipv4(value) is not None)
                    or (name == "metric" and re.fullmatch(r"[0-9]{1,10}", value))
                    or (name == "proto" and value in ("kernel", "boot", "static", "dhcp"))
                    or (name == "scope" and value in ("global", "link", "host"))):
                return None
            words = words[2:]
        gateways.append(gateway)
    return gateways


def read_container_network(container_id):
    result = {"edge_attached": None, "edge_internal": None,
              "default_route_is_edge": None, "default_routes_count": None}
    if not re.fullmatch(r"[0-9a-f]{64}", container_id):
        return result
    network_data, _ = bounded_command(["docker", "inspect", "--format",
                                       "{{json .NetworkSettings.Networks}}", container_id])
    internal_data, _ = bounded_command(["docker", "network", "inspect", "--format",
                                        "{{json .Internal}}", EDGE_NETWORK])
    # BusyBox 1.37.0 in the pinned runtime base supports these IPv4 route flags.
    route_data, _ = bounded_command(["docker", "exec", container_id, "/bin/busybox",
                                     "ip", "-4", "route", "show", "default"])
    if internal_data is not None and internal_data.strip() in (b"true", b"false"):
        result["edge_internal"] = internal_data.strip() == b"true"
    gateway = None
    try:
        networks = json.loads(network_data) if network_data is not None else None
        if isinstance(networks, dict) and len(networks) <= 32:
            result["edge_attached"] = EDGE_NETWORK in networks
            edge = networks.get(EDGE_NETWORK)
            gateway = ipv4(edge.get("Gateway")) if isinstance(edge, dict) else None
    except (ValueError, UnicodeDecodeError, RecursionError):
        pass
    routes = route_gateways(route_data)
    if routes is not None:
        result["default_routes_count"] = len(routes)
        if not routes or result["edge_attached"] is False:
            result["default_route_is_edge"] = False
        elif len(routes) == 1 and gateway is not None:
            result["default_route_is_edge"] = routes[0] == gateway
    return result


def validate_target(installation_dir):
    if not re.fullmatch(r"(?:/[A-Za-z0-9_-]+)+", installation_dir):
        raise ValueError("invalid_target")
    installation = Path(installation_dir)
    accepted = (installation / "current").resolve(strict=True)
    releases = (installation / "releases").resolve(strict=True)
    if not (installation / "current").is_symlink() or accepted.parent != releases:
        raise ValueError("invalid_release")
    if not re.fullmatch(r"[0-9a-f]{40}", accepted.name):
        raise ValueError("invalid_release_revision")
    if not (accepted / "deploy" / "compose.production.yaml").is_file():
        raise ValueError("missing_compose")
    result = subprocess.run(
        ["docker", "inspect", "--format", '{{index .Config.Labels "com.docker.compose.project"}}|{{index .Config.Labels "com.docker.compose.service"}}|{{index .Config.Labels "org.opencontainers.image.revision"}}|{{.Id}}', CONTAINER],
        stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, timeout=10, check=False,
    )
    identity = result.stdout.strip().split(b"|")
    if result.returncode != 0 or len(identity) != 4 or identity[:3] != [
        b"maxposty-backend", b"backend", accepted.name.encode("ascii")
    ] or not re.fullmatch(rb"[0-9a-f]{64}", identity[3]):
        raise ValueError("invalid_container")
    return identity[3].decode("ascii")


def read_diagnostics(container_id):
    records = []
    diagnostics = BoundedDiagnostics(records.append)
    process = subprocess.Popen(
        ["docker", "logs", "--since=1h", "--tail=2000", container_id],
        stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
    )
    timed_out = False
    try:
        with selectors.DefaultSelector() as selector:
            selector.register(process.stdout, selectors.EVENT_READ)
            deadline = time.monotonic() + READ_TIMEOUT
            while True:
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    timed_out = True
                    break
                events = selector.select(remaining)
                if not events:
                    timed_out = True
                    break
                data = os.read(process.stdout.fileno(), min(65536, MAX_BYTES - diagnostics.bytes_read))
                if not data or not diagnostics.feed(data):
                    break
    finally:
        if (timed_out or diagnostics.truncated) and process.poll() is None:
            process.kill()  # Stop only the log-reading client, never the container.
        try:
            process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            timed_out = True
            process.kill()
            process.wait(timeout=5)
        process.stdout.close()
    result = diagnostics.finish()
    result["read_status"] = "timeout" if timed_out else (
        "bounded" if diagnostics.truncated else "complete" if process.returncode == 0 else "failed"
    )
    return records, result


def main():
    try:
        if len(sys.argv) != 2 or sys.version_info < (3, 9):
            raise ValueError("unsupported_invocation")
        container_id = validate_target(sys.argv[1])
        records, result = read_diagnostics(container_id)
        if result["read_status"] in ("complete", "bounded"):
            result["container_network"] = read_container_network(container_id)
            result["outbound_probes"] = read_host_outbound_probes()
        # Retain only bounded, already-sanitized records in memory. A container
        # or accepted release change discards them before any remote output.
        if validate_target(sys.argv[1]) != container_id:
            raise ValueError("release_changed")
        if result["read_status"] in ("complete", "bounded"):
            for safe in records:
                emit(safe)
            emit(result)
            return 0
        emit({"read_status": result["read_status"]})
        return 1
    except Exception:
        # Exception text and tracebacks can include command output or secrets.
        emit({"read_status": "unavailable"})
        return 1


if __name__ == "__main__":
    sys.exit(main())
