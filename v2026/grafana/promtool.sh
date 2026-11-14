#!/usr/bin/env bash

# Print a pinned promtool path for the dashboard tests. test.sh calls this
# before its test commands; direct callers can assign/export its output as
# URNETWORK_PROMTOOL before `go test ./grafana`. An explicit path is checked
# and returned without touching the cache or downloading anything.
#
# Cache: URNETWORK_TEST_TOOL_CACHE, otherwise XDG_CACHE_HOME/TMPDIR under
# urnetwork-test-tools-UID. Offline builders pre-provision URNETWORK_PROMTOOL.
# Cold setup needs curl, tar, head and sha256sum or shasum. It downloads one
# archive (200 MiB maximum, 180 seconds, no retries), extracts only the named
# executable (256 MiB maximum), and never overwrites an existing cache entry.
# Pins: https://github.com/prometheus/prometheus/releases/download/v3.5.0/sha256sums.txt
set -euo pipefail

promtool_version=3.5.0

fail() {
    printf 'promtool setup: %s\n' "$*" >&2
    exit 1
}

# Resolve a caller's relative path before test.sh changes package directory.
checked_tool() {
    local candidate="$1"
    local version_output
    if [[ "$candidate" != */* ]]; then
        candidate="$(command -v "$candidate")" || return 1
    fi
    [[ -f "$candidate" && -x "$candidate" ]] || return 1
    version_output="$("$candidate" --version 2>&1)" || return 1
    [[ "$version_output" == "promtool, version $promtool_version "* ]] || return 1
    printf '%s/%s\n' "$(cd -- "${candidate%/*}" && pwd)" "${candidate##*/}"
}

if [[ -n "${URNETWORK_PROMTOOL:-}" ]]; then
    checked_tool "$URNETWORK_PROMTOOL" || fail "URNETWORK_PROMTOOL must name an executable promtool $promtool_version"
    exit 0
fi
umask 077

case "$(uname -s)/$(uname -m)" in
    Darwin/arm64) platform=darwin-arm64; archive_sha=c17bc2c992f0b5515eebe2b4ccd626214f8e309a0fe6ec40ed13c42ec13d05fb ;;
    Darwin/x86_64) platform=darwin-amd64; archive_sha=5856d0c306e7d852d9a79788b20acb630501b3fab120b69f5317d6b0732dea57 ;;
    Linux/aarch64|Linux/arm64) platform=linux-arm64; archive_sha=173389cc42bf09c4e6e54cb53fa07a5a835d7c261e14775d2183181d6e385d1c ;;
    Linux/x86_64) platform=linux-amd64; archive_sha=e811827af26d822afb09a4f28314f61b618b12cff5369835a67f674d8b46f39a ;;
    *) fail "unsupported platform; provision official promtool $promtool_version and set URNETWORK_PROMTOOL" ;;
esac

if command -v sha256sum >/dev/null 2>&1; then
    hash_command=(sha256sum)
elif command -v shasum >/dev/null 2>&1; then
    hash_command=(shasum -a 256)
else
    fail 'sha256sum or shasum is required'
fi
file_sha() {
    local checksum
    checksum="$("${hash_command[@]}" "$1")" || return 1
    printf '%s\n' "${checksum%% *}"
}

# A shared temporary parent must not turn a cached executable into an implicit
# trust override. Check both directory boundaries and existing entry inodes.
checked_cache_path() {
    local path="$1"
    local metadata owner mode
    [[ ! -L "$path" ]] || fail "cache path is a symlink: $path"
    metadata="$(stat -c '%u %a' -- "$path" 2>/dev/null || stat -f '%u %Lp' -- "$path")" || fail "cannot inspect cache path: $path"
    read -r owner mode <<< "$metadata"
    [[ "$owner" == "$EUID" && "$mode" =~ ^[0-7]{3,4}$ ]] || fail "cache path is not owned by this user: $path"
    (( (8#$mode & 0022) == 0 )) || fail "cache path is writable by another user: $path"
}
make_cache_directory() {
    local path="$1"
    if [[ ! -e "$path" && ! -L "$path" ]]; then
        mkdir -p -- "$path"
    fi
    [[ -d "$path" ]] || fail "cache path is not a directory: $path"
    checked_cache_path "$path"
}

cache_root="${URNETWORK_TEST_TOOL_CACHE:-${XDG_CACHE_HOME:-${TMPDIR:-/tmp}}/urnetwork-test-tools-$EUID}"
make_cache_directory "$cache_root"
cache_dir="$cache_root/prometheus-$promtool_version-$platform"
make_cache_directory "$cache_dir"
cache_dir="$(cd -- "$cache_dir" && pwd)"
cached_tool="$cache_dir/promtool"
cached_sha="$cache_dir/promtool.sha256"
[[ ! -L "$cached_tool" && ! -L "$cached_sha" ]] || fail "cache path contains a symlinked executable or checksum: $cache_dir"
if [[ -e "$cached_tool" || -e "$cached_sha" ]]; then
    [[ -f "$cached_tool" && -f "$cached_sha" ]] || fail "incomplete cache entry at $cache_dir; refusing to overwrite it"
    checked_cache_path "$cached_tool"
    checked_cache_path "$cached_sha"
    read -r recorded_sha < "$cached_sha"
    [[ "$recorded_sha" =~ ^[0-9a-f]{64}$ && "$(file_sha "$cached_tool")" == "$recorded_sha" ]] || fail "cache checksum mismatch at $cache_dir; refusing to overwrite it"
    checked_tool "$cached_tool" || fail "cache version mismatch at $cache_dir; refusing to overwrite it"
    exit 0
fi

temporary_dir="$(mktemp -d "$cache_dir/.download.XXXXXX")"
cleanup() {
    rm -f -- "$temporary_dir/archive.tar.gz" "$temporary_dir/promtool"
    rmdir -- "$temporary_dir" 2>/dev/null || true
}
trap cleanup EXIT
archive_name="prometheus-$promtool_version.$platform.tar.gz"
member="prometheus-$promtool_version.$platform/promtool"
printf 'promtool setup: downloading official %s\n' "$archive_name" >&2
curl --fail --location --silent --show-error --connect-timeout 15 --max-time 180 \
    --max-filesize 209715200 --output "$temporary_dir/archive.tar.gz" \
    "https://github.com/prometheus/prometheus/releases/download/v$promtool_version/$archive_name"
[[ "$(file_sha "$temporary_dir/archive.tar.gz")" == "$archive_sha" ]] || fail 'archive checksum mismatch; executable was not extracted'
tar -xOf "$temporary_dir/archive.tar.gz" "$member" | head -c 268435457 > "$temporary_dir/promtool"
[[ "$(wc -c < "$temporary_dir/promtool")" -le 268435456 ]] || fail 'extracted executable exceeds 256 MiB'
chmod 755 "$temporary_dir/promtool"
checked_tool "$temporary_dir/promtool" >/dev/null || fail 'extracted executable has the wrong version'
binary_sha="$(file_sha "$temporary_dir/promtool")"

# Reserve the checksum without clobbering another installer, then publish the
# complete executable with an atomic, no-overwrite link on the same filesystem.
(set -o noclobber; printf '%s\n' "$binary_sha" > "$cached_sha") || fail 'cache entry appeared during setup; refusing to overwrite it'
ln -- "$temporary_dir/promtool" "$cached_tool" || fail 'cache executable appeared during setup; refusing to overwrite it'
printf '%s\n' "$cached_tool"
