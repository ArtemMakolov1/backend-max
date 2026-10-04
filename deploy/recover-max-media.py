#!/usr/bin/env python3
"""One-time exact-byte media recovery. Inventory is read-only; apply is explicit.

MAX: https://dev.max.ru/docs-api/methods/GET/messages/-messageId-
R2 conditional PUT: https://developers.cloudflare.com/r2/api/s3/api/
Never log messages, attachment tokens, signed URLs, storage keys or credentials.
"""

import argparse
import datetime
import hashlib
import hmac
import http.client
import ipaddress
import json
import os
from pathlib import Path
import re
import socket
import ssl
import stat
import subprocess
import sys
import time
from urllib.parse import quote, urlsplit

MAX_HOST = "platform-api2.max.ru"
R2_HOST = "97b27ab0a14bfe63909b26f167e99999.r2.cloudflarestorage.com"
R2_BUCKET = "maxposty-media-production"
CA_SHA256 = "936a43fea6e8e525bcc0f81acd9c3d21b4fc4b9b68acea7906d698005afc6504"
KEY = re.compile(r"^([0-9a-f]{64})\.(jpg|jpeg|png|gif|mp4|webm)$")
MESSAGE_ID = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,255}$")
VIDEO_TOKEN = re.compile(r"^[A-Za-z0-9_-]{1,2048}$")
MIMES = {"jpg": "image/jpeg", "jpeg": "image/jpeg", "png": "image/png",
         "gif": "image/gif", "mp4": "video/mp4", "webm": "video/webm"}
MAX_TOTAL_BYTES = 500 << 20
MAX_JSON_BYTES = 2 << 20

INVENTORY_SQL = """
BEGIN TRANSACTION READ ONLY;
SET LOCAL statement_timeout='15s';
SET LOCAL lock_timeout='2s';
WITH refs AS (
 SELECT storage_key AS filename FROM post_attachments
 UNION SELECT image_path FROM posts WHERE image_path<>''
), assets AS (
 SELECT ma.filename AS key,MIN(ma.size_bytes) AS size,MAX(ma.size_bytes) AS max_size,
        bool_and(ma.state='ready') AS ready
 FROM media_assets ma JOIN refs r USING(filename) GROUP BY ma.filename
), messages AS (
 SELECT DISTINCT p.max_message_id AS id,c.max_chat_id AS chat_id
 FROM posts p JOIN channels c ON c.id=p.channel_id AND c.workspace_id=p.workspace_id
 WHERE p.status='published' AND p.max_message_id<>''
)
SELECT json_build_object(
 'assets',COALESCE((SELECT json_agg(assets ORDER BY key) FROM assets),'[]'::json),
 'messages',COALESCE((SELECT json_agg(messages ORDER BY id,chat_id) FROM messages),'[]'::json),
 'missing_metadata',(SELECT COUNT(*) FROM refs r WHERE NOT EXISTS
                      (SELECT 1 FROM assets a WHERE a.key=r.filename))
);
ROLLBACK;
"""


class RecoveryError(Exception):
    """Only fixed, non-sensitive diagnostics may escape to stdout/stderr."""


def private_file(path):
    path = Path(path)
    info = path.lstat()
    if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o077:
        raise RecoveryError("Expected an operator-owned private regular file")
    return path


def load_env(path):
    result = {}
    for line in private_file(path).read_text().splitlines():
        if not line or line.startswith("#"):
            continue
        name, sep, value = line.partition("=")
        if not sep or name in result:
            raise RecoveryError("Invalid private environment")
        result[name] = value
    token = result.get("MAX_BOT_TOKEN", "")
    if not token or any(ord(c) < 33 or ord(c) > 126 for c in token):
        raise RecoveryError("MAX credentials are unavailable")
    return result


def read_inventory():
    command = ["docker", "exec", "-i", "maxposty-backend-postgres-1", "sh", "-ec",
               'PGPASSWORD="$POSTGRES_PASSWORD" exec psql -X --no-password '
               '--username="$POSTGRES_USER" --dbname="$POSTGRES_DB" '
               '--quiet --tuples-only --no-align -v ON_ERROR_STOP=1']
    completed = subprocess.run(command, input=INVENTORY_SQL.encode(), capture_output=True,
                               timeout=25, check=False)
    if completed.returncode or len(completed.stdout) > MAX_JSON_BYTES:
        raise RecoveryError("Read-only database inventory failed; details withheld")
    try:
        return validate_inventory(json.loads(completed.stdout))
    except (ValueError, KeyError, TypeError):
        raise RecoveryError("Invalid read-only media inventory") from None


def validate_inventory(data):
    assets, messages = data["assets"], data["messages"]
    if data["missing_metadata"] or len(assets) > 1000 or len(messages) > 1000:
        raise RecoveryError("Media inventory is incomplete or exceeds recovery bounds")
    targets = {}
    for asset in assets:
        key, size = asset["key"], asset["size"]
        match = KEY.fullmatch(key)
        if (not match or type(size) is not int or size <= 0 or size != asset["max_size"]
                or asset["ready"] is not True or key in targets):
            raise RecoveryError("Media metadata is not safe for exact-byte recovery")
        mime = MIMES[match[2]]
        if size > (250 << 20 if mime.startswith("video/") else 50 << 20):
            raise RecoveryError("Media object exceeds its application size bound")
        targets[key] = {"size": size, "mime": mime}
    if sum(item["size"] for item in targets.values()) > MAX_TOTAL_BYTES:
        raise RecoveryError("Media inventory exceeds total recovery byte bound")
    seen = {}
    for message in messages:
        mid, chat = message["id"], message["chat_id"]
        if not MESSAGE_ID.fullmatch(mid) or not re.fullmatch(r"-?[0-9]{1,19}", chat):
            raise RecoveryError("Invalid published-message metadata")
        if mid in seen and seen[mid] != chat:
            raise RecoveryError("Ambiguous published-message ownership")
        seen[mid] = chat
    return targets, seen


def validate_url(url):
    if not isinstance(url, str) or len(url) > 16384 or any(ord(c) <= 32 for c in url) or "\\" in url:
        raise RecoveryError("Rejected unsafe provider media URL")
    parsed = urlsplit(url)
    if (parsed.scheme != "https" or parsed.username is not None or parsed.password is not None
            or parsed.fragment or parsed.port not in (None, 443) or not parsed.hostname
            or not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9.-]{0,252}", parsed.hostname)
            or parsed.hostname.endswith(".") or ".." in parsed.hostname):
        raise RecoveryError("Rejected unsafe provider media URL")
    return parsed


def public_addresses(host):
    results = socket.getaddrinfo(host, 443, type=socket.SOCK_STREAM)
    addresses = list(dict.fromkeys(item[4][0] for item in results))
    def safe(address):
        ip = ipaddress.ip_address(address)
        # Older VPS Python versions classify a few special-purpose ranges
        # differently; explicitly exclude mapped/tunnel and non-unicast IPs.
        if (not ip.is_global or ip.is_multicast or ip.is_reserved or ip.is_loopback
                or ip.is_link_local or ip.is_unspecified):
            return False
        if ip.version == 6 and (ip.ipv4_mapped is not None or ip.sixtofour is not None or ip.teredo is not None):
            return False
        return ip.version != 4 or ip not in ipaddress.ip_network("192.0.0.0/24")
    if not addresses or any(not safe(address) for address in addresses):
        raise RecoveryError("Rejected non-public provider media destination")
    return addresses


class PinnedHTTPSConnection(http.client.HTTPSConnection):
    def __init__(self, host, address, context):
        super().__init__(host, port=443, timeout=20, context=context)
        self.address = address

    def connect(self):
        # Resolve once, reject every non-public answer, and use that exact IP.
        # TLS still validates the original hostname; proxies/redirects are unused.
        raw = socket.create_connection((self.address, 443), timeout=self.timeout)
        try:
            self.sock = self._context.wrap_socket(raw, server_hostname=self.host)
        except BaseException:
            raw.close()
            raise


class Transport:
    def __init__(self, ca_path):
        self.system_context = ssl.create_default_context()
        self.max_context = ssl.create_default_context()
        pem = Path(ca_path).read_bytes()
        if hashlib.sha256(pem).hexdigest() != CA_SHA256:
            raise RecoveryError("Official MAX CA fingerprint does not match")
        self.max_context.load_verify_locations(cadata=pem.decode("ascii"))
        for context in (self.system_context, self.max_context):
            context.minimum_version = ssl.TLSVersion.TLSv1_2
        self.deadline = time.monotonic() + 900
        self.bytes_received = 0

    def request(self, method, url, headers=None, body=None, limit=MAX_JSON_BYTES, sink=None,
                max_trust=False):
        parsed = validate_url(url)
        addresses = public_addresses(parsed.hostname)
        context = self.max_context if max_trust else self.system_context
        connection = PinnedHTTPSConnection(parsed.hostname, addresses[0], context)
        supplied = dict(headers or {})
        supplied["Accept-Encoding"] = "identity"
        try:
            if time.monotonic() > self.deadline:
                raise RecoveryError("Recovery request deadline exceeded")
            path = parsed.path or "/"
            if parsed.query:
                path += "?" + parsed.query
            connection.request(method, path, body=body, headers=supplied)
            response = connection.getresponse()
            status = response.status
            if method == "HEAD" or status != 200:
                # Never follow redirects or retain/log upstream error bodies.
                return status, b""
            if response.getheader("Content-Encoding", "identity") != "identity":
                raise RecoveryError("Provider media response has unsupported content encoding")
            length = response.getheader("Content-Length")
            if length is not None and (not length.isdigit() or int(length) > limit):
                raise RecoveryError("Provider response exceeds its recovery size bound")
            if length is not None and int(length) > MAX_TOTAL_BYTES - self.bytes_received:
                raise RecoveryError("Recovery exceeded its total transfer byte bound")
            output, total = [], 0
            while True:
                if time.monotonic() > self.deadline:
                    raise RecoveryError("Recovery request deadline exceeded")
                chunk = response.read(min(65536, limit - total + 1,
                                          MAX_TOTAL_BYTES - self.bytes_received + 1))
                if not chunk:
                    break
                total += len(chunk)
                if total > limit:
                    raise RecoveryError("Provider response exceeds its recovery size bound")
                self.bytes_received += len(chunk)
                if self.bytes_received > MAX_TOTAL_BYTES:
                    raise RecoveryError("Recovery exceeded its total transfer byte bound")
                if sink is None:
                    output.append(chunk)
                else:
                    sink.write(chunk)
            return status, b"".join(output)
        finally:
            connection.close()


def api_get(transport, token, path):
    # Only read-only MAX JSON requests have a retry budget. A conditional R2
    # write with an uncertain outcome is never automatically replayed.
    for attempt in range(3):
        status, body = transport.request("GET", "https://" + MAX_HOST + path,
                                         {"Authorization": token}, max_trust=True)
        if (status == 429 or 500 <= status <= 599) and attempt < 2:
            time.sleep((1, 4)[attempt])
            continue
        break
    if status == 401:
        raise RecoveryError("MAX authorization failed; recovery stopped")
    if status != 200:
        return status, None
    try:
        payload = json.loads(body)
    except ValueError:
        raise RecoveryError("MAX returned invalid JSON; private response withheld") from None
    if not isinstance(payload, dict):
        raise RecoveryError("MAX returned an unexpected response shape")
    return status, payload


def candidate_urls(transport, token, message):
    body = message.get("body") or {}
    if not isinstance(body, dict):
        raise RecoveryError("MAX returned an unexpected message body")
    attachments = body.get("attachments") or []
    if not isinstance(attachments, list) or len(attachments) > 100:
        raise RecoveryError("MAX returned an unexpected attachment collection")
    for attachment in attachments:
        if not isinstance(attachment, dict):
            continue
        kind, payload = attachment.get("type"), attachment.get("payload") or {}
        if not isinstance(payload, dict):
            continue
        if kind == "image" and payload.get("url"):
            yield "image", payload["url"]
        elif kind == "video":
            video_token = payload.get("token")
            if not isinstance(video_token, str) or not VIDEO_TOKEN.fullmatch(video_token):
                continue
            _, details = api_get(transport, token, "/videos/" + quote(video_token, safe=""))
            if details is None or details.get("token") != video_token:
                continue
            urls = details.get("urls") or {}
            if not isinstance(urls, dict):
                continue
            for quality in ("mp4_1080", "mp4_720", "mp4_480", "mp4_360", "mp4_240", "mp4_144"):
                if urls.get(quality):
                    yield "video", urls[quality]


def file_identity(path):
    digest, size = hashlib.sha256(), 0
    with private_file(path).open("rb") as stream:
        for chunk in iter(lambda: stream.read(65536), b""):
            size += len(chunk)
            if size > 250 << 20:
                raise RecoveryError("Recovery cache object exceeds its size bound")
            digest.update(chunk)
    return digest.hexdigest(), size


def inventory(transport, env, state, targets, messages):
    report = {"target_keys": len(targets), "target_bytes": sum(a["size"] for a in targets.values()),
              "published_messages": len(messages), "messages_read": 0, "messages_unavailable": 0,
              "candidate_urls": 0, "candidates_downloaded": 0, "candidates_rejected": 0,
              "candidates_nonmatching": 0, "matched_keys": 0, "matched_bytes": 0}
    matches, seen_urls = set(), set()
    expected = {}
    for key, item in targets.items():
        expected.setdefault((key[:64], item["size"]), []).append(key)
    for mid, chat in messages.items():
        _, message = api_get(transport, env["MAX_BOT_TOKEN"], "/messages/" + quote(mid, safe=""))
        if message is None:
            report["messages_unavailable"] += 1
            continue
        body, recipient = message.get("body") or {}, message.get("recipient") or {}
        if not isinstance(body, dict) or not isinstance(recipient, dict) or body.get("mid") != mid or str(recipient.get("chat_id")) != chat:
            raise RecoveryError("MAX response does not match the requested published message")
        report["messages_read"] += 1
        for kind, url in candidate_urls(transport, env["MAX_BOT_TOKEN"], message):
            if not isinstance(url, str) or url in seen_urls:
                continue
            seen_urls.add(url)
            report["candidate_urls"] += 1
            if report["candidate_urls"] > 1000:
                raise RecoveryError("Provider media candidate count exceeds recovery bounds")
            limit = max((item["size"] for item in targets.values() if item["mime"].startswith(kind + "/")), default=0)
            temporary = state / ".candidate"
            try:
                if not limit:
                    continue
                with temporary.open("xb") as stream:
                    status, _ = transport.request("GET", url, limit=limit, sink=stream, max_trust=True)
                if status != 200:
                    report["candidates_rejected"] += 1
                    continue
                report["candidates_downloaded"] += 1
                identity = file_identity(temporary)
                keys = [key for key in expected.get(identity, []) if targets[key]["mime"].startswith(kind + "/")]
                if not keys:
                    report["candidates_nonmatching"] += 1
                    continue
                for key in keys:
                    if key not in matches:
                        # Keep independent private files for each original key;
                        # no hard links whose mutation could affect another key.
                        with temporary.open("rb") as source, (state / key).open("xb") as destination:
                            for chunk in iter(lambda: source.read(65536), b""):
                                destination.write(chunk)
                        matches.add(key)
            except (RecoveryError, OSError, ValueError, http.client.HTTPException):
                report["candidates_rejected"] += 1
            finally:
                temporary.unlink(missing_ok=True)
        time.sleep(0.05)
    report["matched_keys"] = len(matches)
    report["matched_bytes"] = sum(targets[key]["size"] for key in matches)
    manifest = {"version": 1, "targets": targets, "matches": sorted(matches), "report": report}
    (state / "manifest.json").write_text(json.dumps(manifest, sort_keys=True))
    return report


def r2_headers(env, method, key, payload=b"", extra=None, now=None):
    if env.get("S3_HOST", "").rstrip("/") != "https://" + R2_HOST or env.get("S3_BUCKET") != R2_BUCKET or env.get("S3_REGION") != "auto":
        raise RecoveryError("R2 configuration does not match the approved account and private bucket")
    now = now or datetime.datetime.now(datetime.timezone.utc)
    timestamp, day = now.strftime("%Y%m%dT%H%M%SZ"), now.strftime("%Y%m%d")
    digest = hashlib.sha256(payload).hexdigest()
    headers = {"host": R2_HOST, "x-amz-date": timestamp, "x-amz-content-sha256": digest}
    headers.update(extra or {})
    signed = ";".join(sorted(headers))
    canonical_headers = "".join(name + ":" + " ".join(headers[name].split()) + "\n" for name in sorted(headers))
    path = "/" + R2_BUCKET + "/" + quote(key, safe="-_.~")
    canonical = "\n".join([method, path, "", canonical_headers, signed, digest])
    scope = day + "/auto/s3/aws4_request"
    string_to_sign = "AWS4-HMAC-SHA256\n" + timestamp + "\n" + scope + "\n" + hashlib.sha256(canonical.encode()).hexdigest()
    signing = ("AWS4" + env["S3_SECRET_KEY"]).encode()
    for part in (day, "auto", "s3", "aws4_request"):
        signing = hmac.new(signing, part.encode(), hashlib.sha256).digest()
    signature = hmac.new(signing, string_to_sign.encode(), hashlib.sha256).hexdigest()
    headers["Authorization"] = "AWS4-HMAC-SHA256 Credential=" + env["S3_ACCESS_KEY"] + "/" + scope + ", SignedHeaders=" + signed + ", Signature=" + signature
    return "https://" + R2_HOST + path, headers


def apply(transport, env, state, current_targets):
    manifest = json.loads(private_file(state / "manifest.json").read_text())
    if manifest.get("version") != 1 or not isinstance(manifest.get("matches"), list) or len(manifest["matches"]) > 1000:
        raise RecoveryError("Invalid private recovery manifest")
    report = {"exact_keys": 0, "already_present": 0, "uploaded": 0, "verified_bytes": 0}
    for key in manifest["matches"]:
        if not isinstance(key, str) or not KEY.fullmatch(key) or current_targets.get(key) != manifest["targets"].get(key):
            raise RecoveryError("Current media ownership no longer matches the recovery manifest")
        target = current_targets[key]
        digest, size = file_identity(state / key)
        if digest != key[:64] or size != target["size"]:
            raise RecoveryError("Recovery file is not an exact original; upload blocked")
    for key in manifest["matches"]:
        target = current_targets[key]
        url, headers = r2_headers(env, "HEAD", key)
        status, _ = transport.request("HEAD", url, headers)
        if status == 404:
            payload = private_file(state / key).read_bytes()
            if hashlib.sha256(payload).hexdigest() != key[:64] or len(payload) != target["size"]:
                raise RecoveryError("Recovery cache changed; upload blocked")
            url, headers = r2_headers(env, "PUT", key, payload,
                                     {"if-none-match": "*", "content-type": target["mime"], "cache-control": "private, no-store"})
            status, _ = transport.request("PUT", url, headers, payload)
            if status not in (200, 412):
                raise RecoveryError("Conditional R2 restore failed; private response withheld")
            if status == 200:
                report["uploaded"] += 1
            else:
                report["already_present"] += 1
        elif status == 200:
            report["already_present"] += 1
        else:
            raise RecoveryError("R2 object preflight failed; private response withheld")
        url, headers = r2_headers(env, "GET", key)
        status, payload = transport.request("GET", url, headers, limit=target["size"])
        if status != 200 or len(payload) != target["size"] or hashlib.sha256(payload).hexdigest() != key[:64]:
            raise RecoveryError("R2 object differs from original; no overwrite or database change performed")
        report["exact_keys"] += 1
        report["verified_bytes"] += len(payload)
    return report


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("mode", choices=("inventory", "apply"))
    parser.add_argument("--env-file", required=True)
    parser.add_argument("--ca-file", required=True)
    parser.add_argument("--state-dir", required=True)
    arguments = parser.parse_args()
    os.umask(0o077)
    state = Path(arguments.state_dir)
    if not state.is_absolute():
        raise RecoveryError("Recovery cache must use an absolute private path")
    if arguments.mode == "inventory":
        state.mkdir(mode=0o700, parents=False, exist_ok=False)
    info = state.lstat()
    if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o077:
        raise RecoveryError("Recovery cache is not an operator-owned private directory")
    env = load_env(arguments.env_file)
    targets, messages = read_inventory()
    transport = Transport(arguments.ca_file)
    result = inventory(transport, env, state, targets, messages) if arguments.mode == "inventory" else apply(transport, env, state, targets)
    print(json.dumps({"mode": arguments.mode, "database_changed": False, **result}, sort_keys=True))


if __name__ == "__main__":
    try:
        main()
    except RecoveryError as error:
        print(str(error), file=sys.stderr)
        sys.exit(1)
    except Exception:
        print("Media recovery failed; private diagnostics withheld", file=sys.stderr)
        sys.exit(1)
