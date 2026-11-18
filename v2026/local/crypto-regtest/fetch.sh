#!/usr/bin/env bash
# Downloads the pinned release tarballs into .cache/, refuses any whose sha256
# differs from the pin, and extracts each into its own directory under
# .cache/bin/<release>/. Idempotent: a verified tarball is not fetched again.

source "$(dirname -- "${BASH_SOURCE[0]}")/lib.sh"

# fetch_release <release> <url> <sha256> <strip components>
fetch_release() {
    local release="$1" url="$2" sha256="$3" strip="$4"
    local tarball="$CACHE_DIR/downloads/${url##*/}"
    local bin_dir="$CACHE_DIR/bin/$release"
    mkdir -p "$CACHE_DIR/downloads" "$CACHE_DIR/bin"

    if [[ -f "$tarball" && "$(sha256_of "$tarball")" != "$sha256" ]]; then
        log "$release: cached tarball does not match its pin; deleting it"
        rm -f "$tarball"
        rm -rf "$bin_dir"
    fi
    if [[ ! -f "$tarball" ]]; then
        log "$release: downloading ${url##*/}"
        local partial="$tarball.partial"
        rm -f "$partial"
        curl --fail --location --silent --show-error --retry 3 --retry-delay 5 \
            --connect-timeout 30 --max-time 1800 --output "$partial" "$url"
        local actual
        actual="$(sha256_of "$partial")"
        if [[ "$actual" != "$sha256" ]]; then
            rm -f "$partial"
            die "$release: sha256 $actual does not match the pin $sha256; refused and deleted"
        fi
        mv "$partial" "$tarball"
    fi
    # The extracted tree is re-made from the verified tarball whenever its
    # marker is missing or names another hash.
    if [[ ! -f "$bin_dir/.verified-sha256" || "$(cat "$bin_dir/.verified-sha256")" != "$sha256" ]]; then
        rm -rf "$bin_dir"
        mkdir -p "$bin_dir"
        tar -x -f "$tarball" -C "$bin_dir" --strip-components "$strip"
        printf '%s' "$sha256" >"$bin_dir/.verified-sha256"
        log "$release: verified and extracted"
    fi
}

fetch_release "$MONERO_RELEASE" "$MONERO_URL" "$MONERO_SHA256" "$MONERO_STRIP"
fetch_release "$ZEBRAD_RELEASE" "$ZEBRAD_URL" "$ZEBRAD_SHA256" "$ZEBRAD_STRIP"
fetch_release "$ZECD_RELEASE" "$ZECD_URL" "$ZECD_SHA256" "$ZECD_STRIP"

for binary in "$CACHE_DIR/bin/$MONERO_RELEASE/monerod" \
    "$CACHE_DIR/bin/$MONERO_RELEASE/monero-wallet-rpc" \
    "$CACHE_DIR/bin/$ZEBRAD_RELEASE/zebrad" \
    "$CACHE_DIR/bin/$ZECD_RELEASE/zecd"; do
    [[ -x "$binary" ]] || die "missing after extraction: ${binary#"$CACHE_DIR"/}"
done
log "release binaries ready in .cache/bin"
