#!/usr/bin/env bash
#
# run-local-dns.sh -- run the local stack's test dns edge (DNS-M5-18, DESIGN.md
# §19.11's local row): the dns API block and one dns edge, on the launcher's
# loopback alias, with a key set of their own (local/testdns), so the edge
# answers signed before DNS-M5-19's ceremony releases real DNSKEY windows.
#
# Start ./run-local.sh first, wait for 'Local environment is up', and migrate the
# local database (bringyourctl/run-local.sh db migrate): the API block reads
# MG2's and MG5's tables. Then run this in its own terminal and query the edge:
#
#   dig @10.213.0.1 -p 10053 +dnssec SOA <root>
#
# for each root in config's local dns.yml. The dns gate in config's
# local/sidecar.yml is closed as checked in, and a closed gate answers 503 to
# every route the edge follows, so the edge stays not ready until you open it in
# your own config checkout: dns: {enabled: true, writes: true}.
#
# Both processes bind only the address the launcher attested, never 127.0.0.1,
# and refuse the production data tunnel's ports (5432, 6432, 6379-6427). Ports:
#
#   LOCAL_DNS_API_PORT          the API block's routes and /status (10080)
#   LOCAL_DNS_EDGE_STATUS_PORT  the edge's /status (10081)
#   LOCAL_DNS_PORT              the edge's DNS, UDP and TCP (10053)
#   LOCAL_DNS_STATE_DIR         the key set, bearer and snapshots, absolute and
#                               owner-only (${TMPDIR}/urnetwork-local-dns-<uid>)
#
# Ctrl-C stops both. The state stays for the next run; remove the directory to
# start over with a new key set.

set -u

server_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." >/dev/null 2>&1 && pwd)" || exit 1

die() {
    printf 'run-local-dns: %s\n' "$*" >&2
    exit 1
}

# A port this may bind: a number, and never one a production data tunnel uses.
check_port() {
    local name="$1" port="$2"
    [[ "$port" =~ ^[0-9]+$ ]] && (( 1 <= port && port <= 65535 )) || die "$name '$port' is not a port"
    if (( port == 5432 || port == 6432 || (6379 <= port && port <= 6427) )); then
        die "$name $port is a production data tunnel's port"
    fi
}

api_port="${LOCAL_DNS_API_PORT:-10080}"
edge_status_port="${LOCAL_DNS_EDGE_STATUS_PORT:-10081}"
dns_port="${LOCAL_DNS_PORT:-10053}"
check_port LOCAL_DNS_API_PORT "$api_port"
check_port LOCAL_DNS_EDGE_STATUS_PORT "$edge_status_port"
check_port LOCAL_DNS_PORT "$dns_port"
if [[ "$api_port" == "$edge_status_port" || "$api_port" == "$dns_port" || "$edge_status_port" == "$dns_port" ]]; then
    die "the API, edge status and DNS ports must differ"
fi

tmp_dir="${TMPDIR:-/tmp}"
state="${LOCAL_DNS_STATE_DIR:-${tmp_dir%/}/urnetwork-local-dns-$(id -u)}"
[[ "$state" == /* ]] || die "LOCAL_DNS_STATE_DIR '$state' is not absolute"

export WARP_HOST="127.0.0.1"
export WARP_BLOCK="local"
export WARP_SERVICE="dns"
export WARP_VERSION="0.0.0-local"
export WARP_ENV="local"
export BRINGYOUR_POSTGRES_HOSTNAME="local-pg.bringyour.com"
export BRINGYOUR_REDIS_HOSTNAME="local-redis.bringyour.com"
source "$server_dir/local/local-env.sh" || exit 1

# The launcher's loopback alias, as its attestation names it: the host of its
# first attested endpoint, which the preflight already refused as loopback.
alias_ip="${WARP_TEST_ENV_ATTESTED_ADDRS%%,*}"
alias_ip="${alias_ip%:*}"
[[ "$alias_ip" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "the launcher attested '$alias_ip', which is no IPv4 loopback alias"
case "$alias_ip" in
    127.*|0.0.0.0)
        die "the launcher attested '$alias_ip', which is no dedicated loopback alias" ;;
esac

cd "$server_dir" || exit 1
mkdir -p -m 700 "$state" || die "cannot make $state"
bin_dir="$state/bin"
mkdir -p -m 700 "$bin_dir" || die "cannot make $bin_dir"
go build -o "$bin_dir/testdns" ./local/testdns || die "local/testdns does not build"
"$bin_dir/testdns" keys -state "$state" || die "the key set in $state cannot be used"

api_pid=""
edge_pid=""
stop() {
    trap - INT TERM EXIT
    [[ -n "$edge_pid" ]] && kill "$edge_pid" 2>/dev/null
    [[ -n "$api_pid" ]] && kill "$api_pid" 2>/dev/null
    wait 2>/dev/null
}
trap 'stop; exit 130' INT
trap 'stop; exit 143' TERM
trap stop EXIT

WARP_SERVICE="dns" WARP_HOST_IPV4="$alias_ip" WARP_PORTS="80:$api_port" \
    "$bin_dir/testdns" api -state "$state" &
api_pid=$!
WARP_SERVICE="dnsedge-1" WARP_HOST_IPV4="$alias_ip" WARP_PORTS="80:$edge_status_port,53:$dns_port" \
    "$bin_dir/testdns" edge -state "$state" -api_url "http://$alias_ip:$api_port" &
edge_pid=$!

printf 'run-local-dns: the API serves on %s:%s and the edge answers on %s:%s (UDP and TCP)\n' \
    "$alias_ip" "$api_port" "$alias_ip" "$dns_port"
printf 'run-local-dns: Ctrl-C stops both; %s keeps the key set\n' "$state"

# until either exits, then both stop
while kill -0 "$api_pid" 2>/dev/null && kill -0 "$edge_pid" 2>/dev/null; do
    sleep 1
done
die "a role exited; stopping both"
