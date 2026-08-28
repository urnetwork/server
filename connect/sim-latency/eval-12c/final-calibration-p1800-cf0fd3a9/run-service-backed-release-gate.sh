#!/usr/bin/env bash

# Run the PostgreSQL/Redis competition-store gate against the dedicated local
# service address after all latency measurements are idle. Passed evidence is
# written only after the stack is stopped and cleanup is verified.

set -Eeuo pipefail
umask 077
export LANG=C LC_ALL=C

readonly ROOT=/home/by/urnetwork/server/connect/sim-latency/eval-12c/final-calibration-p1800-cf0fd3a9
readonly SERVER=/home/by/urnetwork/server
readonly CONTROL=/home/by/urnetwork/server-finalization-control-plane
readonly OUTPUT_ROOT="$ROOT/production-readiness"
readonly OUTPUT="$OUTPUT_ROOT/service-backed-fifo-cache-failover.json"
readonly FINAL_LOG_ROOT="$OUTPUT_ROOT/service-backed-logs"
readonly SOURCE_LOCK_SHA=0cf71458833f3b1ae96a663357c583eba3a9c25a19d6c795c8549e4154141838
readonly PROTOCOL_SHA=6fc4a809779bf6e694ef3afa71522fa50d0512c56177b42da4249738a37dc7af
readonly CONTROL_COMMIT=2ee4883f2b77cccfcbc69b3bcf1cb4ee613dad36
readonly BOOT_ID=34760d1b-a0b6-46a0-b8c1-264abd1affba
readonly SERVICE_IP=10.213.0.1
readonly MANAGEMENT_CPUS=20,22
readonly SCRIPT_PATH="$(readlink -f "${BASH_SOURCE[0]}")"

stack_pid=""
stack_stopped=false
stack_armed=false
LOG_ROOT=""

log() { printf '[service-backed-gate] %s\n' "$*" >&2; }
die() { log "ERROR: $*"; exit 1; }
sha256_file() { sha256sum "$1" | awk '{print $1}'; }
require_command() { command -v "$1" >/dev/null 2>&1 || die "missing command: $1"; }

remove_managed_hosts_block() {
    local marker_begin marker_end temporary
    marker_begin='# >>> urnetwork local-env (server/local/run-local.sh) >>>'
    marker_end='# <<< urnetwork local-env (server/local/run-local.sh) <<<'
    rg -q --fixed-strings "$marker_begin" /etc/hosts || return 0
    temporary="$(mktemp /tmp/urnetwork-service-gate-hosts.XXXXXXXX)"
    awk -v begin="$marker_begin" -v end="$marker_end" '
      $0 == begin { inside=1; next }
      $0 == end { inside=0; next }
      inside != 1 { print }
    ' /etc/hosts >"$temporary"
    if [ ! -s "$temporary" ]; then
        rm -f "$temporary"
        log "refusing to replace /etc/hosts with an empty file"
        return 1
    fi
    sudo -n cp "$temporary" /etc/hosts
    rm -f "$temporary"
}

stop_stack() {
    local rc=0 unused local_container
    if [ "$stack_stopped" = true ]; then
        return 0
    fi
    if [ "$stack_armed" != true ]; then
        stack_stopped=true
        return 0
    fi
    if [ -n "$stack_pid" ] && kill -0 "$stack_pid" 2>/dev/null; then
        kill -TERM "$stack_pid" 2>/dev/null || true
        for unused in $(seq 1 120); do
            kill -0 "$stack_pid" 2>/dev/null || break
            sleep 1
        done
        if kill -0 "$stack_pid" 2>/dev/null; then
            log "local service stack did not stop after TERM; sending KILL"
            kill -KILL "$stack_pid" 2>/dev/null || true
        fi
        wait "$stack_pid" 2>/dev/null || true
    fi
    for local_container in urnetwork-local-pg urnetwork-local-redis; do
        if [ -n "$(sudo -n docker ps -aq --filter "name=^/${local_container}$")" ]; then
            sudo -n docker rm -f "$local_container" >/dev/null 2>&1 || rc=1
        fi
    done
    if sudo -n docker network inspect urnetwork-local >/dev/null 2>&1; then
        sudo -n docker network rm urnetwork-local >/dev/null 2>&1 || rc=1
    fi
    sudo -n ip address del "$SERVICE_IP/32" dev lo >/dev/null 2>&1 || true
    remove_managed_hosts_block || rc=1
    [ -z "$(sudo -n docker ps -aq --filter 'name=^/urnetwork-local-pg$')" ] || rc=1
    [ -z "$(sudo -n docker ps -aq --filter 'name=^/urnetwork-local-redis$')" ] || rc=1
    ! sudo -n docker network inspect urnetwork-local >/dev/null 2>&1 || rc=1
    ! ip -brief address show dev lo | rg -q "(^|[[:space:]])$SERVICE_IP/32([[:space:]]|$)" || rc=1
    ! rg -q --fixed-strings '# >>> urnetwork local-env (server/local/run-local.sh) >>>' /etc/hosts || rc=1
    if [ "$rc" -eq 0 ]; then
        stack_armed=false
        stack_stopped=true
    fi
    return "$rc"
}

on_exit() {
    local rc=$?
    trap - EXIT INT TERM
    if [ "$stack_stopped" != true ]; then
        if ! stop_stack && [ "$rc" -eq 0 ]; then
            rc=1
        fi
    fi
    exit "$rc"
}
trap on_exit EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

for command in awk chmod chown cp date docker env getent git go install ip jq kill mktemp mv readlink rg rm seq sha256sum sleep stat sudo systemctl taskset; do
    require_command "$command"
done

case "${1:-}" in
    --self-test)
        [ "$#" -eq 1 ] || die "--self-test takes no arguments"
        rg -q 'LOG_ROOT="\$\(mktemp -d "\$ROOT/control-plane-release/\.service-backed-gate\.XXXXXXXX"\)"' "$SCRIPT_PATH" ||
            die "service logs do not begin in a retained unprivileged workspace"
        rg -q 'sudo -n mv -- "\$LOG_ROOT" "\$FINAL_LOG_ROOT"' "$SCRIPT_PATH" ||
            die "service logs do not cross the privileged boundary by exact move"
        rg -q 'sudo -n install -o root -g root -m 0400 "\$output_pending" "\$OUTPUT"' "$SCRIPT_PATH" ||
            die "service evidence does not cross the privileged boundary by exact install"
        ! rg -q '\}\x27 >"\$OUTPUT"' "$SCRIPT_PATH" ||
            die "service gate writes directly through the privileged readiness boundary"
        log "self-test passed: root-only readiness evidence uses exact privileged installs"
        exit 0
        ;;
    '') [ "$#" -eq 0 ] || die "unexpected arguments" ;;
    *) die "usage: $0 [--self-test]" ;;
esac

[ "$(cat /proc/sys/kernel/random/boot_id)" = "$BOOT_ID" ] || die "boot identity changed"
[ "$(sha256_file "$ROOT/source-lock.json")" = "$SOURCE_LOCK_SHA" ] || die "source lock changed"
[ "$(sha256_file "$ROOT/production-staging-protocol.json")" = "$PROTOCOL_SHA" ] || die "staging protocol changed"
[ "$(git -C "$CONTROL" rev-parse HEAD)" = "$CONTROL_COMMIT" ] || die "control-plane commit changed"
[ -z "$(git -C "$CONTROL" status --porcelain --untracked-files=no)" ] || die "control-plane tracked worktree is dirty"
sudo -n test ! -e "$OUTPUT" || die "passed service evidence already exists"
sudo -n test ! -e "$FINAL_LOG_ROOT" || die "passed service logs already exist"

for service in urnetwork-final-calibration-recovery-8c7cfc98.service urnetwork-final-independent-r1-da4ee86a.service; do
    state="$(systemctl is-active "$service" 2>/dev/null || true)"
    case "$state" in
        inactive|failed|unknown) ;;
        *) die "measurement service is not idle: $service ($state)" ;;
    esac
done
[ -z "$(sudo -n docker ps -q --filter label=com.urnetwork.competition.job-id)" ] ||
    die "competition containers remain active"
[ -z "$(sudo -n docker ps -aq --filter 'name=^/urnetwork-local-pg$')" ] || die "local PostgreSQL already exists"
[ -z "$(sudo -n docker ps -aq --filter 'name=^/urnetwork-local-redis$')" ] || die "local Redis already exists"
! sudo -n docker network inspect urnetwork-local >/dev/null 2>&1 || die "local service network already exists"
! ip -brief address show dev lo | rg -q "(^|[[:space:]])$SERVICE_IP/32([[:space:]]|$)" || die "local service alias already exists"
! rg -q --fixed-strings '# >>> urnetwork local-env (server/local/run-local.sh) >>>' /etc/hosts ||
    die "managed local-service hosts block already exists"

if awk '$0 !~ /^[[:space:]]*#/ && $1 == "127.0.0.1" && ($0 ~ /local-pg\.bringyour\.com/ || $0 ~ /local-redis\.bringyour\.com/) {found=1} END {exit !found}' /etc/hosts; then
    die "/etc/hosts contains a forbidden localhost database mapping"
fi

sudo -n install -d -o root -g root -m 0700 "$OUTPUT_ROOT"
LOG_ROOT="$(mktemp -d "$ROOT/control-plane-release/.service-backed-gate.XXXXXXXX")"
log "retained service-gate workspace: $LOG_ROOT"

log "starting the dedicated local PostgreSQL/Redis stack"
stack_armed=true
env \
    WARP_ENV=local \
    WARP_CONFIG_HOME=/home/by/urnetwork/config \
    WARP_VAULT_HOME=/home/by/urnetwork/vault \
    BRINGYOUR_POSTGRES_HOSTNAME=local-pg.bringyour.com \
    BRINGYOUR_REDIS_HOSTNAME=local-redis.bringyour.com \
    LOCAL_HOST_IP="$SERVICE_IP" \
    "$SERVER/local/run-local.sh" >"$LOG_ROOT/stack.log" 2>&1 &
stack_pid=$!

ready=false
for unused in $(seq 1 120); do
    kill -0 "$stack_pid" 2>/dev/null || die "local service stack exited during startup"
    pg_address="$(getent ahostsv4 local-pg.bringyour.com 2>/dev/null | awk 'NR==1 {print $1}')"
    redis_address="$(getent ahostsv4 local-redis.bringyour.com 2>/dev/null | awk 'NR==1 {print $1}')"
    if [ "$pg_address" = "$SERVICE_IP" ] && [ "$redis_address" = "$SERVICE_IP" ] &&
       [ "$(sudo -n docker inspect -f '{{.State.Health.Status}}' urnetwork-local-pg 2>/dev/null || true)" = healthy ] &&
       [ "$(sudo -n docker inspect -f '{{.State.Health.Status}}' urnetwork-local-redis 2>/dev/null || true)" = healthy ]; then
        ready=true
        break
    fi
    sleep 1
done
[ "$ready" = true ] || die "dedicated local service stack did not become ready"

sudo -n docker inspect urnetwork-local-pg urnetwork-local-redis >"$LOG_ROOT/service-inspect.json"
jq -e --arg ip "$SERVICE_IP" '
  length==2 and
  all(.[]; .State.Health.Status=="healthy") and
  all(.[]; ([.NetworkSettings.Ports[]?[]?.HostIp] | length)>0 and ([.NetworkSettings.Ports[]?[]?.HostIp] | all(.==$ip)))' \
    "$LOG_ROOT/service-inspect.json" >/dev/null || die "service ports escaped the dedicated address"

log "checking origin-before-local migration order"
(
    cd "$CONTROL"
    env -u GOFLAGS WARP_ENV=local GOMAXPROCS=2 GOPROXY=off GOTOOLCHAIN=local \
        taskset -c "$MANAGEMENT_CPUS" \
        go test -mod=readonly -timeout=5m . \
            -run '^TestCompetitionMigrationsFollowOriginMigrations$' -count=1 -v
) >"$LOG_ROOT/migration-order.log" 2>&1
rg -q -- '--- PASS: TestCompetitionMigrationsFollowOriginMigrations' "$LOG_ROOT/migration-order.log" ||
    die "migration-order test did not report a pass"

log "running FIFO/cache/failover/immutability integration"
(
    cd "$CONTROL"
    env -u GOFLAGS \
        WARP_ENV=local \
        WARP_CONFIG_HOME=/home/by/urnetwork/config \
        WARP_VAULT_HOME=/home/by/urnetwork/vault \
        BRINGYOUR_POSTGRES_HOSTNAME=local-pg.bringyour.com \
        BRINGYOUR_REDIS_HOSTNAME=local-redis.bringyour.com \
        GOMAXPROCS=2 \
        GOPROXY=off \
        GOTOOLCHAIN=local \
        taskset -c "$MANAGEMENT_CPUS" \
        go test -mod=readonly -timeout=20m ./competition \
            -run '^TestPostgresStoreQueueCacheFailoverAndImmutability$' \
            -count=1 -v
) >"$LOG_ROOT/store-integration.log" 2>&1
rg -q -- '--- PASS: TestPostgresStoreQueueCacheFailoverAndImmutability' "$LOG_ROOT/store-integration.log" ||
    die "service-backed integration did not report a pass"

stop_stack
stack_pid=""

[ -z "$(sudo -n docker ps -aq --filter name='^urnetwork-local-pg$')" ] || die "PostgreSQL container survived cleanup"
[ -z "$(sudo -n docker ps -aq --filter name='^urnetwork-local-redis$')" ] || die "Redis container survived cleanup"
if ip -brief address show dev lo | rg -q "(^|[[:space:]])$SERVICE_IP/32([[:space:]]|$)"; then
    die "dedicated loopback alias survived cleanup"
fi
if awk '$0 ~ /urnetwork local-env \(server\/local\/run-local\.sh\)/ {found=1} END {exit !found}' /etc/hosts; then
    die "managed /etc/hosts block survived cleanup"
fi

chmod 0400 "$LOG_ROOT/stack.log" "$LOG_ROOT/service-inspect.json" "$LOG_ROOT/migration-order.log" "$LOG_ROOT/store-integration.log"

output_pending="$(mktemp "$ROOT/control-plane-release/.service-backed-check.XXXXXXXX")"
jq -n \
    --arg generated_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    --arg source_lock_sha256 "$SOURCE_LOCK_SHA" \
    --arg protocol_sha256 "$PROTOCOL_SHA" \
    --arg control_commit "$CONTROL_COMMIT" \
    --arg service_ip "$SERVICE_IP" \
    --arg migration_test_sha256 "$(sha256_file "$CONTROL/db_migrations_test.go")" \
    --arg store_test_sha256 "$(sha256_file "$CONTROL/competition/store_integration_test.go")" \
    --arg stack_log_sha256 "$(sha256_file "$LOG_ROOT/stack.log")" \
    --arg inspect_sha256 "$(sha256_file "$LOG_ROOT/service-inspect.json")" \
    --arg migration_log_sha256 "$(sha256_file "$LOG_ROOT/migration-order.log")" \
    --arg store_log_sha256 "$(sha256_file "$LOG_ROOT/store-integration.log")" \
    '{
      schema:1,
      kind:"sim-latency-production-readiness-check",
      check_id:"service_backed_fifo_cache_failover",
      passed:true,
      generated_at:$generated_at,
      source_lock_sha256:$source_lock_sha256,
      production_staging_protocol_sha256:$protocol_sha256,
      control_plane_commit:$control_commit,
      dedicated_service_ip:$service_ip,
      test_source_sha256:{migration_order:$migration_test_sha256,store_integration:$store_test_sha256},
      logs:{stack:$stack_log_sha256,service_inspect:$inspect_sha256,migration_order:$migration_log_sha256,store_integration:$store_log_sha256},
      assertions:{
        postgres_dedicated_address:true,
        redis_dedicated_address:true,
        origin_migrations_before_local:true,
        fifo_verified:true,
        cache_acl_verified:true,
        singleton_slot_verified:true,
        lease_failover_verified:true,
        infrastructure_retry_verified:true,
        terminal_fields_immutable:true,
        event_log_append_only:true,
        test_exit_zero:true
      }
    }' >"$output_pending"

jq -e '.schema==1 and .passed==true and (.assertions|length)==11 and all(.assertions[]; .==true)' "$output_pending" >/dev/null
chmod 0400 "$output_pending"
output_sha="$(sha256_file "$output_pending")"
sudo -n chown root:root \
    "$LOG_ROOT/stack.log" \
    "$LOG_ROOT/service-inspect.json" \
    "$LOG_ROOT/migration-order.log" \
    "$LOG_ROOT/store-integration.log" \
    "$LOG_ROOT"
sudo -n chmod 0400 \
    "$LOG_ROOT/stack.log" \
    "$LOG_ROOT/service-inspect.json" \
    "$LOG_ROOT/migration-order.log" \
    "$LOG_ROOT/store-integration.log"
sudo -n chmod 0500 "$LOG_ROOT"
sudo -n mv -- "$LOG_ROOT" "$FINAL_LOG_ROOT"
sudo -n install -o root -g root -m 0400 "$output_pending" "$OUTPUT"
[ "$(sudo -n sha256sum "$OUTPUT" | awk '{print $1}')" = "$output_sha" ] ||
    die "installed service-backed evidence changed"
rm -f -- "$output_pending"
trap - EXIT
log "passed evidence: $OUTPUT"
printf '%s\n' "$output_sha"
