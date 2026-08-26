#!/usr/bin/env bash

# Pin every movable device IRQ away from the ten evaluation cores and onto the
# two management cores. Legacy IRQs below 16 are excluded because several are
# architecture-fixed; all PCI/MSI/MSI-X and ordinary device IRQs are covered.

set -Eeuo pipefail
umask 077

readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
readonly RESOURCE_BOUNDARY="$SCRIPT_DIR/container/resource-boundary.sh"
readonly MIN_DEVICE_IRQ=16

mode="${1:---check}"
[ "$#" -eq 1 ] && { [ "$mode" = --check ] || [ "$mode" = --apply ]; } || {
    printf 'usage: authoritative-host-irqs --check|--apply\n' >&2
    exit 2
}

die() {
    printf '[competition-host-irqs] ERROR: %s\n' "$*" >&2
    exit 1
}
for command in awk jq sed sha256sum sort sudo tr; do
    command -v "$command" >/dev/null 2>&1 || die "required command missing: $command"
done
[ -x "$RESOURCE_BOUNDARY" ] || die 'resource-boundary helper is unavailable'

boundary="$($RESOURCE_BOUNDARY)" || die 'resource boundary is invalid'
management_cpuset="$(jq -er '.management_cpuset' <<<"$boundary")"
evaluation_cpuset="$(jq -er '.evaluation_cpuset' <<<"$boundary")"
[[ "$management_cpuset" =~ ^[0-9,-]+$ ]] || die 'management CPU set is invalid'

mapfile -t device_irqs < <(
    awk -F: -v minimum="$MIN_DEVICE_IRQ" '
        /^[[:space:]]*[0-9]+:/ {
            irq = $1 + 0
            if (irq >= minimum) print irq
        }
    ' /proc/interrupts | sort -n -u
)
[ "${#device_irqs[@]}" -gt 0 ] || die 'no device IRQs were discovered'

failed_irqs=()
verified_irqs=()
for irq in "${device_irqs[@]}"; do
    affinity_path="/proc/irq/$irq/smp_affinity_list"
    [ -e "$affinity_path" ] || {
        failed_irqs+=("$irq:missing")
        continue
    }
    if [ "$mode" = --apply ]; then
        if ! printf '%s\n' "$management_cpuset" | sudo -n tee "$affinity_path" >/dev/null 2>&1; then
            failed_irqs+=("$irq:write")
            continue
        fi
    fi
    configured="$(tr -d '\n' <"$affinity_path" 2>/dev/null || true)"
    if [ "$configured" != "$management_cpuset" ]; then
        failed_irqs+=("$irq:$configured")
        continue
    fi
    verified_irqs+=("$irq")
done

irq_affinity_sha256="$(
    for path in /proc/irq/[0-9]*/smp_affinity_list; do
        [ -r "$path" ] || continue
        printf '%s=' "${path#/proc/irq/}"
        tr -d '\n' <"$path"
        printf '\n'
    done | LC_ALL=C sort | sha256sum | awk '{print $1}'
)"
passed=false
[ "${#failed_irqs[@]}" -eq 0 ] && [ "${#verified_irqs[@]}" -eq "${#device_irqs[@]}" ] && passed=true

failed_json="$(printf '%s\n' "${failed_irqs[@]:-}" | sed '/^$/d' | jq -R . | jq -s .)"
jq -n \
    --arg mode "${mode#--}" \
    --arg evaluation_cpuset "$evaluation_cpuset" \
    --arg management_cpuset "$management_cpuset" \
    --arg irq_affinity_sha256 "$irq_affinity_sha256" \
    --argjson discovered_irq_count "${#device_irqs[@]}" \
    --argjson verified_irq_count "${#verified_irqs[@]}" \
    --argjson failed_irqs "$failed_json" \
    --argjson passed "$passed" \
    '{schema:1,kind:"sim-latency-authoritative-host-irqs",mode:$mode,
      evaluation_cpuset:$evaluation_cpuset,management_cpuset:$management_cpuset,
      minimum_device_irq:16,discovered_irq_count:$discovered_irq_count,
      verified_irq_count:$verified_irq_count,failed_irqs:$failed_irqs,
      irq_affinity_sha256:$irq_affinity_sha256,passed:$passed}'

[ "$passed" = true ]
