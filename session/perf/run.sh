#!/usr/bin/env bash

# Runs the local session performance harness (README.md):
#
#   1. checks the local test environment with test-env.sh, and that its
#      PostgreSQL is the launcher's dedicated address (10.213.0.1);
#   2. optionally takes a DB test lock directory, waiting for its holder;
#   3. starts a disposable Redis Cluster in one container, its client ports
#      published on that same dedicated address, never on loopback, and checks
#      that it is empty;
#   4. runs TestSessionPerf against it and the test database;
#   5. removes the container and releases the lock, whatever happened.
#
# Settings (environment):
#   SESSION_PERF_PROFILE           quick or standard (default standard)
#   SESSION_PERF_OUT               output directory (default temp/session-perf/<utc time>)
#   SESSION_PERF_LABEL             report label (default branch@commit)
#   SESSION_PERF_PHASES            comma list: unit,capacity,steady,storm,signout,outbox
#   SESSION_PERF_STEADY_SECONDS    steady phase length
#   SESSION_PERF_RATES             overrides, e.g. api=4000,lease=3000
#   SESSION_PERF_LOCK_DIR          a mkdir lock to hold while the DB tests run
#   SESSION_PERF_REDIS_NODES       masters, 3 to 6 (default 3)
#   SESSION_PERF_REDIS_PORT_BASE   first client port (default 26379)
#   SESSION_PERF_EXPECT_DB_HOST    the launcher address to insist on (default 10.213.0.1)

set -euo pipefail

perf_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd)"
server_dir="$(cd -- "$perf_dir/../.." >/dev/null 2>&1 && pwd)"

perf_error() {
    printf 'session perf: %s\n' "$*" >&2
}

profile="${SESSION_PERF_PROFILE:-standard}"
nodes="${SESSION_PERF_REDIS_NODES:-3}"
port_base="${SESSION_PERF_REDIS_PORT_BASE:-26379}"
image="${SESSION_PERF_REDIS_IMAGE:-redis:8-alpine}"
container="${SESSION_PERF_REDIS_CONTAINER:-urnetwork-session-perf-redis}"
expect_db_host="${SESSION_PERF_EXPECT_DB_HOST:-10.213.0.1}"
lock_dir="${SESSION_PERF_LOCK_DIR:-}"
lock_wait_seconds="${SESSION_PERF_LOCK_WAIT_SECONDS:-3600}"
test_timeout="${SESSION_PERF_TIMEOUT:-90m}"
maxmemory="${SESSION_PERF_REDIS_MAXMEMORY:-4gb}"
label="${SESSION_PERF_LABEL:-$(git -C "$server_dir" rev-parse --abbrev-ref HEAD)@$(git -C "$server_dir" rev-parse --short=12 HEAD)}"
out_dir="${SESSION_PERF_OUT:-$server_dir/temp/session-perf/$(date -u +%Y%m%dT%H%M%SZ)}"

case "$profile" in
    quick | standard) ;;
    *)
        perf_error "SESSION_PERF_PROFILE must be quick or standard"
        exit 2
        ;;
esac
if [[ ! "$nodes" =~ ^[3-6]$ ]]; then
    perf_error "SESSION_PERF_REDIS_NODES must be 3 to 6 (the dial attestation holds at most 8 endpoints)"
    exit 2
fi
if [[ ! "$port_base" =~ ^[0-9]+$ ]] || (( port_base < 1024 || port_base + nodes - 1 + 10000 > 65535 )); then
    perf_error "SESSION_PERF_REDIS_PORT_BASE must leave room for the client and bus ports"
    exit 2
fi

# The loopback ports a production data tunnel forwards: PostgreSQL, PgBouncer
# and Main's Redis cluster nodes. No client or bus port of the disposable
# cluster may be one of them.
tunnel_port() {
    local port="$1"
    (( port == 5432 || port == 6432 || (port >= 6379 && port <= 6427) ))
}
ports=()
for ((index = 0; index < nodes; index++)); do
    port=$((port_base + index))
    for candidate in "$port" "$((port + 10000))"; do
        if tunnel_port "$candidate"; then
            perf_error "port $candidate is a production data tunnel port; choose another SESSION_PERF_REDIS_PORT_BASE"
            exit 2
        fi
    done
    ports+=("$port")
done

for command_name in docker go git nc; do
    if ! command -v "$command_name" >/dev/null 2>&1; then
        perf_error "missing prerequisite: $command_name"
        exit 1
    fi
done

cd "$server_dir"
# test-env.sh validates the local launcher and exports the endpoint
# attestation every PostgreSQL and Redis dial of the test process is pinned to.
# It is written for a plain shell, so it runs without -e, -u and pipefail.
set +euo pipefail
# shellcheck source=/dev/null
source ./test-env.sh
test_env_status=$?
set -euo pipefail
if [[ "$test_env_status" != 0 || -z "${WARP_TEST_ENV_ATTESTED_ADDRS:-}" ]]; then
    perf_error "test-env.sh did not attest the local test environment; start ./local/run-local.sh"
    exit 1
fi

pg_endpoint="${WARP_TEST_ENV_ATTESTED_ADDRS%%,*}"
pg_host="${pg_endpoint%:*}"
if [[ "$pg_host" != "$expect_db_host" || "${LOCAL_RUN_READY_HOST_IP:-}" != "$expect_db_host" ]]; then
    perf_error "the test database is at $pg_host (launcher ${LOCAL_RUN_READY_HOST_IP:-unset}), not $expect_db_host; refusing"
    exit 1
fi
bind_ip="$LOCAL_RUN_READY_HOST_IP"
if [[ "$bind_ip" == 127.* || "$bind_ip" == "::1" || "$bind_ip" == "0.0.0.0" ]]; then
    perf_error "the launcher address $bind_ip is loopback or unspecified; refusing"
    exit 1
fi

if docker inspect "$container" >/dev/null 2>&1; then
    perf_error "a container named $container already exists; remove it once you know it is a stale harness container"
    exit 1
fi
for port in "${ports[@]}"; do
    if nc -z -w 1 "$bind_ip" "$port" >/dev/null 2>&1; then
        perf_error "$bind_ip:$port already answers; refusing to share it"
        exit 1
    fi
done

lock_held=0
release_lock() {
    if [[ "$lock_held" == 1 ]]; then
        rm -f "$lock_dir/owner"
        rmdir "$lock_dir" 2>/dev/null || true
        lock_held=0
    fi
}
cleanup() {
    docker rm -f "$container" >/dev/null 2>&1 || true
    release_lock
}
trap cleanup EXIT
trap 'exit 130' INT TERM

if [[ -n "$lock_dir" ]]; then
    waited=0
    until mkdir "$lock_dir" 2>/dev/null; do
        if (( waited == 0 )); then
            perf_error "waiting for the DB test lock $lock_dir ($(cat "$lock_dir/owner" 2>/dev/null || echo 'no owner file'))"
        fi
        if (( waited >= lock_wait_seconds )); then
            perf_error "the DB test lock is still held after ${waited}s"
            exit 1
        fi
        sleep 10
        waited=$((waited + 10))
    done
    lock_held=1
    printf 'session perf harness (session/perf/run.sh), pid %s, label %s, since %s\n' "$$" "$label" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" >"$lock_dir/owner"
fi

publish=()
for port in "${ports[@]}"; do
    publish+=(-p "$bind_ip:$port:$port")
done
# Every node of the cluster runs in this one container. The nodes reach each
# other on the container's loopback (cluster bus), and announce the launcher
# address to clients, where the published client ports listen. Settings
# follow Main's redis.conf where they change behavior: no persistence (the
# run is disposable), volatile-ttl, lazy freeing, two I/O threads. The harness
# sets Main's keyspace notification classes itself.
docker run -d --name "$container" --label urnetwork.session-perf=1 \
    -e "PERF_PORTS=${ports[*]}" -e "PERF_HOST=$bind_ip" -e "PERF_MAXMEMORY=$maxmemory" \
    "${publish[@]}" "$image" sh -c '
set -e
for port in $PERF_PORTS; do
    redis-server --port "$port" --bind 0.0.0.0 --protected-mode no --daemonize yes \
        --dir /data --logfile "/data/redis-$port.log" \
        --cluster-enabled yes --cluster-config-file "/data/nodes-$port.conf" \
        --cluster-node-timeout 30000 --cluster-require-full-coverage no \
        --cluster-announce-ip 127.0.0.1 --cluster-announce-hostname "$PERF_HOST" \
        --cluster-preferred-endpoint-type hostname \
        --save "" --appendonly no \
        --maxmemory "$PERF_MAXMEMORY" --maxmemory-policy volatile-ttl \
        --lazyfree-lazy-eviction yes --lazyfree-lazy-expire yes \
        --lazyfree-lazy-server-del yes --lazyfree-lazy-user-del yes \
        --io-threads 2 --hz 10 --dynamic-hz yes --latency-tracking no
done
members=""
for port in $PERF_PORTS; do members="$members 127.0.0.1:$port"; done
sleep 1
redis-cli --cluster create $members --cluster-yes >/data/create.log 2>&1
exec sleep 2147483647
' >/dev/null

ready=0
for _ in $(seq 1 60); do
    if docker exec "$container" redis-cli -p "${ports[0]}" cluster info 2>/dev/null | grep -q '^cluster_state:ok'; then
        ready=1
        break
    fi
    sleep 1
done
if [[ "$ready" != 1 ]]; then
    perf_error "the disposable cluster did not become ready"
    docker logs "$container" >&2 || true
    docker exec "$container" sh -c 'cat /data/create.log /data/redis-*.log' >&2 || true
    exit 1
fi
known="$(docker exec "$container" redis-cli -p "${ports[0]}" cluster info | tr -d '\r' | awk -F: '$1 == "cluster_known_nodes" { print $2 }')"
if [[ "$known" != "$nodes" ]]; then
    perf_error "the disposable cluster knows $known nodes, not $nodes"
    exit 1
fi
for port in "${ports[@]}"; do
    keys="$(docker exec "$container" redis-cli -p "$port" dbsize | tr -d '\r')"
    if [[ "$keys" != 0 ]]; then
        perf_error "node $port of the disposable cluster holds $keys keys"
        exit 1
    fi
done

# The test process may dial the cluster's nodes as well as the launcher's
# PostgreSQL and Redis endpoints, and nothing else.
attested="$WARP_TEST_ENV_ATTESTED_ADDRS"
for port in "${ports[@]}"; do
    attested="$attested,$bind_ip:$port"
done
export WARP_TEST_ENV_ATTESTED_ADDRS="$attested"
export SESSION_PERF_REDIS_CLUSTER="$bind_ip:${ports[0]}"
export SESSION_PERF_REDIS_NODES="$nodes"
export SESSION_PERF_OUT="$out_dir"
export SESSION_PERF_LABEL="$label"
export SESSION_PERF_PROFILE="$profile"
export WARP_TEST_ENV_FAIL_FAST=1

mkdir -p "$out_dir"
printf 'session perf: profile %s, label %s, cluster %s (%s masters), output %s\n' "$profile" "$label" "$SESSION_PERF_REDIS_CLUSTER" "$nodes" "$out_dir"
set +e
go test -count=1 -timeout "$test_timeout" -run '^TestSessionPerf$' -v ./session/perf/ 2>&1 | tee "$out_dir/test.log"
status="${PIPESTATUS[0]}"
set -e
docker exec "$container" sh -c 'for log in /data/redis-*.log; do echo "== $log"; grep -E "WARNING|oom|OOM|# " "$log" | tail -20; done' >"$out_dir/redis-logs.txt" 2>&1 || true
printf 'session perf: exit %s; report %s/report.md\n' "$status" "$out_dir"
exit "$status"
