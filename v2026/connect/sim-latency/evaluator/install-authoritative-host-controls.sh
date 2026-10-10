#!/usr/bin/env bash

# Install or verify the root-owned pre-Docker host-control unit. The control
# command and its resource-boundary dependency are installed together so a
# boot cannot silently skip topology validation.

set -Eeuo pipefail
umask 077

readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
readonly CONTROL_SOURCE="$SCRIPT_DIR/authoritative-host-controls.sh"
readonly CONTROL_LIBRARY_SOURCE="$SCRIPT_DIR/authoritative-host-controls-lib.sh"
readonly BOUNDARY_SOURCE="$SCRIPT_DIR/container/resource-boundary.sh"
readonly UNIT_SOURCE="$SCRIPT_DIR/authoritative-host-controls.service.example"
readonly IRQ_SOURCE="$SCRIPT_DIR/authoritative-host-irqs.sh"
readonly IRQ_UNIT_SOURCE="$SCRIPT_DIR/authoritative-host-irqs.service.example"
readonly CONTROL_TARGET=/usr/local/libexec/urnetwork/authoritative-host-controls
readonly CONTROL_LIBRARY_TARGET=/usr/local/libexec/urnetwork/authoritative-host-controls-lib.sh
readonly BOUNDARY_TARGET=/usr/local/libexec/urnetwork/container/resource-boundary.sh
readonly UNIT_TARGET=/etc/systemd/system/urnetwork-authoritative-host-controls.service
readonly IRQ_TARGET=/usr/local/libexec/urnetwork/authoritative-host-irqs
readonly IRQ_UNIT_TARGET=/etc/systemd/system/urnetwork-authoritative-host-irqs.service
readonly UNIT_NAME=urnetwork-authoritative-host-controls.service
readonly IRQ_UNIT_NAME=urnetwork-authoritative-host-irqs.service

mode="${1:---check}"
[ "$#" -eq 1 ] && { [ "$mode" = --check ] || [ "$mode" = --install ]; } || {
    printf 'usage: install-authoritative-host-controls --check|--install\n' >&2
    exit 2
}

die() {
    printf '[install-host-controls] ERROR: %s\n' "$*" >&2
    exit 1
}
sha256_file() { sha256sum "$1" | awk '{print $1}'; }
for command in awk jq sha256sum stat sudo systemctl; do
    command -v "$command" >/dev/null 2>&1 || die "required command missing: $command"
done
for source in "$CONTROL_SOURCE" "$CONTROL_LIBRARY_SOURCE" "$BOUNDARY_SOURCE" "$UNIT_SOURCE" "$IRQ_SOURCE" "$IRQ_UNIT_SOURCE"; do
    [ -f "$source" ] && [ ! -L "$source" ] || die "install source is unsafe: $source"
done

if [ "$mode" = --install ]; then
    sudo -n install -D -o root -g root -m 0555 "$CONTROL_SOURCE" "$CONTROL_TARGET"
    sudo -n install -D -o root -g root -m 0444 "$CONTROL_LIBRARY_SOURCE" "$CONTROL_LIBRARY_TARGET"
    sudo -n install -D -o root -g root -m 0555 "$BOUNDARY_SOURCE" "$BOUNDARY_TARGET"
    sudo -n install -D -o root -g root -m 0555 "$IRQ_SOURCE" "$IRQ_TARGET"
    sudo -n install -D -o root -g root -m 0444 "$UNIT_SOURCE" "$UNIT_TARGET"
    sudo -n install -D -o root -g root -m 0444 "$IRQ_UNIT_SOURCE" "$IRQ_UNIT_TARGET"
    sudo -n systemctl daemon-reload
    sudo -n systemctl reenable "$UNIT_NAME" "$IRQ_UNIT_NAME" >/dev/null
    if systemctl is-active --quiet "$UNIT_NAME"; then
        sudo -n systemctl restart "$UNIT_NAME"
    else
        sudo -n systemctl start "$UNIT_NAME"
    fi
    if systemctl is-active --quiet "$IRQ_UNIT_NAME"; then
        sudo -n systemctl restart "$IRQ_UNIT_NAME"
    else
        sudo -n systemctl start "$IRQ_UNIT_NAME"
    fi
fi

for target in "$CONTROL_TARGET" "$CONTROL_LIBRARY_TARGET" "$BOUNDARY_TARGET" "$UNIT_TARGET" "$IRQ_TARGET" "$IRQ_UNIT_TARGET"; do
    [ -f "$target" ] && [ ! -L "$target" ] || die "installed file is missing or unsafe: $target"
    [ "$(stat -c %u "$target")" = 0 ] && [ "$(stat -c %g "$target")" = 0 ] ||
        die "installed file is not root-owned: $target"
done
[ "$(stat -c %a "$CONTROL_TARGET")" = 555 ] || die 'control executable mode is not 0555'
[ "$(stat -c %a "$CONTROL_LIBRARY_TARGET")" = 444 ] || die 'control library mode is not 0444'
[ "$(stat -c %a "$BOUNDARY_TARGET")" = 555 ] || die 'boundary executable mode is not 0555'
[ "$(stat -c %a "$IRQ_TARGET")" = 555 ] || die 'IRQ executable mode is not 0555'
[ "$(stat -c %a "$UNIT_TARGET")" = 444 ] || die 'unit mode is not 0444'
[ "$(stat -c %a "$IRQ_UNIT_TARGET")" = 444 ] || die 'IRQ unit mode is not 0444'
[ "$(sha256_file "$CONTROL_SOURCE")" = "$(sha256_file "$CONTROL_TARGET")" ] ||
    die 'installed control executable differs from the release source'
[ "$(sha256_file "$CONTROL_LIBRARY_SOURCE")" = "$(sha256_file "$CONTROL_LIBRARY_TARGET")" ] ||
    die 'installed control library differs from the release source'
[ "$(sha256_file "$BOUNDARY_SOURCE")" = "$(sha256_file "$BOUNDARY_TARGET")" ] ||
    die 'installed resource boundary differs from the release source'
[ "$(sha256_file "$UNIT_SOURCE")" = "$(sha256_file "$UNIT_TARGET")" ] ||
    die 'installed unit differs from the release source'
[ "$(sha256_file "$IRQ_SOURCE")" = "$(sha256_file "$IRQ_TARGET")" ] ||
    die 'installed IRQ executable differs from the release source'
[ "$(sha256_file "$IRQ_UNIT_SOURCE")" = "$(sha256_file "$IRQ_UNIT_TARGET")" ] ||
    die 'installed IRQ unit differs from the release source'
[ "$(systemctl is-enabled "$UNIT_NAME")" = enabled ] || die 'host-control unit is not enabled'
[ "$(systemctl is-active "$UNIT_NAME")" = active ] || die 'host-control unit is not active'
[ "$(systemctl is-enabled "$IRQ_UNIT_NAME")" = enabled ] || die 'IRQ unit is not enabled'
[ "$(systemctl is-active "$IRQ_UNIT_NAME")" = active ] || die 'IRQ unit is not active'
[ "$(systemctl show "$UNIT_NAME" -p ExecMainStatus --value)" = 0 ] ||
    die 'host-control unit did not exit successfully'
[ "$(systemctl show "$IRQ_UNIT_NAME" -p ExecMainStatus --value)" = 0 ] ||
    die 'IRQ unit did not exit successfully'
sudo -n "$CONTROL_TARGET" --check >/dev/null || die 'installed live host controls do not pass'
sudo -n "$IRQ_TARGET" --check >/dev/null || die 'installed IRQ placement does not pass'

jq -n \
    --arg mode "${mode#--}" \
    --arg unit "$UNIT_NAME" \
    --arg irq_unit "$IRQ_UNIT_NAME" \
    --arg control_sha256 "$(sha256_file "$CONTROL_TARGET")" \
    --arg control_library_sha256 "$(sha256_file "$CONTROL_LIBRARY_TARGET")" \
    --arg boundary_sha256 "$(sha256_file "$BOUNDARY_TARGET")" \
    --arg unit_sha256 "$(sha256_file "$UNIT_TARGET")" \
    --arg irq_sha256 "$(sha256_file "$IRQ_TARGET")" \
    --arg irq_unit_sha256 "$(sha256_file "$IRQ_UNIT_TARGET")" \
    '{schema:1,kind:"sim-latency-authoritative-host-controls-install",
      mode:$mode,unit:$unit,irq_unit:$irq_unit,enabled:true,active:true,
      live_check_passed:true,irq_check_passed:true,
      control_sha256:$control_sha256,control_library_sha256:$control_library_sha256,
      boundary_sha256:$boundary_sha256,
      unit_sha256:$unit_sha256,irq_sha256:$irq_sha256,
      irq_unit_sha256:$irq_unit_sha256}'
