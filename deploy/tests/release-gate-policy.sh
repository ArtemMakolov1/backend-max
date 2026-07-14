#!/usr/bin/env bash
set -euo pipefail

repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
fixture=$(mktemp -d)
trap 'rm -rf "$fixture"' EXIT

ln -s "$repo_root/deploy/tests/fake-gh.sh" "$fixture/gh"
ln -s "$(type -P jq)" "$fixture/jq"
ln -s "$(type -P sleep)" "$fixture/sleep"

sha=0123456789abcdef0123456789abcdef01234567
wrong_sha=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
workflow="$repo_root/.github/workflows/deploy.yml"

grep -Fq '  push:' "$workflow"
grep -Fq '    branches: [main]' "$workflow"
grep -Fq 'SOURCE_SHA: ${{ github.sha }}' "$workflow"
grep -Fq "./deploy/verify-release-gates.sh \"\$GITHUB_REPOSITORY\" \"\$SOURCE_SHA\"" "$workflow"
grep -Fq "install -d -m 755 '\$install_dir/certs' '\$install_dir/hooks'" "$workflow"
grep -Fq 'install -d -m 755 "$installation_dir/certs"' "$repo_root/deploy/run-from-ci.sh"
if grep -Fq -- "-m 750 '\$install_dir/certs'" "$workflow" ||
  grep -Fq 'install -d -m 750 "$installation_dir/certs"' "$repo_root/deploy/run-from-ci.sh"; then
  echo "Public certificate directory must remain traversable by the container user" >&2
  exit 1
fi
if grep -Fq 'workflow_run:' "$workflow"; then
  echo "Production deploy must start directly for every main push" >&2
  exit 1
fi

PATH="$fixture:/usr/bin:/bin" MOCK_GATE_SHA="$sha" \
  "$repo_root/deploy/verify-release-gates.sh" owner/repository "$sha" 1 0 >/dev/null

if PATH="$fixture:/usr/bin:/bin" MOCK_GATE_SHA="$sha" MOCK_SECURITY_CONCLUSION=failure \
  "$repo_root/deploy/verify-release-gates.sh" owner/repository "$sha" 1 0 >/dev/null 2>&1; then
  echo "Release gate accepted a failed security workflow" >&2
  exit 1
fi

if PATH="$fixture:/usr/bin:/bin" MOCK_GATE_SHA="$wrong_sha" \
  "$repo_root/deploy/verify-release-gates.sh" owner/repository "$sha" 1 0 >/dev/null 2>&1; then
  echo "Release gate accepted successful workflows from a different commit" >&2
  exit 1
fi

if PATH="$fixture:/usr/bin:/bin" MOCK_GATE_SHA="$sha" \
  "$repo_root/deploy/verify-release-gates.sh" owner/repository "$sha" 361 0 >/dev/null 2>&1; then
  echo "Release gate accepted an unbounded attempt count" >&2
  exit 1
fi

echo "Release gate policy tests passed."
