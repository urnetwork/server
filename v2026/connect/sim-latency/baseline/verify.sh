#!/usr/bin/env bash

set -euo pipefail

baseline_root="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
manifest_path="$baseline_root/MANIFEST.sha256"

if [[ ! -f "$manifest_path" ]]; then
  echo "missing baseline manifest: $manifest_path" >&2
  exit 1
fi

tmp_dir="$(mktemp -d)"
cleanup() {
  rm -f -- "$tmp_dir/actual.paths" "$tmp_dir/manifest.paths"
  rmdir -- "$tmp_dir"
}
trap cleanup EXIT

(
  cd "$baseline_root"
  LC_ALL=C find . -type f ! -path './MANIFEST.sha256' -printf '%P\n' |
    LC_ALL=C sort >"$tmp_dir/actual.paths"
  sed -nE 's/^[0-9a-f]{64}  (.*)$/\1/p' MANIFEST.sha256 |
    LC_ALL=C sort >"$tmp_dir/manifest.paths"

  if [[ "$(wc -l <MANIFEST.sha256)" -ne "$(wc -l <"$tmp_dir/manifest.paths")" ]]; then
    echo "baseline manifest contains a malformed entry" >&2
    exit 1
  fi
  if ! diff -u "$tmp_dir/manifest.paths" "$tmp_dir/actual.paths"; then
    echo "baseline manifest coverage differs from the working tree" >&2
    exit 1
  fi

  sha256sum --check --strict MANIFEST.sha256
)

echo "sim-latency baseline snapshot: verified"
