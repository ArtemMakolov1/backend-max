"""Bounded storage inspection and reviewed, unused own-image removal only.

No environment files, container configuration, logs, volumes, containers or
application data are read or removed. Cleanup requires explicit inspected IDs.
"""
import argparse
import datetime
import fcntl
import itertools
import json
import os
from pathlib import Path
import re
import selectors
import stat
import subprocess
import sys
import time
import urllib.parse
import urllib.request

IMAGE_ID = re.compile(r"sha256:[0-9a-f]{64}\Z")
REPOSITORY = r"ghcr\.io/artemmakolov1/(?:backend-max|maxposty-[a-z0-9][a-z0-9._-]*)"
DIGEST_REF = re.compile(REPOSITORY + r"@sha256:[0-9a-f]{64}\Z")
TAG_REF = re.compile(REPOSITORY + r":[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}\Z")
TAGGED_DIGEST_REF = re.compile(REPOSITORY + r":[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}@sha256:[0-9a-f]{64}\Z")
BACKEND_REF = re.compile(r"ghcr\.io/artemmakolov1/backend-max@sha256:[0-9a-f]{64}\Z")
COMMIT = re.compile(r"[0-9a-f]{40}\Z")
SOURCE = "https://github.com/artemmakolov1/backend-max"
MAX_IMAGES = 512
MAX_CONTAINERS = 512
MAX_RELEASES = 256
MAX_OUTPUT = 4 * 1024 * 1024
COMMAND_TIMEOUT = 30
OPERATIONS = frozenset(("docker_command", "image_inspect", "image_inventory", "container_inventory",
                        "container_image", "release_image", "docker_root", "docker_usage", "image_remove"))


class MaintenanceError(Exception):
    """Only fixed, local diagnostic codes may cross the output boundary."""
    def __init__(self, code, operation="docker_command"):
        super().__init__(code)
        self.operation = operation if operation in OPERATIONS else "docker_command"


def emit(event, **fields):
    print(json.dumps({"event": event, **fields}, sort_keys=True), flush=True)


def run(argv, limit=MAX_OUTPUT, missing_ok=False, operation="docker_command"):
    """Never invoke a shell; cap stdout and discard all raw stderr."""
    if operation not in OPERATIONS:
        raise MaintenanceError("invalid_operation")
    try:
        process = subprocess.Popen(argv, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    except OSError:
        raise MaintenanceError("command_unavailable", operation) from None
    selector = selectors.DefaultSelector()
    selector.register(process.stdout, selectors.EVENT_READ)
    selector.register(process.stderr, selectors.EVENT_READ)
    result = bytearray()
    error_output = bytearray()
    deadline = time.monotonic() + COMMAND_TIMEOUT
    try:
        while selector.get_map():
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise MaintenanceError("command_timeout", operation)
            for key, _ in selector.select(min(remaining, 1)):
                chunk = os.read(key.fileobj.fileno(), 65536)
                if not chunk:
                    selector.unregister(key.fileobj)
                else:
                    destination = result if key.fileobj is process.stdout else error_output
                    destination.extend(chunk)
                    if len(result) > limit or len(error_output) > 65536:
                        raise MaintenanceError("command_output_limit", operation)
        if process.wait(timeout=max(0.1, deadline - time.monotonic())) != 0:
            # Only Docker's exact not-found error for this validated public
            # image reference is optional; daemon/auth/permission failures stop
            # selection. Raw error output is always discarded.
            expected = argv[-1].encode("ascii") if missing_ok else b""
            if missing_ok and bytes(error_output).strip() in (
                    b"Error response from daemon: No such image: " + expected,
                    b"Error: No such image: " + expected):
                return None
            raise MaintenanceError("command_failed", operation)
        return result.decode("utf-8", errors="strict")
    finally:
        selector.close()
        if process.poll() is None:
            process.kill()
        process.wait()
        process.stdout.close()
        process.stderr.close()


def inspect_image(image_id):
    if not IMAGE_ID.fullmatch(image_id):
        raise MaintenanceError("invalid_image_id")
    # Select fields inside Docker: Config.Env and other private fields never
    # leave the daemon, even temporarily in this process.
    template = ('{"Id":{{json .Id}},"RepoTags":{{json .RepoTags}},'
                '"RepoDigests":{{json .RepoDigests}},"Created":{{json .Created}},'
                '"Size":{{json .Size}},"Config":{"Labels":{'
                '"org.opencontainers.image.source":{{if .Config}}{{if .Config.Labels}}'
                '{{json (index .Config.Labels "org.opencontainers.image.source")}}'
                '{{else}}null{{end}}{{else}}null{{end}}}}}')
    image = json.loads(run(["docker", "image", "inspect", "--format", template, image_id], 65536, operation="image_inspect"))
    if not isinstance(image, dict) or image.get("Id") != image_id:
        raise MaintenanceError("invalid_image_inventory")
    return image


def image_ids():
    ids = sorted(set(run(["docker", "image", "ls", "--all", "--quiet", "--no-trunc"], 65536, operation="image_inventory").splitlines()))
    if len(ids) > MAX_IMAGES or any(not IMAGE_ID.fullmatch(value) for value in ids):
        raise MaintenanceError("image_inventory_limit")
    return ids


def container_image_ids():
    containers = run(["docker", "container", "ls", "--all", "--quiet", "--no-trunc"], 65536, operation="container_inventory").splitlines()
    if len(containers) > MAX_CONTAINERS or any(not re.fullmatch(r"[0-9a-f]{64}", value) for value in containers):
        raise MaintenanceError("container_inventory_limit")
    protected = set()
    for container in containers:
        image_id = run(["docker", "container", "inspect", "--format", "{{.Image}}", container], 256, operation="container_image").strip()
        if not IMAGE_ID.fullmatch(image_id):
            raise MaintenanceError("invalid_container_image")
        protected.add(image_id)
    return protected


def read_regular(path, limit):
    descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    try:
        if not stat.S_ISREG(os.fstat(descriptor).st_mode):
            raise MaintenanceError("invalid_release_file")
        content = os.read(descriptor, limit + 1)
        if len(content) > limit:
            raise MaintenanceError("release_file_limit")
        return content.decode("utf-8", errors="strict")
    finally:
        os.close(descriptor)


def release_refs(installation):
    """Preserve every retained bundle, a conservative superset of rollback."""
    installation = installation.resolve(strict=True)
    releases = installation / "releases"
    if releases.is_symlink() or not releases.is_dir():
        raise MaintenanceError("invalid_release_directory")
    current = installation / "current"
    if not current.is_symlink():
        raise MaintenanceError("missing_current_release")
    accepted = current.resolve(strict=True)
    if accepted.parent != releases or not COMMIT.fullmatch(accepted.name):
        raise MaintenanceError("invalid_current_release")
    entries = list(itertools.islice(releases.iterdir(), MAX_RELEASES + 1))
    if len(entries) > MAX_RELEASES:
        raise MaintenanceError("release_inventory_limit")
    protected = set()
    for release in entries:
        if not COMMIT.fullmatch(release.name):
            continue
        if release.is_symlink() or not release.is_dir():
            raise MaintenanceError("invalid_release_directory")
        metadata_found = False
        for name in (".release", ".release.next"):
            path = release / name
            if not path.exists() and not path.is_symlink():
                continue
            lines = read_regular(path, 4096).splitlines()
            backend = [line.removeprefix("BACKEND_IMAGE=") for line in lines if line.startswith("BACKEND_IMAGE=")]
            if len(backend) != 1 or not BACKEND_REF.fullmatch(backend[0]):
                raise MaintenanceError("invalid_release_image")
            protected.add(backend[0])
            metadata_found = True
        if release == accepted and not metadata_found:
            raise MaintenanceError("missing_current_image")
        compose = release / "deploy" / "compose.production.yaml"
        if (release / "deploy").is_symlink():
            raise MaintenanceError("invalid_release_directory")
        if compose.exists() or compose.is_symlink():
            for line in read_regular(compose, 128 * 1024).splitlines():
                match = re.fullmatch(r"\s+image:\s*(\S+)\s*", line)
                if match:
                    reference = match[1]
                    if DIGEST_REF.fullmatch(reference) or TAG_REF.fullmatch(reference):
                        protected.add(reference)
                    elif TAGGED_DIGEST_REF.fullmatch(reference):
                        tagged_repository, digest = reference.split("@")
                        repository = tagged_repository.rsplit(":", 1)[0]
                        protected.update((reference, tagged_repository, repository + "@" + digest))
    return protected


def classify_image(image, protected_ids, protected_refs, now):
    image_id = image.get("Id")
    if not isinstance(image_id, str) or not IMAGE_ID.fullmatch(image_id):
        return "invalid_image_id"
    if image_id in protected_ids:
        return "container_or_release_referenced"
    tags = image.get("RepoTags") or []
    digests = image.get("RepoDigests") or []
    if not isinstance(tags, list) or not isinstance(digests, list):
        return "invalid_references"
    if any(not isinstance(ref, str) or not TAG_REF.fullmatch(ref) for ref in tags):
        return "foreign_tag"
    if not digests or any(not isinstance(ref, str) or not DIGEST_REF.fullmatch(ref) for ref in digests):
        return "missing_or_foreign_digest"
    if protected_refs.intersection(tags + digests):
        return "release_or_candidate_referenced"
    config = image.get("Config") or {}
    labels = (config.get("Labels") or {}) if isinstance(config, dict) else {}
    source = labels.get("org.opencontainers.image.source") if isinstance(labels, dict) else None
    if not isinstance(source, str) or source.lower() != SOURCE:
        return "foreign_source"
    try:
        if not isinstance(image.get("Created"), str):
            return "invalid_creation_time"
        created = datetime.datetime.fromisoformat(image["Created"].replace("Z", "+00:00"))
        if created.tzinfo is None or (now - created).total_seconds() < 24 * 60 * 60:
            return "younger_than_24h"
    except (KeyError, TypeError, ValueError):
        return "invalid_creation_time"
    return "eligible"


def protection(installation, candidate, images):
    refs = release_refs(installation)
    if candidate:
        refs.add(candidate)
    ids = container_image_ids()
    # A release's OCI index digest and a local platform digest need not be
    # identical. Resolve every retained ref through Docker without pulling.
    for ref in sorted(refs):
        resolved = run(["docker", "image", "inspect", "--format", "{{.Id}}", ref], 256, missing_ok=True, operation="release_image")
        if resolved is None and ref == candidate:
            raise MaintenanceError("candidate_image_not_available")
        value = resolved.splitlines() if resolved is not None else []
        if any(not IMAGE_ID.fullmatch(item) for item in value):
            raise MaintenanceError("invalid_protected_image")
        ids.update(value)
    for image in images:
        if refs.intersection((image.get("RepoTags") or []) + (image.get("RepoDigests") or [])):
            ids.add(image["Id"])
    return ids, refs


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, request, response, code, message, headers, new_url):
        return None


def public_digest_exists(ref):
    """Anonymous HTTPS proves the exact retained registry digest is public."""
    if not DIGEST_REF.fullmatch(ref):
        return False
    repository, digest = ref.removeprefix("ghcr.io/").split("@")
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
    query = urllib.parse.urlencode({"service": "ghcr.io", "scope": "repository:" + repository + ":pull"})
    try:
        with opener.open("https://ghcr.io/token?" + query, timeout=8) as response:
            raw = response.read(65537)
            if len(raw) > 65536:
                return False
            token = json.loads(raw).get("token")
        if not isinstance(token, str) or not re.fullmatch(r"[A-Za-z0-9._~+/=-]{1,16384}", token):
            return False
        request = urllib.request.Request(
            "https://ghcr.io/v2/" + repository + "/manifests/" + digest,
            headers={"Authorization": "Bearer " + token, "Accept": "application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json,application/vnd.oci.image.manifest.v1+json,application/vnd.docker.distribution.manifest.v2+json"},
            method="HEAD")
        with opener.open(request, timeout=8) as response:
            return response.status == 200 and response.headers.get("Docker-Content-Digest") == digest
    except Exception:
        return False


def report_storage(installation):
    docker_root = run(["docker", "info", "--format", "{{.DockerRootDir}}"], 4096, operation="docker_root").strip()
    if not re.fullmatch(r"(/[A-Za-z0-9_.-]+)+", docker_root):
        raise MaintenanceError("invalid_docker_root")
    for scope, path in (("installation", installation), ("docker", docker_root)):
        space = os.statvfs(path)
        emit("filesystem", scope=scope, total_bytes=space.f_blocks * space.f_frsize,
             free_bytes=space.f_bfree * space.f_frsize, available_bytes=space.f_bavail * space.f_frsize,
             total_inodes=space.f_files, free_inodes=space.f_ffree, available_inodes=space.f_favail)
    records = run(["docker", "system", "df", "--format", "{{json .}}"], 65536, operation="docker_usage").splitlines()
    for raw in records:
        record = json.loads(raw)
        kind = record.get("Type")
        if kind not in ("Images", "Containers", "Local Volumes", "Build Cache"):
            raise MaintenanceError("invalid_usage_summary")
        safe = {}
        for key in ("TotalCount", "Active", "Size", "Reclaimable"):
            value = record.get(key)
            if not isinstance(value, (str, int)) or not re.fullmatch(r"[0-9A-Za-z., %()+-]{1,80}", str(value)):
                raise MaintenanceError("invalid_usage_summary")
            safe[key.lower()] = value
        emit("docker_usage", kind=kind, **safe)


def main(argv=None):
    parser = argparse.ArgumentParser()
    parser.add_argument("installation")
    parser.add_argument("mode", choices=("inspect", "cleanup_unused_images"), nargs="?", default="inspect")
    parser.add_argument("candidate_backend_image", nargs="?", default="")
    parser.add_argument("approved_image_ids", nargs="?", default="")
    args = parser.parse_args(argv)
    if not re.fullmatch(r"(/[A-Za-z0-9_-]+)+", args.installation):
        raise MaintenanceError("invalid_installation")
    installation = Path(args.installation).resolve(strict=True)
    if args.candidate_backend_image and not BACKEND_REF.fullmatch(args.candidate_backend_image):
        raise MaintenanceError("invalid_candidate_image")
    approved = args.approved_image_ids.split(",") if args.approved_image_ids else []
    if len(approved) > 64 or len(approved) != len(set(approved)) or any(not IMAGE_ID.fullmatch(value) for value in approved):
        raise MaintenanceError("invalid_approved_images")
    cleanup = args.mode == "cleanup_unused_images"
    if cleanup and (not approved or not args.candidate_backend_image):
        raise MaintenanceError("cleanup_requires_candidate_and_approved_ids")
    if not cleanup and approved:
        raise MaintenanceError("inspect_cannot_approve_images")
    descriptor = os.open(installation / ".deploy.lock", os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    try:
        if not stat.S_ISREG(os.fstat(descriptor).st_mode):
            raise MaintenanceError("invalid_deployment_lock")
        fcntl.flock(descriptor, (fcntl.LOCK_EX if cleanup else fcntl.LOCK_SH) | fcntl.LOCK_NB)
        report_storage(installation)
        images = [inspect_image(value) for value in image_ids()]
        protected_ids, protected_refs = protection(installation, args.candidate_backend_image, images)
        now = datetime.datetime.now(datetime.timezone.utc)
        eligible = {}
        counts = {}
        for image in images:
            reason = classify_image(image, protected_ids, protected_refs, now)
            counts[reason] = counts.get(reason, 0) + 1
            if reason == "eligible":
                if all(public_digest_exists(ref) for ref in image["RepoDigests"]):
                    eligible[image["Id"]] = image
                    emit("unused_image", image_id=image["Id"], public_refs=sorted((image.get("RepoTags") or []) + image["RepoDigests"]), size_bytes=image.get("Size", 0))
                else:
                    counts["public_digest_unverified"] = counts.get("public_digest_unverified", 0) + 1
        emit("inventory", images=len(images), protected_image_ids=len(protected_ids), eligible=len(eligible), classifications=counts)
        if cleanup:
            if any(value not in eligible for value in approved):
                raise MaintenanceError("approved_image_is_not_eligible")
            removed = 0
            for image_id in approved:
                # Refresh all containers/release refs and this image immediately
                # before removal. Docker's no-force removal is the final race
                # protection if a container is created after this snapshot.
                image = inspect_image(image_id)
                ids, refs = protection(installation, args.candidate_backend_image, [image])
                if classify_image(image, ids, refs, datetime.datetime.now(datetime.timezone.utc)) != "eligible" or not all(public_digest_exists(ref) for ref in image["RepoDigests"]):
                    raise MaintenanceError("image_protection_changed")
                try:
                    run(["docker", "image", "rm", "--no-prune", image_id], 65536, operation="image_remove")
                except MaintenanceError:
                    emit("image_retained", image_id=image_id, reason="docker_refused_no_force_removal")
                    continue
                removed += 1
                emit("image_removed", image_id=image_id, public_refs=sorted(image["RepoDigests"]))
            emit("cleanup_complete", approved=len(approved), removed=removed)
            report_storage(installation)
    finally:
        os.close(descriptor)


if __name__ == "__main__":
    try:
        main()
    except MaintenanceError as error:
        emit("maintenance_failed", code=str(error), operation=error.operation)
        sys.exit(1)
    except Exception:
        emit("maintenance_failed", code="inspection_failed")
        sys.exit(1)
