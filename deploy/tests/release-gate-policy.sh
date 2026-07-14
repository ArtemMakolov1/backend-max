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
run_from_ci="$repo_root/deploy/run-from-ci.sh"

grep -Fq '  push:' "$workflow"
grep -Fq '    branches: [main]' "$workflow"
grep -Fq 'SOURCE_SHA: ${{ github.sha }}' "$workflow"
grep -Fq "./deploy/verify-release-gates.sh \"\$GITHUB_REPOSITORY\" \"\$SOURCE_SHA\"" "$workflow"
sed -n '/^      configure_max_webhook:/,/^permissions:/p' "$workflow" | grep -Fxq '        default: true'
grep -Fq "vars.DEPLOY_STAGE == 'production'" "$workflow"
grep -Fq "github.event_name == 'push'" "$workflow"
grep -Fq "github.event_name == 'workflow_dispatch' && inputs.configure_max_webhook" "$workflow"
if [[ $(grep -Fc 'CONFIGURE_MAX_WEBHOOK:' "$workflow") -ne 1 ]]; then
  echo "Webhook deploy policy must have one job-level source of truth" >&2
  exit 1
fi
grep -Fq "install -d -m 755 '\$install_dir/certs' '\$install_dir/hooks'" "$workflow"
grep -Fq 'install -d -m 755 "$installation_dir/certs"' "$run_from_ci"
if grep -Fq -- "-m 750 '\$install_dir/certs'" "$workflow" ||
  grep -Fq 'install -d -m 750 "$installation_dir/certs"' "$run_from_ci"; then
  echo "Public certificate directory must remain traversable by the container user" >&2
  exit 1
fi
if grep -Fq 'workflow_run:' "$workflow"; then
  echo "Production deploy must start directly for every main push" >&2
  exit 1
fi

activation_line=$(grep -nFx 'deployment_complete=true' "$run_from_ci" | cut -d: -f1)
webhook_line=$(grep -nF 'setup-max-webhook' "$run_from_ci" | cut -d: -f1)
if [[ -z "$activation_line" || -z "$webhook_line" ]] || ((webhook_line <= activation_line)); then
  echo "Webhook reconciliation must run only after a healthy release is accepted" >&2
  exit 1
fi
grep -Fq 'MAX webhook cannot be configured during the fail-closed HTTP bootstrap stage' "$run_from_ci"

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
