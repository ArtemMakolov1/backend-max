"""Validate a deployment-owned Exa CONNECT proxy without disclosing userinfo."""
import ipaddress
import re
import sys
import urllib.parse

MAX_BYTES = 4096
HOSTNAME = re.compile(r"[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*")


def valid(raw):
    if len(raw) > MAX_BYTES or any(c in raw for c in "\r\n\t"):
        return False
    raw = raw.strip()
    if not raw:
        return True
    # Compose expands raw dollars in env files; quotes/backslashes also carry
    # env-file syntax. Encode those credential bytes in URL userinfo instead.
    if any(ord(c) <= 32 or ord(c) > 126 for c in raw) or any(c in raw for c in "$\\\"'"):
        return False
    if not re.fullmatch(r"https?://[^/?#]+", raw) or re.search(r"%(?![0-9A-Fa-f]{2})", raw):
        return False
    try:
        url = urllib.parse.urlsplit(raw)
        authority = url.netloc.rsplit('@', 1)[-1]
        if authority.startswith('['):
            closing = authority.find(']')
            if closing < 0 or not re.fullmatch(r'(?::[0-9]+)?', authority[closing + 1:]):
                return False
        host = url.hostname
        if not host or len(host) > 253 or "%" in host or url.netloc.endswith(":"):
            return False
        try:
            ipaddress.ip_address(host)
        except ValueError:
            if not HOSTNAME.fullmatch(host):
                return False
        if url.port is not None and not 1 <= url.port <= 65535:
            return False
        if url.username is not None:
            userinfo = url.netloc.rsplit('@', 1)[0]
            if not re.fullmatch(r'[A-Za-z0-9._~!&()*+,;=:%@-]+', userinfo):
                return False
            username = urllib.parse.unquote(url.username, errors="strict")
            password = urllib.parse.unquote(url.password or "", errors="strict")
            if not username or any(ord(c) < 32 or ord(c) > 126 for c in username + password):
                return False
        return True
    except (ValueError, UnicodeError):
        return False


def main():
    raw = sys.stdin.buffer.read(MAX_BYTES + 1)
    try:
        accepted = len(raw) <= MAX_BYTES and valid(raw.decode("ascii"))
    except UnicodeError:
        accepted = False
    if not accepted:
        print("EXA_PROXY_URL must be an authority-only HTTP(S) proxy URL with URI-encoded userinfo", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())

