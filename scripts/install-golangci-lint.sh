#!/bin/sh

set -eu

root_dir=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
version=$(tr -d '[:space:]' < "$root_dir/.golangci-lint-version")
bin_dir=${GOLANGCI_LINT_BIN_DIR:-"$root_dir/bin"}
binary="$bin_dir/golangci-lint"

if [ -z "$version" ]; then
  echo "golangci-lint version is empty" >&2
  exit 1
fi

if [ -x "$binary" ]; then
  installed_version=$($binary version 2>/dev/null || true)
  case "$installed_version" in
    *"version ${version#v} "*) exit 0 ;;
  esac
fi

if ! command -v curl >/dev/null 2>&1; then
  echo "curl is required to install golangci-lint" >&2
  exit 1
fi

mkdir -p "$bin_dir"
installer=$(mktemp "${TMPDIR:-/tmp}/golangci-lint-install.XXXXXX")
trap 'rm -f "$installer"' EXIT HUP INT TERM

curl --proto '=https' --tlsv1.2 --fail --silent --show-error --location \
  https://golangci-lint.run/install.sh \
  --output "$installer"
sh "$installer" -b "$bin_dir" "$version"

installed_version=$($binary version)
case "$installed_version" in
  *"version ${version#v} "*) ;;
  *)
    echo "unexpected golangci-lint version: $installed_version" >&2
    exit 1
    ;;
esac
