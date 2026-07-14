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

echo "Release gate policy tests passed."
