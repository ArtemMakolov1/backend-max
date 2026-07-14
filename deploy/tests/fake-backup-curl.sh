#!/usr/bin/env bash
set -euo pipefail

state=${TEST_CURL_STATE:?TEST_CURL_STATE is required}
method=GET
output=''
data=''
url=''

while (($#)); do
  case "$1" in
    --config|-H|--max-time)
      shift 2
      ;;
    -X)
      method=$2
      shift 2
      ;;
    --data-binary)
      data=${2#@}
      shift 2
      ;;
    -o)
      output=$2
      shift 2
      ;;
    --silent|--show-error|--fail-with-body|--location)
      shift
      ;;
    http://*|https://*)
      url=$1
      shift
      ;;
    *)
      echo "Unexpected fake curl argument: $1" >&2
      exit 2
      ;;
  esac
done

respond() {
  if [[ -n "$output" ]]; then
    printf '%s\n' "$1" >"$output"
  else
    printf '%s\n' "$1"
  fi
}

mkdir -p "$state"
if [[ "$method" == POST && "$url" == */releases ]]; then
  tag=$(jq -er '.tag_name' "$data")
  printf '%s' "$tag" >"$state/tag"
  respond "$(jq -nc --arg tag "$tag" '{id:42,draft:true,tag_name:$tag}')"
elif [[ "$method" == POST && "$url" == */releases/42/assets\?name=* ]]; then
  name=${url##*name=}
  cp "$data" "$state/asset"
  sha=$(sha256sum "$data" | awk '{print $1}')
  size=$(stat -c '%s' "$data")
  printf '%s' "$name" >"$state/name"
  printf '%s' "$sha" >"$state/sha"
  if [[ ${TEST_BAD_DIGEST:-false} == true ]]; then
    sha=$(printf '0%.0s' {1..64})
  fi
  respond "$(jq -nc --arg name "$name" --arg digest "sha256:$sha" --argjson size "$size" \
    '{id:77,name:$name,state:"uploaded",size:$size,digest:$digest}')"
elif [[ "$method" == GET && "$url" == */releases/assets/77 ]]; then
  cp "$state/asset" "$output"
elif [[ "$method" == PATCH && "$url" == */releases/42 ]]; then
  respond '{"id":42,"draft":false}'
elif [[ "$method" == GET && "$url" == */releases/42 ]]; then
  tag=$(cat "$state/tag")
  name=$(cat "$state/name")
  sha=$(cat "$state/sha")
  respond "$(jq -nc --arg tag "$tag" --arg name "$name" --arg digest "sha256:$sha" \
    '{id:42,draft:false,immutable:true,tag_name:$tag,assets:[{name:$name,state:"uploaded",digest:$digest}]}')"
elif [[ "$method" == DELETE && "$url" == */releases/42 ]]; then
  : >"$state/deleted"
  respond ''
else
  echo "Unexpected fake curl request: $method $url" >&2
  exit 2
fi
