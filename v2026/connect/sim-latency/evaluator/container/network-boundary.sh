#!/usr/bin/env bash
# Verify an internal bridge cannot expose a host gateway, before untrusted code.
set -Eeuo pipefail
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)/subevaluation-ownership.sh"
subevaluation_ownership_init

check_inspect() {
    jq -e 'length == 1 and .[0].Driver == "bridge" and .[0].Scope == "local" and
      .[0].Internal == true and .[0].EnableIPv6 == false and .[0].Attachable == false and
      (.[0].Id | test("^[0-9a-f]{64}$")) and
      .[0].Options["com.docker.network.bridge.gateway_mode_ipv4"] == "isolated" and
      .[0].Options["com.docker.network.bridge.gateway_mode_ipv6"] == "isolated" and
      .[0].Options["com.docker.network.bridge.name"] == null' >/dev/null
}

check_link() {
    jq -e 'length == 1 and (.[0].ifname | test("^br-[0-9a-f]{12}$")) and
      (.[0].addr_info | type == "array" and
        all(.[]; .family == "inet6" and .scope == "link" and .prefixlen == 64 and
          (.local | type == "string" and startswith("fe80:"))))' >/dev/null
}

[[ $# == 1 ]] || { printf 'usage: network-boundary.sh NETWORK | --check-inspect | --check-link\n' >&2; exit 2; }
case "$1" in
    --check-inspect) check_inspect; exit ;;
    --check-link) check_link; exit ;;
    -*) exit 2 ;;
esac
network_json="$(evaluation_docker network inspect "$1")"
if [[ "$subevaluation_scoped" = true ]]; then
    network_id="$(jq -er 'select(length == 1) | .[0].Id | select(test("^[0-9a-f]{64}$"))' <<<"$network_json")"
    proof="$(evaluation_host verify-network "$network_id")" || {
        printf 'evaluation private namespace network proof is unavailable\n' >&2
        exit 1
    }
    jq -e --arg network_id "$network_id" --arg bridge "$URNETWORK_SUBEVALUATION_BRIDGE_NAME" '
        .schema == 1 and .network_id == $network_id and .bridge_name == $bridge and
        .private_namespace == true and .isolated_gateway == true and
        (.link_sha256 | test("^[0-9a-f]{64}$"))' <<<"$proof" >/dev/null || exit 1
    jq -n --argjson network "$network_json" --argjson proof "$proof" \
        '{schema:1,network:$network[0],private_namespace:$proof.private_namespace,
          bridge_name:$proof.bridge_name,link_sha256:$proof.link_sha256,isolated_gateway:true}'
    exit 0
fi
if ! check_inspect <<<"$network_json"; then
    jq -n --argjson network "$network_json" \
        '{schema:1, network:$network[0], isolated_gateway:false, failure:"network_inspection"}'
    printf 'evaluation network has no isolated gateway boundary\n' >&2
    exit 1
fi
bridge="$(jq -er '"br-" + .[0].Id[0:12]' <<<"$network_json")"
link_json="$(ip -j address show dev "$bridge")"
if ! check_link <<<"$link_json"; then
    jq -n --argjson network "$network_json" --argjson bridge "$link_json" \
        '{schema:1, network:$network[0], host_bridge:$bridge[0], isolated_gateway:false, failure:"host_bridge_address"}'
    printf 'evaluation host bridge still has a reachable IP address\n' >&2
    exit 1
fi
jq -e --arg bridge "$bridge" '.[0].ifname == $bridge' <<<"$link_json" >/dev/null
jq -n --argjson network "$network_json" --argjson bridge "$link_json" \
    '{schema:1, network:$network[0], host_bridge:$bridge[0], isolated_gateway:true}'
