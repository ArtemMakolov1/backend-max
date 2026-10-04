"""Runner-only: approved secrets travel solely as encrypted SSH stdin."""
import importlib.util
import json
import os
from pathlib import Path
import shlex
import subprocess
import sys

SOURCE = Path(__file__).with_name("validate-provider-host-post.py")
SPEC = importlib.util.spec_from_file_location("provider_host_validation", SOURCE)
validation = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(validation)
MAX_OUTPUT_BYTES = 4096


def remote_command(source):
    if not isinstance(source, str) or len(source.encode("utf-8")) > 32768:
        raise ValueError()
    return "env -i PATH=/usr/bin:/bin timeout -s KILL 50 python3 -c " + shlex.quote(source)


def validate_output(data):
    if len(data) > MAX_OUTPUT_BYTES:
        raise ValueError()
    records = [json.loads(line) for line in data.splitlines()]
    if len(records) != 2:
        raise ValueError()
    for provider, record in zip(validation.PROVIDERS, records):
        if not isinstance(record, dict) or set(record) != {"provider", "scope", "result", "http_status"}:
            raise ValueError()
        status = record["http_status"]
        kind = record["result"]
        if (record["provider"] != provider or record["scope"] != "host"
                or not isinstance(kind, str) or kind not in validation.RESULTS
                or (kind == "http" and not (type(status) is int and 100 <= status <= 599))
                or (kind != "http" and status is not None)):
            raise ValueError()
    return records


def invoke(source, payload):
    ssh_dir = Path.home() / ".ssh"
    command = ["ssh", "-F", "/dev/null", "-i", str(ssh_dir / "id_deploy"), "-p", "22",
               "-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes", "-o", "StrictHostKeyChecking=yes",
               "-o", "UserKnownHostsFile=" + str(ssh_dir / "known_hosts"), "-o", "ConnectTimeout=10",
               "-o", "ServerAliveInterval=10", "-o", "ServerAliveCountMax=2",
               "maxposty-deploy@77.91.94.235", remote_command(source)]
    process = None
    try:
        with validation.deadline(70):
            process = subprocess.Popen(command, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                       stderr=subprocess.DEVNULL, env={"PATH": "/usr/bin:/bin"})
            process.stdin.write(payload)
            process.stdin.close()
            data = process.stdout.read(MAX_OUTPUT_BYTES + 1)
            if len(data) > MAX_OUTPUT_BYTES:
                raise ValueError()
            return_code = process.wait(timeout=3)
            return validate_output(data), return_code
    finally:
        if process is not None:
            if process.poll() is None:
                process.kill()
            process.wait(timeout=3)
            for stream in (process.stdin, process.stdout):
                if stream is not None and not stream.closed:
                    stream.close()


def main():
    try:
        if len(sys.argv) != 1 or not SOURCE.is_file() or SOURCE.is_symlink():
            raise ValueError()
        # Only these two designated runner secrets are consulted. No server env,
        # runtime configuration, or other provider credentials are read.
        payload = json.dumps({"exa": os.environ.get("EXA_API_KEY"),
                              "tavily": os.environ.get("TAVILY_API_KEY")}).encode("ascii")
        validation.credentials(payload)
        source = SOURCE.read_text(encoding="utf-8")
        records, code = invoke(source, payload)
        validation.emit(records)
        return 0 if code == 0 else 1
    except Exception:
        validation.emit([validation.result(provider, "ssh_failed") for provider in validation.PROVIDERS])
        return 1


if __name__ == "__main__":
    sys.exit(main())
