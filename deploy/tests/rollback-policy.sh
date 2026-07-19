#!/usr/bin/env bash
set -euo pipefail

repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
new_sha=1111111111111111111111111111111111111111
old_sha=0000000000000000000000000000000000000000
image="ghcr.io/example/backend@sha256:$(printf 'a%.0s' {1..64})"

grep -F -- '../docker/postgres/init-app-role.sh:' "$repo_root/deploy/compose.production.yaml" >/dev/null
grep -F -- '../docker/pgbouncer/entrypoint.sh:' "$repo_root/deploy/compose.production.yaml" >/dev/null
if grep -F '/opt/maxposty/backend/docker/' "$repo_root/deploy/compose.production.yaml" >/dev/null; then
  echo "Release compose still depends on mutable shared helper scripts" >&2
  exit 1
fi

assert_file_absent() {
  local file=$1
  if [[ -e "$file" ]]; then
    echo "Rollback policy left a staged file behind: $file" >&2
    exit 1
  fi
}

assert_file_present() {
  local file=$1
  if [[ ! -f "$file" ]]; then
    echo "Rollback policy did not preserve required recovery metadata: $file" >&2
    exit 1
  fi
}

prepare_release() {
  local release_dir=$1
  install -d -m 750 "$release_dir"
  cp -R "$repo_root/deploy" "$release_dir/deploy"
  cp -R "$repo_root/docker" "$release_dir/docker"
  cp -R "$repo_root/monitoring" "$release_dir/monitoring"
}

render_bootstrap_env() {
  local output=$1
  DEPLOY_STAGE=bootstrap \
    POSTGRES_OWNER_PASSWORD=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa \
    POSTGRES_APP_PASSWORD=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb \
    POSTGRES_MONITOR_PASSWORD=cccccccccccccccccccccccccccccccc \
    GRAFANA_ADMIN_PASSWORD=dddddddddddddddddddddddddddddddd \
    GRAFANA_SECRET_KEY=eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee \
    "$repo_root/deploy/render-production-env.sh" "$output"
}

run_bootstrap_skips_installed_offsite_hook() {
  local sandbox installation_dir release_dir fake_bin output
  if ! mv --help 2>&1 | grep -q -- '-T'; then
    return 0
  fi
  sandbox=$(mktemp -d)
  sandbox=$(CDPATH='' cd -- "$sandbox" && pwd -P)
  installation_dir="$sandbox/backend"
  release_dir="$installation_dir/releases/$new_sha"
  fake_bin="$sandbox/bin"
  output="$sandbox/output.log"
  install -d "$fake_bin" "$sandbox/empty" "$installation_dir/hooks"
  prepare_release "$release_dir"
  render_bootstrap_env "$release_dir/.env.production.next"
  ln -s "$repo_root/deploy/tests/fake-docker.sh" "$fake_bin/docker"
  ln -s "$(type -P true)" "$fake_bin/flock"
  printf '#!/usr/bin/env sh\ntouch "$TEST_HOOK_CALLED"\nexit 99\n' >"$installation_dir/hooks/after-backup"
  chmod 755 "$installation_dir/hooks/after-backup"

  PATH="$fake_bin:$PATH" \
    TEST_LOG="$sandbox/docker.log" \
    TEST_SCENARIO=bootstrap-hook-skip \
    TEST_EMPTY_DIR="$sandbox/empty" \
    TEST_OLD_SHA="$old_sha" \
    TEST_HOOK_CALLED="$sandbox/hook-called" \
    "$release_dir/deploy/deploy-production.sh" "$image" >"$output" 2>&1

  [[ ! -e "$sandbox/hook-called" ]]
  [[ "$(readlink -f "$installation_dir/current")" == "$release_dir" ]]
  rm -rf "$sandbox"
}

run_initial_failure_policy() {
  local sandbox installation_dir release_dir fake_bin output
  sandbox=$(mktemp -d)
  sandbox=$(CDPATH='' cd -- "$sandbox" && pwd -P)
  installation_dir="$sandbox/backend"
  release_dir="$installation_dir/releases/$new_sha"
  fake_bin="$sandbox/bin"
  output="$sandbox/output.log"
  install -d "$fake_bin" "$sandbox/empty"
  prepare_release "$release_dir"
  render_bootstrap_env "$release_dir/.env.production.next"
  ln -s "$repo_root/deploy/tests/fake-docker.sh" "$fake_bin/docker"
  ln -s "$(type -P true)" "$fake_bin/flock"

  if PATH="$fake_bin:$PATH" \
    TEST_LOG="$sandbox/docker.log" \
    TEST_SCENARIO=backup-fail \
    TEST_EMPTY_DIR="$sandbox/empty" \
    TEST_OLD_SHA="$old_sha" \
    "$release_dir/deploy/deploy-production.sh" "$image" >"$output" 2>&1; then
    echo "Initial failure policy fixture unexpectedly succeeded" >&2
    rm -rf "$sandbox"
    exit 1
  fi

  grep -F 'down --remove-orphans' "$sandbox/docker.log" >/dev/null
  if grep -F 'missing-env:' "$sandbox/docker.log" >/dev/null; then
    echo "Initial rollback deleted its env files before compose cleanup" >&2
    rm -rf "$sandbox"
    exit 1
  fi
  assert_file_absent "$release_dir/.env.production.next"
  assert_file_absent "$release_dir/.release.next"
  if [[ -e "$installation_dir/current" ]]; then
    echo "Initial failed release was incorrectly accepted" >&2
    rm -rf "$sandbox"
    exit 1
  fi
  rm -rf "$sandbox"
}

run_initial_post_migration_roll_forward_policy() {
  local sandbox installation_dir release_dir fake_bin output
  sandbox=$(mktemp -d)
  sandbox=$(CDPATH='' cd -- "$sandbox" && pwd -P)
  installation_dir="$sandbox/backend"
  release_dir="$installation_dir/releases/$new_sha"
  fake_bin="$sandbox/bin"
  output="$sandbox/output.log"
  install -d "$fake_bin" "$sandbox/empty"
  prepare_release "$release_dir"
  render_bootstrap_env "$release_dir/.env.production.next"
  ln -s "$repo_root/deploy/tests/fake-docker.sh" "$fake_bin/docker"
  ln -s "$(type -P true)" "$fake_bin/flock"

  if PATH="$fake_bin:$PATH" \
    TEST_LOG="$sandbox/docker.log" \
    TEST_SCENARIO=initial-health-fail \
    TEST_EMPTY_DIR="$sandbox/empty" \
    TEST_OLD_SHA="$old_sha" \
    "$release_dir/deploy/deploy-production.sh" "$image" >"$output" 2>&1; then
    echo "Initial post-migration fixture unexpectedly succeeded" >&2
    rm -rf "$sandbox"
    exit 1
  fi

  grep -F "$release_dir/deploy/compose.production.yaml run --rm --no-deps migrate" "$sandbox/docker.log" >/dev/null
  grep -F "$release_dir/deploy/compose.production.yaml stop backend grafana prometheus alertmanager postgres-exporter pgbouncer-exporter node-exporter" "$sandbox/docker.log" >/dev/null
  grep -F "preserving the staged bundle for roll-forward recovery" "$output" >/dev/null
  assert_file_present "$release_dir/.env.production.next"
  assert_file_present "$release_dir/.release.next"
  if [[ -e "$installation_dir/current" ]]; then
    echo "Initial failed release was incorrectly accepted" >&2
    rm -rf "$sandbox"
    exit 1
  fi
  rm -rf "$sandbox"
}

run_previous_release_recovery_policy() {
  local sandbox installation_dir release_dir current_dir fake_bin output stop_line restore_line
  sandbox=$(mktemp -d)
  sandbox=$(CDPATH='' cd -- "$sandbox" && pwd -P)
  installation_dir="$sandbox/backend"
  release_dir="$installation_dir/releases/$new_sha"
  current_dir="$installation_dir/releases/$old_sha"
  fake_bin="$sandbox/bin"
  output="$sandbox/output.log"
  install -d "$fake_bin" "$sandbox/empty"
  prepare_release "$release_dir"
  prepare_release "$current_dir"
  render_bootstrap_env "$release_dir/.env.production.next"
  render_bootstrap_env "$current_dir/.env.production"
  printf 'BACKEND_IMAGE=%s\n' "$image" >"$current_dir/.release"
  ln -s "$current_dir" "$installation_dir/current"
  ln -s "$repo_root/deploy/tests/fake-docker.sh" "$fake_bin/docker"
  ln -s "$(type -P true)" "$fake_bin/flock"

  if PATH="$fake_bin:$PATH" \
    TEST_LOG="$sandbox/docker.log" \
    TEST_SCENARIO=backup-fail \
    TEST_EMPTY_DIR="$sandbox/empty" \
    TEST_OLD_SHA="$old_sha" \
    "$release_dir/deploy/deploy-production.sh" "$image" >"$output" 2>&1; then
    echo "Pre-migration failure policy fixture unexpectedly succeeded" >&2
    rm -rf "$sandbox"
    exit 1
  fi

  stop_line=$(grep -nF "$current_dir/deploy/compose.production.yaml stop backend" "$sandbox/docker.log" | tail -1 | cut -d: -f1)
  restore_line=$(grep -nF "$current_dir/deploy/compose.production.yaml up -d --no-deps --force-recreate backend" "$sandbox/docker.log" | tail -1 | cut -d: -f1)
  if [[ -z "$stop_line" || -z "$restore_line" ]] || ((restore_line <= stop_line)); then
    echo "Rollback policy did not restart the accepted backend after freezing writes" >&2
    rm -rf "$sandbox"
    exit 1
  fi
  if grep -F 'missing-env:' "$sandbox/docker.log" >/dev/null; then
    echo "Previous-release rollback lost required env metadata" >&2
    rm -rf "$sandbox"
    exit 1
  fi
  assert_file_absent "$release_dir/.env.production.next"
  assert_file_absent "$release_dir/.release.next"
  [[ -f "$current_dir/.env.production" && -f "$current_dir/.release" ]]
  [[ "$(readlink -f "$installation_dir/current")" == "$current_dir" ]]
  if find "$installation_dir/backups" -type f -name '*.tmp' -print -quit | grep -q .; then
    echo "Rollback policy left a partial backup behind" >&2
    rm -rf "$sandbox"
    exit 1
  fi
  rm -rf "$sandbox"
}

run_post_migration_roll_forward_policy() {
  local scenario=$1
  local sandbox installation_dir release_dir current_dir fake_bin output
  sandbox=$(mktemp -d)
  sandbox=$(CDPATH='' cd -- "$sandbox" && pwd -P)
  installation_dir="$sandbox/backend"
  release_dir="$installation_dir/releases/$new_sha"
  current_dir="$installation_dir/releases/$old_sha"
  fake_bin="$sandbox/bin"
  output="$sandbox/output.log"
  install -d "$fake_bin" "$sandbox/empty"
  prepare_release "$release_dir"
  prepare_release "$current_dir"
  render_bootstrap_env "$release_dir/.env.production.next"
  render_bootstrap_env "$current_dir/.env.production"
  printf 'BACKEND_IMAGE=%s\n' "$image" >"$current_dir/.release"
  ln -s "$current_dir" "$installation_dir/current"
  ln -s "$repo_root/deploy/tests/fake-docker.sh" "$fake_bin/docker"
  ln -s "$(type -P true)" "$fake_bin/flock"

  if PATH="$fake_bin:$PATH" \
    TEST_LOG="$sandbox/docker.log" \
    TEST_SCENARIO="$scenario" \
    TEST_EMPTY_DIR="$sandbox/empty" \
    TEST_OLD_SHA="$old_sha" \
    "$release_dir/deploy/deploy-production.sh" "$image" >"$output" 2>&1; then
    echo "Post-migration roll-forward fixture unexpectedly succeeded: $scenario" >&2
    rm -rf "$sandbox"
    exit 1
  fi

  grep -F "$release_dir/deploy/compose.production.yaml run --rm --no-deps migrate" "$sandbox/docker.log" >/dev/null
  grep -F "$release_dir/deploy/compose.production.yaml up -d --no-deps --force-recreate backend" "$sandbox/docker.log" >/dev/null
  if grep -F "$current_dir/deploy/compose.production.yaml up -d --no-deps --force-recreate backend" "$sandbox/docker.log" >/dev/null; then
    echo "Post-migration failure unsafely restarted the retired backend: $scenario" >&2
    rm -rf "$sandbox"
    exit 1
  fi
  if [[ "$scenario" == "monitoring-fail" ]]; then
    grep -F "$release_dir/deploy/compose.production.yaml stop grafana prometheus alertmanager postgres-exporter pgbouncer-exporter node-exporter" "$sandbox/docker.log" >/dev/null
    grep -F "$current_dir/deploy/compose.production.yaml up -d --no-deps --force-recreate grafana" "$sandbox/docker.log" >/dev/null
    grep -F "The healthy new backend remains active; previous monitoring was restored" "$output" >/dev/null
    assert_file_absent "$release_dir/.env.production.next"
    assert_file_absent "$release_dir/.release.next"
    assert_file_present "$release_dir/.env.production"
    assert_file_present "$release_dir/.release"
    [[ "$(readlink -f "$installation_dir/current")" == "$release_dir" ]]
  else
    grep -F "$release_dir/deploy/compose.production.yaml stop backend" "$sandbox/docker.log" >/dev/null
    grep -F "Backend is fail-closed; complete a roll-forward deployment" "$output" >/dev/null
    assert_file_present "$release_dir/.env.production.next"
    assert_file_present "$release_dir/.release.next"
    [[ "$(readlink -f "$installation_dir/current")" == "$current_dir" ]]
  fi
  if grep -F 'missing-env:' "$sandbox/docker.log" >/dev/null; then
    echo "Post-migration roll-forward lost required env metadata: $scenario" >&2
    rm -rf "$sandbox"
    exit 1
  fi
  rm -rf "$sandbox"
}

run_bootstrap_skips_installed_offsite_hook
run_initial_failure_policy
run_initial_post_migration_roll_forward_policy
run_previous_release_recovery_policy
run_post_migration_roll_forward_policy new-backend-health-fail
run_post_migration_roll_forward_policy monitoring-fail
echo "Deployment rollback policy tests passed."
