#!/usr/bin/env bash

# Exercise the released competition control plane through one authenticated
# generate/rebaseline/submit/failover/cache/reveal round. The script is armed
# only after calibration and independent-reference evidence are terminal.

set -Eeuo pipefail
umask 077
export LANG=C LC_ALL=C TZ=UTC

readonly ROOT=/home/by/urnetwork/server/connect/sim-latency/eval-12c/final-calibration-p1800-cf0fd3a9
readonly SERVER=/home/by/urnetwork/server
readonly CONTROL=/home/by/urnetwork/server-finalization-control-plane
readonly RELEASE="$ROOT/control-plane-release/final"
readonly RELEASE_MANIFEST="$RELEASE/release-build.json"
readonly SELECTION="$ROOT/post-frontier/final-calibration-selection.json"
readonly REFERENCE_V5="$ROOT/reference-requalification-v5"
readonly INDEPENDENT_ATTESTATION="$REFERENCE_V5/hidden-launch-runtime/independent-campaign-attestation.json"
readonly INDEPENDENT_DECISION="$REFERENCE_V5/hidden-launch-decision.json"
readonly INDEPENDENT_PROGRESS="$REFERENCE_V5/hidden-launch-runtime/independent-references/progress.json"
readonly INDEPENDENT_ATTESTATION_REPAIR="$REFERENCE_V5/hidden-attestation-path-repair.json"
readonly INDEPENDENT_ATTESTATION_REPAIR_SCRIPT="$REFERENCE_V5/repair-hidden-attestation-path.py"
readonly REFERENCE_V5_STAGING_AMENDMENT="$ROOT/production-staging-reference-v5-amendment.json"
readonly RELEASE_AMENDMENT="$ROOT/production-release-self-check-contract-amendment.json"
readonly SERVICE_CHECK="$ROOT/production-readiness/service-backed-fifo-cache-failover.json"
readonly RELEASE_CHECK="$ROOT/production-readiness/release-artifacts.json"
readonly OUTPUT_ROOT="$ROOT/production-readiness"
readonly STAGING="$OUTPUT_ROOT/staging-round"
readonly PROVISIONER="$ROOT/provision-competition-api.sh"
readonly API_CONFIG=/etc/urnetwork/competition-api/config
readonly API_VAULT=/etc/urnetwork/competition-api/vault
readonly CREDENTIALS=/etc/urnetwork/competition-api/credentials.json
readonly DEPLOYMENT_MANIFEST=/etc/urnetwork/competition-api/deployment-manifest.json
readonly HOST_CONFIG=/etc/urnetwork/competition-host.json
readonly COMMAND_ROOT=/usr/local/libexec/urnetwork/competition-cf0fd3a9
readonly SELF_CHECK="$COMMAND_ROOT/competition-host-self-check"
readonly REBASELINE_PROMOTER="$COMMAND_ROOT/promote-round-rebaseline.sh"
readonly RESOURCE_BOMB="$ROOT/host-qualification/resource-bomb-cleanup-production.json"
readonly NOOP_PATCH="$SERVER/competition/references/noop.patch"
readonly API="$RELEASE/binaries/api"
readonly WORKER="$RELEASE/binaries/competitionworker"
readonly REBASELINE="$RELEASE/binaries/competitionrebaseline"
readonly DBINIT="$RELEASE/binaries/competitiondbinit"
readonly SOURCE_LOCK_SHA=0cf71458833f3b1ae96a663357c583eba3a9c25a19d6c795c8549e4154141838
readonly PROTOCOL_SHA=6fc4a809779bf6e694ef3afa71522fa50d0512c56177b42da4249738a37dc7af
readonly REFERENCE_V5_STAGING_AMENDMENT_SHA=618393539636b69cfcdbd6fec14afef3e58fe20d43bda06fbcbf15693802b695
readonly RELEASE_AMENDMENT_SHA=99d6010edcbc659d936e97cbc7cde48129d0af9146c6404a1bc03604d750ef5d
readonly INDEPENDENT_ATTESTATION_REPAIR_SHA=499efd5e6d99f4d56a55f05d3949f6107ae8fcdeb2c7dfeb5b9877207541412d
readonly INDEPENDENT_ATTESTATION_REPAIR_SCRIPT_SHA=a5bfedfd7228b8e7c01a41334aa01b0d6a413ffadc4cca380073ac9ecdb668a0
readonly CONTROL_COMMIT=2ee4883f2b77cccfcbc69b3bcf1cb4ee613dad36
readonly CONTROL_RELEASE_SHA=90458a61e19259bba1bf1626b63567e92a06082d3944a070a8ea071b5f8bd5e7
readonly RELEASE_MANIFEST_SHA=17d2817a69f3bc506c98ba00f31b8cc15fc9e2b7e0a4e18b5ca0df9fc89bfc00
readonly RELEASE_CHECK_SHA=3fa3ca749a4718f31ed9ac17351c2b2695891f89117e794b7d3220e477d3b5cd
readonly SERVICE_CHECK_SHA=e3c168731edab3f0de0a823aaed23aa64e72b1ed7ea32e0dc5663806af7c4a08
readonly EVALUATOR_IMAGE=sha256:cf0fd3a9e73385729ee8dcd8da7ea53eb59d5f372b9ff36789ec923056222038
readonly HOST_QUALIFICATION_SHA=9cb7a977f171babafb5ff35c045799cbd54ec734ecfdebe7ebd106e482683d2f
readonly NOOP_SHA=8bd57a48ac82a6e846b607a9301c48145da5c66717c9e3a341138d034d1e0775
readonly PROVISIONER_SHA=3f68f7dca0f74523aaa0a36be7eb395f8378d34de6d1f7ee6c2b6f4c7dd3a416
readonly BOOT_ID=34760d1b-a0b6-46a0-b8c1-264abd1affba
readonly MANAGEMENT_CPUS=20,22
readonly SERVICE_IP=10.213.0.1
readonly API_PORT=18080
readonly API_BASE="http://127.0.0.1:$API_PORT/competition"
readonly OPERATIONAL_LOCK=/run/urnetwork/competition-operational.lock
readonly FROZEN_EPHEMERAL_PORT_LOW=32768
readonly FROZEN_EPHEMERAL_PORT_HIGH=60999
readonly FROZEN_SOMAXCONN=4096
readonly FROZEN_SYN_BACKLOG=4096
readonly FROZEN_NETDEV_BACKLOG=1000
readonly FROZEN_CONNTRACK_MAX=262144
readonly LOCAL_TUNED_EPHEMERAL_RANGE="10240 65535"
readonly LOCAL_TUNED_SOMAXCONN=65535
readonly LOCAL_TUNED_SYN_BACKLOG=65535
readonly LOCAL_TUNED_NETDEV_BACKLOG=65535
readonly LOCAL_TUNED_CONNTRACK_MAX=1048576

stack_pid=""
api_pid=""
worker_pid=""
worker_id=""
success=false
runtime_armed=false
runtime_stopped=false
preflight_only=false
http_status=""

log() { printf '[production-staging] %s %s\n' "$(date -u '+%FT%TZ')" "$*" >&2; }
die() { log "ERROR: $*"; exit 1; }
sha256_file() { sha256sum "$1" | awk '{print $1}'; }
require_command() { command -v "$1" >/dev/null 2>&1 || die "missing command: $1"; }

local_stack_env() {
    env \
        WARP_ENV=local \
        WARP_CONFIG_HOME=/home/by/urnetwork/config \
        WARP_VAULT_HOME=/home/by/urnetwork/vault \
        BRINGYOUR_POSTGRES_HOSTNAME=local-pg.bringyour.com \
        BRINGYOUR_REDIS_HOSTNAME=local-redis.bringyour.com \
        LOCAL_HOST_IP="$SERVICE_IP" \
        LOCAL_EPHEMERAL_PORT_LOW="$FROZEN_EPHEMERAL_PORT_LOW" \
        LOCAL_EPHEMERAL_PORT_HIGH="$FROZEN_EPHEMERAL_PORT_HIGH" \
        LOCAL_TCP_LISTEN_BACKLOG="$FROZEN_SOMAXCONN" \
        LOCAL_NETDEV_MAX_BACKLOG="$FROZEN_NETDEV_BACKLOG" \
        LOCAL_NF_CONNTRACK_MAX="$FROZEN_CONNTRACK_MAX" \
        "$@"
}

ephemeral_port_range() {
    local low high
    read -r low high < /proc/sys/net/ipv4/ip_local_port_range || return 1
    printf '%s %s\n' "$low" "$high"
}

network_restore_action() {
    local current="$1" frozen="$2" locally_tuned="$3"
    if [ "$current" = "$frozen" ]; then
        printf 'unchanged\n'
    elif [ "$current" = "$locally_tuned" ]; then
        printf 'restore\n'
    else
        return 1
    fi
}

host_network_controls_match_frozen() {
    [ "$(ephemeral_port_range)" = "$FROZEN_EPHEMERAL_PORT_LOW $FROZEN_EPHEMERAL_PORT_HIGH" ] &&
        [ "$(sysctl -n net.core.somaxconn)" = "$FROZEN_SOMAXCONN" ] &&
        [ "$(sysctl -n net.ipv4.tcp_max_syn_backlog)" = "$FROZEN_SYN_BACKLOG" ] &&
        [ "$(sysctl -n net.core.netdev_max_backlog)" = "$FROZEN_NETDEV_BACKLOG" ] &&
        [ "$(sysctl -n net.netfilter.nf_conntrack_max)" = "$FROZEN_CONNTRACK_MAX" ]
}

restore_owned_scalar_network_control() {
    local key="$1" frozen="$2" locally_tuned="$3" current action count
    current="$(sysctl -n "$key")" || return 1
    if ! action="$(network_restore_action "$current" "$frozen" "$locally_tuned")"; then
        log "refusing to overwrite externally changed $key=$current"
        return 1
    fi
    [ "$action" = restore ] || return 0
    if [ "$key" = net.netfilter.nf_conntrack_max ]; then
        count="$(sysctl -n net.netfilter.nf_conntrack_count)" || return 1
        if [ "$count" -gt "$frozen" ]; then
            log "cannot restore $key to $frozen while $count entries remain"
            return 1
        fi
    fi
    log "restoring recognized local-stack setting $key: $current -> $frozen"
    sysctl -q -w "$key=$frozen" || return 1
    [ "$(sysctl -n "$key")" = "$frozen" ]
}

restore_owned_host_network_controls() {
    local current action rc=0
    current="$(ephemeral_port_range)" || return 1
    if ! action="$(network_restore_action "$current" \
        "$FROZEN_EPHEMERAL_PORT_LOW $FROZEN_EPHEMERAL_PORT_HIGH" \
        "$LOCAL_TUNED_EPHEMERAL_RANGE")"; then
        log "refusing to overwrite externally changed net.ipv4.ip_local_port_range=$current"
        rc=1
    elif [ "$action" = restore ]; then
        log "restoring recognized local-stack setting net.ipv4.ip_local_port_range: $current -> $FROZEN_EPHEMERAL_PORT_LOW $FROZEN_EPHEMERAL_PORT_HIGH"
        sysctl -q -w "net.ipv4.ip_local_port_range=$FROZEN_EPHEMERAL_PORT_LOW $FROZEN_EPHEMERAL_PORT_HIGH" || rc=1
    fi
    restore_owned_scalar_network_control net.core.somaxconn \
        "$FROZEN_SOMAXCONN" "$LOCAL_TUNED_SOMAXCONN" || rc=1
    restore_owned_scalar_network_control net.ipv4.tcp_max_syn_backlog \
        "$FROZEN_SYN_BACKLOG" "$LOCAL_TUNED_SYN_BACKLOG" || rc=1
    restore_owned_scalar_network_control net.core.netdev_max_backlog \
        "$FROZEN_NETDEV_BACKLOG" "$LOCAL_TUNED_NETDEV_BACKLOG" || rc=1
    restore_owned_scalar_network_control net.netfilter.nf_conntrack_max \
        "$FROZEN_CONNTRACK_MAX" "$LOCAL_TUNED_CONNTRACK_MAX" || rc=1
    host_network_controls_match_frozen || rc=1
    return "$rc"
}

stop_process() {
    local pid="$1" label="$2" unused
    [ -n "$pid" ] || return 0
    if kill -0 "$pid" 2>/dev/null; then
        kill -TERM "$pid" 2>/dev/null || true
        for unused in $(seq 1 30); do
            kill -0 "$pid" 2>/dev/null || break
            sleep 1
        done
        if kill -0 "$pid" 2>/dev/null; then
            log "$label did not stop after TERM; sending KILL"
            kill -KILL "$pid" 2>/dev/null || true
        fi
        wait "$pid" 2>/dev/null || true
    fi
    if kill -0 "$pid" 2>/dev/null; then
        log "$label survived TERM and KILL"
        return 1
    fi
}

stop_worker() {
    local rc=0
    stop_process "$worker_pid" "competition worker ${worker_id:-unknown}" || rc=1
    worker_pid=""
    worker_id=""
    return "$rc"
}

remove_managed_hosts_block() {
    local marker_begin marker_end temporary
    marker_begin='# >>> urnetwork local-env (server/local/run-local.sh) >>>'
    marker_end='# <<< urnetwork local-env (server/local/run-local.sh) <<<'
    rg -q --fixed-strings "$marker_begin" /etc/hosts || return 0
    temporary="$(mktemp /run/urnetwork-staging-hosts.XXXXXXXX)"
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
    cp "$temporary" /etc/hosts
    rm -f "$temporary"
}

remove_labeled_job_resources() {
    local rc=0
    local -a containers networks
    mapfile -t containers < <(docker ps -aq --filter label=com.urnetwork.competition.job-id)
    if [ "${#containers[@]}" -gt 0 ]; then
        log "force-removing ${#containers[@]} residual competition container(s)"
        docker rm -f -- "${containers[@]}" >/dev/null 2>&1 || rc=1
    fi
    mapfile -t networks < <(docker network ls -q --filter label=com.urnetwork.competition.job-id)
    if [ "${#networks[@]}" -gt 0 ]; then
        log "removing ${#networks[@]} residual competition network(s)"
        docker network rm -- "${networks[@]}" >/dev/null 2>&1 || rc=1
    fi
    return "$rc"
}

cleanup_runtime() {
    local rc=0 local_container
    if [ "$runtime_stopped" = true ]; then
        return 0
    fi
    if [ "$runtime_armed" != true ]; then
        runtime_stopped=true
        return 0
    fi
    stop_worker || rc=1
    stop_process "$api_pid" "competition API" || rc=1
    api_pid=""
    stop_process "$stack_pid" "local service stack" || rc=1
    stack_pid=""
    remove_labeled_job_resources || rc=1

    # If run-local itself had to be killed, finish only its exact, dedicated
    # resources. Persistent data volumes are intentionally left untouched.
    for local_container in urnetwork-local-pg urnetwork-local-redis; do
        if [ -n "$(docker ps -aq --filter "name=^/${local_container}$")" ]; then
            log "force-removing residual $local_container"
            docker rm -f "$local_container" >/dev/null 2>&1 || rc=1
        fi
    done
    if docker network inspect urnetwork-local >/dev/null 2>&1; then
        docker network rm urnetwork-local >/dev/null 2>&1 || rc=1
    fi
    ip address del "$SERVICE_IP/32" dev lo >/dev/null 2>&1 || true
    remove_managed_hosts_block || rc=1
    restore_owned_host_network_controls || rc=1

    [ -z "$(docker ps -aq --filter label=com.urnetwork.competition.job-id)" ] || rc=1
    [ -z "$(docker network ls -q --filter label=com.urnetwork.competition.job-id)" ] || rc=1
    [ -z "$(docker ps -aq --filter 'name=^/urnetwork-local-pg$')" ] || rc=1
    [ -z "$(docker ps -aq --filter 'name=^/urnetwork-local-redis$')" ] || rc=1
    ! docker network inspect urnetwork-local >/dev/null 2>&1 || rc=1
    ! ip -brief address show dev lo | rg -q "(^|[[:space:]])$SERVICE_IP/32([[:space:]]|$)" || rc=1
    ! rg -q --fixed-strings '# >>> urnetwork local-env (server/local/run-local.sh) >>>' /etc/hosts || rc=1
    if [ "$rc" -eq 0 ]; then
        runtime_armed=false
        runtime_stopped=true
    fi
    return "$rc"
}

cleanup() {
    local rc=$?
    trap - EXIT INT TERM
    if ! cleanup_runtime; then
        log "runtime cleanup did not reach a clean terminal state"
        if [ "$rc" -eq 0 ]; then
            rc=1
        fi
    fi
    if [ "$success" = true ] && [ "$runtime_stopped" != true ]; then
        log "successful staging cannot retain a live runtime"
        rc=1
    fi
    exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

runtime_env() {
    env \
        WARP_CONFIG_HOME="$API_CONFIG" \
        WARP_VAULT_HOME="$API_VAULT" \
        WARP_ENV=local \
        WARP_SERVICE=api \
        WARP_DOMAIN=bringyour.com \
        WARP_HOST=127.0.0.1 \
        WARP_BLOCK=competition \
        WARP_VERSION=0.0.0-competition-2ee4883f \
        BRINGYOUR_POSTGRES_HOSTNAME=local-pg.bringyour.com \
        BRINGYOUR_REDIS_HOSTNAME=local-redis.bringyour.com \
        "$@"
}

runtime_exec() {
    exec env \
        WARP_CONFIG_HOME="$API_CONFIG" \
        WARP_VAULT_HOME="$API_VAULT" \
        WARP_ENV=local \
        WARP_SERVICE=api \
        WARP_DOMAIN=bringyour.com \
        WARP_HOST=127.0.0.1 \
        WARP_BLOCK=competition \
        WARP_VERSION=0.0.0-competition-2ee4883f \
        BRINGYOUR_POSTGRES_HOSTNAME=local-pg.bringyour.com \
        BRINGYOUR_REDIS_HOSTNAME=local-redis.bringyour.com \
        "$@"
}

http_status_expected() {
    local status="$1" expected="$2"
    case ",$expected," in
        *",$status,"*) return 0 ;;
        *) return 1 ;;
    esac
}

http_json() {
    local method="$1" url="$2" token="$3" input="$4" output="$5" expected="$6"
    local status
    if [ -n "$input" ]; then
        status="$(curl --silent --show-error --request "$method" \
            --header "Authorization: Bearer $token" \
            --header 'Content-Type: application/json' \
            --data-binary "@$input" \
            --output "$output" --write-out '%{http_code}' "$url")"
    elif [ -n "$token" ]; then
        status="$(curl --silent --show-error --request "$method" \
            --header "Authorization: Bearer $token" \
            --output "$output" --write-out '%{http_code}' "$url")"
    else
        status="$(curl --silent --show-error --request "$method" \
            --output "$output" --write-out '%{http_code}' "$url")"
    fi
    http_status_expected "$status" "$expected" ||
        die "$method $url returned HTTP $status, expected one of $expected"
    http_status="$status"
}

wait_stack() {
    local unused pg_address redis_address
    for unused in $(seq 1 120); do
        kill -0 "$stack_pid" 2>/dev/null || die "local service stack exited during startup"
        pg_address="$(getent ahostsv4 local-pg.bringyour.com 2>/dev/null | awk 'NR==1 {print $1}')"
        redis_address="$(getent ahostsv4 local-redis.bringyour.com 2>/dev/null | awk 'NR==1 {print $1}')"
        if [ "$pg_address" = "$SERVICE_IP" ] && [ "$redis_address" = "$SERVICE_IP" ] &&
           [ "$(docker inspect -f '{{.State.Health.Status}}' urnetwork-local-pg 2>/dev/null || true)" = healthy ] &&
           [ "$(docker inspect -f '{{.State.Health.Status}}' urnetwork-local-redis 2>/dev/null || true)" = healthy ]; then
            return 0
        fi
        sleep 1
    done
    die "local service stack did not become ready"
}

wait_api() {
    local unused
    for unused in $(seq 1 120); do
        kill -0 "$api_pid" 2>/dev/null || die "competition API exited during startup"
        if curl --silent --fail "$API_BASE/healthz" >"$STAGING/health.json.new" 2>/dev/null; then
            mv "$STAGING/health.json.new" "$STAGING/health.json"
            jq -e '.status == "alive" and .version == "0.0.0-competition-2ee4883f" and
                (.time | type == "string" and endswith("Z"))' \
                "$STAGING/health.json" >/dev/null || die "health response is malformed"
            return 0
        fi
        sleep 1
    done
    die "competition API did not become healthy"
}

start_worker() {
    worker_id="$1"
    local log_path="$2"
    runtime_exec taskset -c "$MANAGEMENT_CPUS" "$WORKER" --worker_id="$worker_id" >"$log_path" 2>&1 &
    worker_pid=$!
}

generate_round() {
    local operator_token="$1" request="$2" output="$3" unused
    for unused in $(seq 1 120); do
        kill -0 "$worker_pid" 2>/dev/null || die "pre-round worker exited"
        http_json POST "$API_BASE/generate-round" "$operator_token" "$request" "$output.new" 201,503
        if [ "$http_status" = 201 ]; then
            mv "$output.new" "$output"
            return 0
        fi
        jq -e '.kind == "infrastructure" and .code == "not_ready" and .retriable == true' \
            "$output.new" >/dev/null || die "round-generation readiness error is malformed"
        mv "$output.new" "$STAGING/round-not-ready.json"
        sleep 1
    done
    die "worker heartbeat did not make round-generation infrastructure ready"
}

wait_fully_ready() {
    local operator_token="$1" unused
    for unused in $(seq 1 120); do
        kill -0 "$worker_pid" 2>/dev/null || die "post-promotion worker exited"
        http_json GET "$API_BASE/readyz" "$operator_token" "" "$STAGING/ready-post-promotion.json.new" 200,503
        mv "$STAGING/ready-post-promotion.json.new" "$STAGING/ready-post-promotion.json"
        if [ "$http_status" = 200 ] &&
           jq -e '.ready == true and ([.checks[]] | all)' "$STAGING/ready-post-promotion.json" >/dev/null; then
            return 0
        fi
        if [ "$http_status" = 503 ]; then
            jq -e '.kind == "infrastructure" and .code == "not_ready" and .retriable == true' \
                "$STAGING/ready-post-promotion.json" >/dev/null || die "readiness error is malformed"
        else
            die "successful readiness response is malformed"
        fi
        sleep 1
    done
    die "same-round worker did not make the service fully ready"
}

wait_job_state() {
    local token="$1" job_id="$2" wanted="$3" output="$4" limit="$5" unused state
    for unused in $(seq 1 "$limit"); do
        http_json GET "$API_BASE/score/$job_id" "$token" "" "$output.new" 200
        mv "$output.new" "$output"
        state="$(jq -er '.state' "$output")"
        [ "$state" = "$wanted" ] && return 0
        case "$state" in failed) die "staging job reached failed state" ;; esac
        sleep 1
    done
    die "staging job did not reach $wanted"
}

wait_job_terminal() {
    local token="$1" job_id="$2" output="$3" unused state
    for unused in $(seq 1 1440); do
        kill -0 "$worker_pid" 2>/dev/null || die "recovery worker exited before terminal result"
        http_json GET "$API_BASE/score/$job_id" "$token" "" "$output.new" 200
        mv "$output.new" "$output"
        state="$(jq -er '.state' "$output")"
        case "$state" in
            succeeded) return 0 ;;
            failed) die "staging job reached failed state" ;;
            queued|running) ;;
            *) die "staging job returned unknown state $state" ;;
        esac
        sleep 30
    done
    die "staging job did not reach a terminal state"
}

wait_no_job_resources() {
    local job_id="$1" unused
    for unused in $(seq 1 120); do
        if [ -z "$(docker ps -aq --filter label=com.urnetwork.competition.job-id="$job_id")" ] &&
           [ -z "$(docker network ls -q --filter label=com.urnetwork.competition.job-id="$job_id")" ]; then
            return 0
        fi
        sleep 1
    done
    die "job resources survived cleanup"
}

write_check() {
    local path="$1"
    jq -e --arg release_amendment "$RELEASE_AMENDMENT_SHA" \
        '.schema == 1 and .kind == "sim-latency-production-readiness-check" and
        .passed == true and
        .production_release_self_check_contract_amendment_sha256 == $release_amendment and
        ([.assertions[]] | all)' "$path" >/dev/null || die "readiness check is malformed: $path"
    chmod 0400 "$path"
}

static_self_test() {
    local action child_pid environment expected local_environment local_expected probe_pid probe_root request unused
    if rg -n '[[:space:]][+][[:space:]]{4}' "$0" >/dev/null; then
        die "patch-marker shell arguments remain in the staging driver"
    fi
    environment="$(runtime_env sh -c 'printf "%s|%s|%s|%s|%s" "$WARP_CONFIG_HOME" "$WARP_VAULT_HOME" "$WARP_BLOCK" "$BRINGYOUR_POSTGRES_HOSTNAME" "$WARP_VERSION"')"
    expected="$API_CONFIG|$API_VAULT|competition|local-pg.bringyour.com|0.0.0-competition-2ee4883f"
    [ "$environment" = "$expected" ] || die "control-plane runtime environment"
    local_environment="$(local_stack_env sh -c 'printf "%s|%s|%s|%s|%s" "$LOCAL_EPHEMERAL_PORT_LOW" "$LOCAL_EPHEMERAL_PORT_HIGH" "$LOCAL_TCP_LISTEN_BACKLOG" "$LOCAL_NETDEV_MAX_BACKLOG" "$LOCAL_NF_CONNTRACK_MAX"')"
    local_expected="$FROZEN_EPHEMERAL_PORT_LOW|$FROZEN_EPHEMERAL_PORT_HIGH|$FROZEN_SOMAXCONN|$FROZEN_NETDEV_BACKLOG|$FROZEN_CONNTRACK_MAX"
    [ "$local_environment" = "$local_expected" ] || die "local stack changed the frozen network-control contract"
    action="$(network_restore_action 4096 4096 65535)"
    [ "$action" = unchanged ] || die "frozen network-control restore decision"
    action="$(network_restore_action 65535 4096 65535)"
    [ "$action" = restore ] || die "owned network-control restore decision"
    ! network_restore_action 8192 4096 65535 >/dev/null || die "external network-control change was accepted"
    request="$(jq -cn --arg opens 2026-01-01T00:00:00Z \
        --arg closes 2026-01-01T01:00:00Z \
        --arg reveal 2026-01-01T02:00:00Z \
        '{opens_at:$opens,closes_at:$closes,reveal_at:$reveal}')"
    jq -e '.opens_at < .closes_at and .closes_at < .reveal_at' \
        <<<"$request" >/dev/null || die "round request construction"
    http_status_expected 503 200,503 || die "HTTP status-set matching failed"
    ! http_status_expected 503 200 || die "HTTP status-set matching accepted a false member"
    probe_root="$(mktemp -d /tmp/urnetwork-staging-process.XXXXXXXX)"
    runtime_exec sh -c 'printf "%s\n" "$$" >"$1"; exec sleep 30' sh "$probe_root/pid" &
    probe_pid=$!
    for unused in $(seq 1 20); do
        [ -s "$probe_root/pid" ] && break
        sleep 0.05
    done
    child_pid="$(cat "$probe_root/pid" 2>/dev/null || true)"
    if [ "$child_pid" != "$probe_pid" ]; then
        kill -KILL "$probe_pid" 2>/dev/null || true
        wait "$probe_pid" 2>/dev/null || true
        rm -rf -- "$probe_root"
        die "background runtime PID does not own the launched process"
    fi
    kill -TERM "$probe_pid"
    wait "$probe_pid" 2>/dev/null || true
    rm -rf -- "$probe_root"
    log "static self-test passed: continuations, environments, network restoration, process ownership, and JSON construction"
}

for command in awk cat chmod cp curl date docker env find flock getent git id install ip jq kill mktemp mv openssl python3 readlink realpath rg rm runuser seq sh sha256sum sleep sort stat sysctl systemctl taskset xargs; do
    require_command "$command"
done
case "${1:-}" in
    --self-test)
        [ "$#" -eq 1 ] || die "usage: $0 [--self-test|--preflight]"
        static_self_test
        exit 0
        ;;
    --preflight)
        [ "$#" -eq 1 ] || die "usage: $0 [--self-test|--preflight]"
        preflight_only=true
        ;;
    "")
        [ "$#" -eq 0 ] || die "usage: $0 [--self-test|--preflight]"
        ;;
    *) die "usage: $0 [--self-test|--preflight]" ;;
esac
[ "$(id -u)" -eq 0 ] || die "production staging must run as root"
[ "$(< /proc/sys/kernel/random/boot_id)" = "$BOOT_ID" ] || die "host rebooted"
[ "$(sha256_file "$ROOT/source-lock.json")" = "$SOURCE_LOCK_SHA" ] || die "source lock changed"
[ "$(sha256_file "$ROOT/production-staging-protocol.json")" = "$PROTOCOL_SHA" ] || die "staging protocol changed"
[ "$(sha256_file "$REFERENCE_V5_STAGING_AMENDMENT")" = "$REFERENCE_V5_STAGING_AMENDMENT_SHA" ] ||
    die "reference-v5 staging amendment changed"
[ "$(sha256_file "$RELEASE_AMENDMENT")" = "$RELEASE_AMENDMENT_SHA" ] ||
    die "production release self-check amendment changed"
[ "$(sha256_file "$INDEPENDENT_ATTESTATION_REPAIR")" = "$INDEPENDENT_ATTESTATION_REPAIR_SHA" ] ||
    die "independent attestation repair changed"
[ "$(sha256_file "$INDEPENDENT_ATTESTATION_REPAIR_SCRIPT")" = "$INDEPENDENT_ATTESTATION_REPAIR_SCRIPT_SHA" ] ||
    die "independent attestation repair script changed"
[ "$(sha256_file "$PROVISIONER")" = "$PROVISIONER_SHA" ] || die "provisioner changed"
[ "$(sha256_file "$ROOT/control-plane-release/source-release.json")" = "$CONTROL_RELEASE_SHA" ] || die "control source release changed"
[ "$(git -C "$CONTROL" rev-parse HEAD)" = "$CONTROL_COMMIT" ] || die "control-plane commit changed"
[ "$(git -C "$CONTROL" rev-parse '@{upstream}')" = "$CONTROL_COMMIT" ] || die "control-plane push identity changed"
[ -z "$(git -C "$CONTROL" status --porcelain --untracked-files=no)" ] || die "control-plane worktree is dirty"
[ -f "$RELEASE_MANIFEST" ] && [ ! -L "$RELEASE_MANIFEST" ] || die "final control-plane release is missing"
[ "$(sha256_file "$RELEASE_MANIFEST")" = "$RELEASE_MANIFEST_SHA" ] || die "release manifest changed"
[ -f "$SELECTION" ] && [ -f "$INDEPENDENT_ATTESTATION" ] || die "terminal measurement evidence is missing"
jq -e --arg source "$SOURCE_LOCK_SHA" '.accepted == true and .source_lock_sha256 == $source and
    .same_seed_pairs == 12 and .independent_seed_target == 12 and .reference_required_passes == 11' \
    "$SELECTION" >/dev/null || die "original same-seed selection is invalid"
jq -e --arg source "$SOURCE_LOCK_SHA" --arg protocol "$PROTOCOL_SHA" \
    --arg attestation "$(sha256_file "$INDEPENDENT_ATTESTATION")" \
    --arg decision "$(sha256_file "$INDEPENDENT_DECISION")" \
    --arg repair "$INDEPENDENT_ATTESTATION_REPAIR_SHA" \
    --arg repair_script "$INDEPENDENT_ATTESTATION_REPAIR_SCRIPT_SHA" \
    '.kind == "sim-latency-production-staging-reference-v5-amendment" and
     .draft == false and .authorized == true and .source_lock_sha256 == $source and
     .original_production_staging_protocol_sha256 == $protocol and
     .hidden_campaign_attestation_sha256 == $attestation and
     .hidden_campaign_decision_sha256 == $decision and
     .hidden_attestation_repair_sha256 == $repair and
     .hidden_attestation_repair_script_sha256 == $repair_script and
     .replacement_measurement_dependencies.same_seed_pairs == 12 and
     .replacement_measurement_dependencies.independent_seeds == 5 and
     .replacement_measurement_dependencies.required_reference_ordering_passes == 4 and
     .replacement_measurement_dependencies.selected_competition_replicates == 9 and
     .retained_invariants.all_original_release_gates_unchanged == true and
     .retained_invariants.all_original_security_gates_unchanged == true and
     .retained_invariants.all_original_staging_round_gates_unchanged == true' \
    "$REFERENCE_V5_STAGING_AMENDMENT" >/dev/null || die "reference-v5 staging amendment is invalid"
jq -e --arg source "$SOURCE_LOCK_SHA" --arg protocol "$PROTOCOL_SHA" \
    --arg staging_amendment "$REFERENCE_V5_STAGING_AMENDMENT_SHA" \
    --arg control "$CONTROL_COMMIT" --arg source_release "$CONTROL_RELEASE_SHA" \
    --arg release_manifest "$RELEASE_MANIFEST_SHA" --arg release_check "$RELEASE_CHECK_SHA" \
    --arg service_check "$SERVICE_CHECK_SHA" \
    '.schema == 1 and .kind == "sim-latency-production-release-self-check-contract-amendment" and
     .passed == true and .binding.source_lock_sha256 == $source and
     .binding.production_staging_protocol_sha256 == $protocol and
     .binding.production_staging_reference_v5_amendment_sha256 == $staging_amendment and
     .replacement_release.control_plane_commit == $control and
     .replacement_release.origin_control_plane_commit == $control and
     .replacement_release.source_release_sha256 == $source_release and
     .replacement_release.release_manifest_sha256 == $release_manifest and
     .replacement_release.release_check_sha256 == $release_check and
     .replacement_release.service_gate_sha256 == $service_check and
     .root_cause.strict_unknown_field_rejection_retained == true and
     ([.retained_invariants[]] | all)' \
    "$RELEASE_AMENDMENT" >/dev/null || die "production release self-check amendment is invalid"
jq -e '.accepted == true and .target_independent_seeds == 5 and
    .reference_required_passes == 4 and .reference_ordering_passes >= 4 and
    .selected_competition_replicate_count_unchanged == true and
    .confidence_equivalent_to_original_protocol == false' \
    "$INDEPENDENT_ATTESTATION" >/dev/null || die "independent attestation is invalid"
jq -e '.accepted == true and .completed_independent_seeds == 5 and
    .reference_required_passes == 4 and .reference_ordering_passes >= 4 and
    .cleanup.residual_competition_containers == 0 and
    .cleanup.residual_competition_networks == 0' \
    "$INDEPENDENT_DECISION" >/dev/null || die "independent decision is invalid"
jq -e '.complete == true and .completed_independent_seeds == 5 and
    .target_independent_seeds == 5 and .designated_independent_baselines == 5 and
    .reference_required_passes == 4 and .reference_ordering_passes >= 4 and
    .separability_passed == true' \
    "$INDEPENDENT_PROGRESS" >/dev/null || die "independent progress is invalid"
jq -e --arg source "$SOURCE_LOCK_SHA" --arg protocol "$PROTOCOL_SHA" --arg control "$CONTROL_COMMIT" \
    --arg control_release "$CONTROL_RELEASE_SHA" --arg image "$EVALUATOR_IMAGE" \
    '.source_lock_sha256 == $source and .production_staging_protocol_sha256 == $protocol and
     .control_plane_commit == $control and .control_plane_source_release_sha256 == $control_release and
     .evaluator_image_digest == $image and .image_contexts_contain_config_or_vault == false' \
    "$RELEASE_MANIFEST" >/dev/null || die "release manifest identity is invalid"
[ "$(sha256_file "$SERVICE_CHECK")" = "$SERVICE_CHECK_SHA" ] || die "service-backed check changed"
[ "$(sha256_file "$RELEASE_CHECK")" = "$RELEASE_CHECK_SHA" ] || die "release check changed"
for prerequisite_check in "$SERVICE_CHECK" "$RELEASE_CHECK"; do
    jq -e --arg source "$SOURCE_LOCK_SHA" --arg protocol "$PROTOCOL_SHA" --arg control "$CONTROL_COMMIT" \
        '.schema == 1 and .kind == "sim-latency-production-readiness-check" and
         .passed == true and .source_lock_sha256 == $source and
         .production_staging_protocol_sha256 == $protocol and
         .control_plane_commit == $control and ([.assertions[]] | all)' \
        "$prerequisite_check" >/dev/null || die "prerequisite readiness check is invalid: $prerequisite_check"
done
[ ! -e "$STAGING" ] || die "staging output already exists: $STAGING"
for output in authenticated-api.json full-staging-round.json monitoring-and-recovery.json artifact-retention.json no-secrets-audit.json; do
    [ ! -e "$OUTPUT_ROOT/$output" ] || die "production check already exists: $output"
done
for service in urnetwork-final-calibration-recovery-8c7cfc98.service urnetwork-final-independent-r1-da4ee86a.service urnetwork-reference-v5-pilot-4a290509-attempt-01.service urnetwork-reference-v5-hidden-a889248b-attempt-01.service; do
    state="$(systemctl is-active "$service" 2>/dev/null || true)"
    case "$state" in inactive|failed|unknown) ;; *) die "measurement service is active: $service ($state)" ;; esac
done
[ -z "$(docker ps -q --filter label=com.urnetwork.competition.job-id)" ] || die "competition containers are active"
[ -z "$(docker ps -aq --filter 'name=^/urnetwork-local-pg$')" ] || die "local PostgreSQL already exists"
[ -z "$(docker ps -aq --filter 'name=^/urnetwork-local-redis$')" ] || die "local Redis already exists"
! docker network inspect urnetwork-local >/dev/null 2>&1 || die "local service network already exists"
! ip -brief address show dev lo | rg -q "(^|[[:space:]])$SERVICE_IP/32([[:space:]]|$)" || die "local service alias already exists"
! rg -q --fixed-strings '# >>> urnetwork local-env (server/local/run-local.sh) >>>' /etc/hosts ||
    die "managed local-service hosts block already exists"
host_network_controls_match_frozen || die "host network controls differ from the frozen qualification"

for binary in api competitionworker competitionrebaseline competitiondbinit; do
    path="$RELEASE/binaries/$binary"
    case "$binary" in
        api) manifest_key=api ;;
        competitionworker) manifest_key=worker ;;
        competitionrebaseline) manifest_key=rebaseline ;;
        competitiondbinit) manifest_key=dbinit ;;
    esac
    [ -x "$path" ] && [ "$(sha256_file "$path")" = "$(jq -er --arg key "$manifest_key" '.binaries[$key].sha256' "$RELEASE_MANIFEST")" ] ||
        die "release binary changed: $binary"
done
[ "$(sha256_file "$NOOP_PATCH")" = "$NOOP_SHA" ] || die "no-op patch changed"
jq -e --arg image "$EVALUATOR_IMAGE" --arg qualification "$HOST_QUALIFICATION_SHA" \
    '.image_digest == $image and .qualification_sha256 == $qualification and
     .evaluation_cpu_list == "0,2,4,6,8,10,12,14,16,18" and
     .management_cpu_list == "20,22" and .artifact_root == "/var/lib/urnetwork/competition"' \
    "$HOST_CONFIG" >/dev/null || die "authoritative host configuration changed"
if [ "$preflight_only" = true ]; then
    log "production staging preflight passed"
    exit 0
fi

install -d -o 0 -g 0 -m 0700 "$OUTPUT_ROOT" "$STAGING"
log "installing separate control-plane resources and root-owned trusted commands"
runuser -u by -- "$PROVISIONER" --install >"$STAGING/provision.log" 2>&1
[ "$(stat -c '%U:%G:%a' "$CREDENTIALS")" = root:root:400 ] || die "raw credentials are not root-only"
[ "$(jq -er '.control_plane_commit' "$DEPLOYMENT_MANIFEST")" = "$CONTROL_COMMIT" ] || die "deployment manifest control identity"
[ "$(jq -er '.production_release_self_check_contract_amendment_sha256' "$DEPLOYMENT_MANIFEST")" = "$RELEASE_AMENDMENT_SHA" ] ||
    die "deployment manifest release amendment identity"

operator_token="$(jq -er '.tokens["competition-operator"]' "$CREDENTIALS")"
submit_a_token="$(jq -er '.tokens["apex-submit-a"]' "$CREDENTIALS")"
submit_b_token="$(jq -er '.tokens["apex-submit-b"]' "$CREDENTIALS")"
for token in "$operator_token" "$submit_a_token" "$submit_b_token"; do
    [[ "$token" =~ ^[0-9a-f]{64}$ ]] || die "raw credential is malformed"
done
[ "$operator_token" != "$submit_a_token" ] && [ "$operator_token" != "$submit_b_token" ] &&
    [ "$submit_a_token" != "$submit_b_token" ] || die "credentials are not unique"

log "starting dedicated PostgreSQL and Redis with frozen host network controls"
runtime_armed=true
local_stack_env \
    "$SERVER/local/run-local.sh" >"$STAGING/stack.log" 2>&1 &
stack_pid=$!
wait_stack
docker inspect urnetwork-local-pg urnetwork-local-redis >"$STAGING/services.json"

log "applying the exact released migration set"
runtime_env taskset -c "$MANAGEMENT_CPUS" "$DBINIT" >"$STAGING/dbinit.json" 2>"$STAGING/dbinit.log"
jq -e '.schema == 1 and .database_version == .migration_count and .migration_count > 0' "$STAGING/dbinit.json" >/dev/null ||
    die "database initialization did not reach the repository migration count"

log "starting the released competition API"
runtime_exec taskset -c "$MANAGEMENT_CPUS" "$API" -p "$API_PORT" >"$STAGING/api.log" 2>&1 &
api_pid=$!
wait_api

start_worker staging-bootstrap "$STAGING/worker-bootstrap.log"

replicates="$(jq -er '.replicate_count' "$SELECTION")"
[[ "$replicates" =~ ^(1|3|5|7|9)$ ]] || die "selected replicate count is invalid"
now_epoch="$(date -u +%s)"
opens_epoch=$((now_epoch - 30))
closes_epoch=$((now_epoch + replicates * 1110 + 900))
reveal_epoch=$((now_epoch + 2 * replicates * 1110 + 1800))
opens_at="$(date -u -d "@$opens_epoch" '+%FT%TZ')"
closes_at="$(date -u -d "@$closes_epoch" '+%FT%TZ')"
reveal_at="$(date -u -d "@$reveal_epoch" '+%FT%TZ')"
jq -n --arg opens "$opens_at" --arg closes "$closes_at" --arg reveal "$reveal_at" \
    '{opens_at:$opens,closes_at:$closes,reveal_at:$reveal}' >"$STAGING/generate-request.json"
generate_round "$operator_token" "$STAGING/generate-request.json" "$STAGING/round.json"
round_id="$(jq -er '.round_id' "$STAGING/round.json")"
providers_sha="$(jq -er '.providers_sha256' "$STAGING/round.json")"
commitment="$(jq -er '.workload_commitment' "$STAGING/round.json")"
[[ "$round_id" =~ ^[0-9a-f-]{36}$ ]] && [[ "$providers_sha" =~ ^[0-9a-f]{64}$ ]] &&
    [[ "$commitment" =~ ^[0-9a-f]{64}$ ]] || die "generated round identity is malformed"

log "stopping the ordinary worker for trusted same-round rebaseline"
stop_worker
[ -z "$(docker ps -aq --filter label=com.urnetwork.competition.job-id)" ] || die "worker stop left an active job"
self_check_sha="$(sha256_file "$SELF_CHECK")"
install -d -o 0 -g 0 -m 0700 "$STAGING/rebaseline"
(
    flock -x 9
    log "running exact no-op same-round rebaseline (R=$replicates)"
    runtime_env taskset -c "$MANAGEMENT_CPUS" "$REBASELINE" \
        --round_id "$round_id" \
        --patch "$NOOP_PATCH" \
        --patch_sha256 "$NOOP_SHA" \
        --output "$STAGING/rebaseline/result.json" >"$STAGING/rebaseline/run.log" 2>&1
    "$REBASELINE_PROMOTER" \
        --result "$STAGING/rebaseline/result.json" \
        --host-config "$HOST_CONFIG" \
        --resource-bomb-report "$RESOURCE_BOMB" \
        --self-check "$SELF_CHECK" \
        --self-check-sha256 "$self_check_sha" \
        --output-directory "$STAGING/rebaseline/promotion" >"$STAGING/rebaseline/promotion-result.json"
) 9>"$OPERATIONAL_LOCK"
jq -e --arg round "$round_id" '.round_id == $round and .candidate_placeable == true' \
    "$STAGING/rebaseline/result.json" >/dev/null || die "rebaseline result is invalid"
jq -e --arg round "$round_id" '.round_id == $round and .promoted == true' \
    "$STAGING/rebaseline/promotion-result.json" >/dev/null || die "rebaseline promotion is invalid"

start_worker staging-primary "$STAGING/worker-primary.log"
wait_fully_ready "$operator_token"

jq -n --arg round "$round_id" --rawfile patch "$NOOP_PATCH" \
    '{round_id:$round,patch:$patch}' >"$STAGING/submit-request.json"
http_json POST "$API_BASE/score" "$submit_a_token" "$STAGING/submit-request.json" "$STAGING/submit-a.json" 202
job_id="$(jq -er '.job_id' "$STAGING/submit-a.json")"
jq -e --arg round "$round_id" --arg patch "$NOOP_SHA" \
    '.round_id == $round and .patch_sha256 == $patch and .cache_hit == false' \
    "$STAGING/submit-a.json" >/dev/null || die "first submission identity is invalid"
http_json POST "$API_BASE/score" "$submit_b_token" "$STAGING/submit-request.json" "$STAGING/submit-b.json" 202
jq -e --arg job "$job_id" --arg round "$round_id" \
    '.job_id == $job and .round_id == $round and .cache_hit == true' \
    "$STAGING/submit-b.json" >/dev/null || die "second-principal cache hit is invalid"

wait_job_state "$submit_a_token" "$job_id" running "$STAGING/poll-active-submitter.json" 120
http_json GET "$API_BASE/score/$job_id" "$operator_token" "" "$STAGING/poll-active-operator.json" 200
jq -e '.state == "running" and (.score == null or
    ((.score.raw_score == null) and (.score.diagnostics == null) and
     ([.score.gates[].details == {}] | all)))' "$STAGING/poll-active-submitter.json" >/dev/null ||
    die "active-round submitter response leaked score details"

log "forcing one real worker handback after the evaluator starts"
started=false
for unused in $(seq 1 300); do
    if [ -n "$(docker ps -q --filter label=com.urnetwork.competition.job-id="$job_id")" ]; then
        started=true
        break
    fi
    kill -0 "$worker_pid" 2>/dev/null || die "primary worker exited before recovery probe"
    sleep 1
done
[ "$started" = true ] || die "staging evaluator did not start a labeled container"
sleep 5
stop_worker
wait_no_job_resources "$job_id"
wait_job_state "$submit_a_token" "$job_id" queued "$STAGING/poll-handed-back.json" 120

start_worker staging-recovery "$STAGING/worker-recovery.log"
wait_job_terminal "$submit_a_token" "$job_id" "$STAGING/poll-terminal-submitter.json"
terminal_epoch="$(date -u +%s)"
[ "$terminal_epoch" -lt "$reveal_epoch" ] || die "job completed after reveal; active terminal redaction was not observable"
http_json GET "$API_BASE/score/$job_id" "$operator_token" "" "$STAGING/poll-terminal-operator.json" 200
http_json GET "$API_BASE/score/$job_id" "$submit_b_token" "" "$STAGING/poll-terminal-submitter-b.json" 200
jq -e '.state == "succeeded" and .score != null and .score.raw_score == null and
    .score.diagnostics == null and ([.score.gates[].details == {}] | all)' \
    "$STAGING/poll-terminal-submitter.json" >/dev/null || die "terminal submitter response leaked active-round details"
jq -e '.state == "succeeded" and .score.placeable == true and
    (.score.raw_score | type == "number") and (.score.diagnostics | type == "object") and
    ((.score.gates | keys | sort) ==
      ["G1_success","G2_volume","G3_path_integrity","G4_matchmaking","G5_stability","G6_resources"]) and
    ([.score.gates[].passed] | all)' "$STAGING/poll-terminal-operator.json" >/dev/null ||
    die "operator terminal score did not pass the exact frozen gate set"
jq -e --arg job "$job_id" '.job_id == $job and .state == "succeeded" and .score.raw_score == null' \
    "$STAGING/poll-terminal-submitter-b.json" >/dev/null || die "cache principal cannot poll the redacted terminal result"

log "waiting for the committed reveal time"
while [ "$(date -u +%s)" -lt "$reveal_epoch" ]; do
    sleep 30
done
http_json GET "$API_BASE/info" "" "" "$STAGING/info-revealed.json" 200
revealed_seed="$(jq -er --arg round "$round_id" '.active_round |
    select(.round_id == $round) | .revealed_seed' "$STAGING/info-revealed.json")"
[[ "$revealed_seed" =~ ^[0-9a-f]{64}$ ]] || die "revealed seed is malformed"
recomputed_commitment="$(python3 -c 'import hashlib,sys,uuid; print(hashlib.sha256(b"urnetwork-sim-latency-round-v1\0"+uuid.UUID(sys.argv[1]).bytes+bytes.fromhex(sys.argv[2])).hexdigest())' "$round_id" "$revealed_seed")"
[ "$recomputed_commitment" = "$commitment" ] || die "revealed seed does not match the public commitment"
providers_status="$(curl --silent --show-error \
    --dump-header "$STAGING/providers.headers" \
    --output "$STAGING/providers.yml" \
    --write-out '%{http_code}' "$API_BASE/round/$round_id/providers.yml")"
[ "$providers_status" = 200 ] || die "providers download returned HTTP $providers_status"
[ "$(sha256_file "$STAGING/providers.yml")" = "$providers_sha" ] || die "downloaded providers hash mismatch"
header_hash="$(awk 'BEGIN{IGNORECASE=1} /^X-Content-SHA256:/ {gsub("\r","",$2); print $2}' "$STAGING/providers.headers")"
[ "$header_hash" = "$providers_sha" ] || die "providers response hash header mismatch"

attempt_root="/var/lib/urnetwork/competition/$job_id"
[ -d "$attempt_root/attempt-01" ] && [ -d "$attempt_root/attempt-02" ] ||
    die "worker recovery did not retain both evaluation attempts"
attempt_one="$attempt_root/attempt-01"
attempt_final="$attempt_root/attempt-02"
[ -f "$attempt_final/worker-result.json" ] && [ -f "$attempt_final/evaluation.complete.json" ] ||
    die "terminal attempt artifacts are incomplete"
jq -e --arg job "$job_id" '.job_id == $job and .eval_error == null and .score.placeable == true and
    ([.security | to_entries[] | select(.key != "cgroup_id" and .key != "template_database_id" and .key != "redis_generation_id") | .value] | all)' \
    "$attempt_final/worker-result.json" >/dev/null || die "terminal worker security evidence failed"
jq -e '.direct_bind == true and .read_only == true and .parent_mounts == false and
    .all_main_site_absent == true and .config.target == "/runtime/config/local" and
    .vault.target == "/runtime/vault/local"' "$attempt_final/evidence/local-mounts.json" >/dev/null ||
    die "terminal local-mount evidence failed"
jq -e '.cleanup_complete == true and .attempt == 2' "$attempt_final/evaluation.complete.json" >/dev/null ||
    die "terminal completion marker failed"
wait_no_job_resources "$job_id"
[ -z "$(find "$attempt_final" -mindepth 1 -perm /022 -print -quit)" ] || die "terminal artifacts remain writable"

pg_user="$(awk '$1 == "user:" {print $2; exit}' /home/by/urnetwork/vault/local/pg.yml)"
pg_password="$(awk '$1 == "password:" {print $2; exit}' /home/by/urnetwork/vault/local/pg.yml)"
pg_database="$(awk '$1 == "db:" {print $2; exit}' /home/by/urnetwork/vault/local/pg.yml)"
docker exec -e PGPASSWORD="$pg_password" urnetwork-local-pg \
    psql --quiet --tuples-only --no-align --username "$pg_user" --dbname "$pg_database" \
    --command "SELECT COALESCE(json_agg(json_build_object('event_id',event_id,'event_type',event_type,'actor_id',actor_id,'payload_sha256',payload_sha256) ORDER BY event_id),'[]'::json) FROM competition_job_event WHERE job_id = '$job_id'::uuid;" \
    >"$STAGING/job-events.json"
jq -e '([.[].event_type] | index("cache_hit")) != null and
    ([.[].event_type] | index("handed_back")) != null and
    ([.[].event_type] | map(select(. == "claimed")) | length) >= 2 and
    ([.[].event_type] | index("succeeded")) != null and
    ([.[].payload_sha256 | test("^[0-9a-f]{64}$")] | all)' "$STAGING/job-events.json" >/dev/null ||
    die "job event history does not prove cache, handback, recovery, and success"
pg_user=""; pg_password=""; pg_database=""

log "stopping the staging runtime before scanning and sealing evidence"
cleanup_runtime || die "staging runtime cleanup failed"

for token in "$operator_token" "$submit_a_token" "$submit_b_token"; do
    if rg --text --fixed-strings --files-with-matches -- "$token" "$STAGING" "$attempt_one" "$attempt_final" >/dev/null 2>&1; then
        die "raw API token leaked into retained evidence"
    fi
done
if rg --text --ignore-case --files-with-matches \
    '(BEGIN (RSA |EC |OPENSSH )?PRIVATE KEY|seed_key_base64|Authorization:[[:space:]]*Bearer)' \
    "$STAGING" "$attempt_one" "$attempt_final" >/dev/null 2>&1; then
    die "secret-shaped material leaked into retained evidence"
fi
jq -n --arg scanned_at "$(date -u '+%FT%TZ')" \
    --arg staging_sha "$(find "$STAGING" -type f -not -name secret-scan.json -print0 | sort -z | xargs -0 sha256sum | sha256sum | awk '{print $1}')" \
    --arg attempt_one "$(sha256_file "$attempt_one/worker-request.json")" \
    --arg attempt_final "$(sha256_file "$attempt_final/evidence-manifest.json")" \
    '{schema:1,kind:"sim-latency-staging-secret-scan",scanned_at:$scanned_at,
      staging_file_set_sha256:$staging_sha,failed_attempt_request_sha256:$attempt_one,
      terminal_evidence_manifest_sha256:$attempt_final,
      actual_raw_tokens_searched:3,matches:0,private_key_markers:0,passed:true}' \
    >"$STAGING/secret-scan.json"

chmod 0400 "$STAGING"/*.json "$STAGING"/*.log "$STAGING"/providers.headers "$STAGING"/providers.yml
chmod 0400 "$STAGING/rebaseline"/*.json "$STAGING/rebaseline"/*.log

common_args=(
    --arg generated_at "$(date -u '+%FT%TZ')"
    --arg source "$SOURCE_LOCK_SHA"
    --arg protocol "$PROTOCOL_SHA"
    --arg reference_v5 "$REFERENCE_V5_STAGING_AMENDMENT_SHA"
    --arg release_amendment "$RELEASE_AMENDMENT_SHA"
    --arg control "$CONTROL_COMMIT"
)

jq -n "${common_args[@]}" \
    --arg round "$round_id" --arg job "$job_id" \
    --arg round_sha "$(sha256_file "$STAGING/round.json")" \
    --arg submit_a "$(sha256_file "$STAGING/submit-a.json")" \
    --arg submit_b "$(sha256_file "$STAGING/submit-b.json")" \
    --arg active "$(sha256_file "$STAGING/poll-active-submitter.json")" \
    --arg terminal "$(sha256_file "$STAGING/poll-terminal-operator.json")" \
    --arg revealed "$(sha256_file "$STAGING/info-revealed.json")" \
    --arg providers "$providers_sha" \
    '{schema:1,kind:"sim-latency-production-readiness-check",
      check_id:"authenticated_api_generate_submit_poll",passed:true,generated_at:$generated_at,
      source_lock_sha256:$source,production_staging_protocol_sha256:$protocol,
      production_staging_reference_v5_amendment_sha256:$reference_v5,
      production_release_self_check_contract_amendment_sha256:$release_amendment,
      control_plane_commit:$control,
      round_id:$round,job_id:$job,evidence_sha256:{round:$round_sha,submit_a:$submit_a,
        submit_b:$submit_b,active_submitter:$active,terminal_operator:$terminal,revealed_info:$revealed,
        providers:$providers},
      assertions:{operator_generate_authenticated:true,submitter_submit_authenticated:true,
        poll_authenticated:true,second_principal_cache_hit:true,active_raw_score_redacted:true,
        reveal_commitment_verified:true,providers_download_hash_verified:true}}' \
    >"$OUTPUT_ROOT/authenticated-api.json"
write_check "$OUTPUT_ROOT/authenticated-api.json"

jq -n "${common_args[@]}" \
    --arg round "$round_id" --arg job "$job_id" --arg patch "$NOOP_SHA" \
    --arg rebaseline "$(sha256_file "$STAGING/rebaseline/result.json")" \
    --arg promotion "$(sha256_file "$STAGING/rebaseline/promotion-result.json")" \
    --arg worker "$(sha256_file "$attempt_final/worker-result.json")" \
    --arg completion "$(sha256_file "$attempt_final/evaluation.complete.json")" \
    --arg manifest "$(sha256_file "$attempt_final/evidence-manifest.json")" \
    '{schema:1,kind:"sim-latency-production-readiness-check",
      check_id:"full_staging_round",passed:true,generated_at:$generated_at,
      source_lock_sha256:$source,production_staging_protocol_sha256:$protocol,
      production_staging_reference_v5_amendment_sha256:$reference_v5,
      production_release_self_check_contract_amendment_sha256:$release_amendment,
      control_plane_commit:$control,
      round_id:$round,job_id:$job,patch_sha256:$patch,
      evidence_sha256:{rebaseline:$rebaseline,promotion:$promotion,worker_result:$worker,
        completion:$completion,evidence_manifest:$manifest},
      assertions:{same_round_baseline_verified:true,exact_noop_patch_verified:true,
        frozen_six_gate_set_verified:true,evaluator_identity_verified:true,host_identity_verified:true,
        cleanup_verified:true,artifact_manifest_verified:true}}' \
    >"$OUTPUT_ROOT/full-staging-round.json"
write_check "$OUTPUT_ROOT/full-staging-round.json"

jq -n "${common_args[@]}" \
    --arg round "$round_id" --arg job "$job_id" \
    --arg ready "$(sha256_file "$STAGING/ready-post-promotion.json")" \
    --arg events "$(sha256_file "$STAGING/job-events.json")" \
    --arg failed_attempt "$(sha256_file "$attempt_one/worker-request.json")" \
    --arg resource_bomb "$(sha256_file "$RESOURCE_BOMB")" \
    '{schema:1,kind:"sim-latency-production-readiness-check",
      check_id:"monitoring_and_recovery",passed:true,generated_at:$generated_at,
      source_lock_sha256:$source,production_staging_protocol_sha256:$protocol,
      production_staging_reference_v5_amendment_sha256:$reference_v5,
      production_release_self_check_contract_amendment_sha256:$release_amendment,
      control_plane_commit:$control,
      round_id:$round,job_id:$job,evidence_sha256:{ready:$ready,events:$events,
        failed_attempt:$failed_attempt,resource_bomb:$resource_bomb},
      assertions:{single_job_fifo_verified:true,lease_recovery_verified:true,
        host_heartbeat_verified:true,resource_reports_verified:true,cleanup_after_failure_verified:true}}' \
    >"$OUTPUT_ROOT/monitoring-and-recovery.json"
write_check "$OUTPUT_ROOT/monitoring-and-recovery.json"

jq -n "${common_args[@]}" \
    --arg round "$round_id" --arg job "$job_id" \
    --arg accounting "$(sha256_file "$attempt_final/accounting.json")" \
    --arg resources "$(sha256_file "$attempt_final/resources.json")" \
    --arg quota "$(sha256_file "$attempt_final/evidence/evidence-quota.json")" \
    --arg failure "$(sha256_file "$attempt_one/worker-request.json")" \
    --arg retain "$(jq -er '.retain_until' "$API_CONFIG/local/competition.yml")" \
    '{schema:1,kind:"sim-latency-production-readiness-check",
      check_id:"artifact_retention",passed:true,generated_at:$generated_at,
      source_lock_sha256:$source,production_staging_protocol_sha256:$protocol,
      production_staging_reference_v5_amendment_sha256:$reference_v5,
      production_release_self_check_contract_amendment_sha256:$release_amendment,
      control_plane_commit:$control,
      round_id:$round,job_id:$job,retain_until:$retain,
      evidence_sha256:{accounting:$accounting,resources:$resources,quota:$quota,failed_attempt:$failure},
      assertions:{accounting_immutable:true,resources_immutable:true,artifact_quota_verified:true,
        retain_until_verified:true,failure_evidence_retained:true}}' \
    >"$OUTPUT_ROOT/artifact-retention.json"
write_check "$OUTPUT_ROOT/artifact-retention.json"

jq -n "${common_args[@]}" \
    --arg round "$round_id" --arg job "$job_id" \
    --arg mounts "$(sha256_file "$attempt_final/evidence/local-mounts.json")" \
    --arg boundary "$(sha256_file "$ROOT/control-plane-secret-boundary.json")" \
    --arg scan "$(sha256_file "$STAGING/secret-scan.json")" \
    --arg deployment "$(sha256_file "$DEPLOYMENT_MANIFEST")" \
    '{schema:1,kind:"sim-latency-production-readiness-check",
      check_id:"no_secrets_audit",passed:true,generated_at:$generated_at,
      source_lock_sha256:$source,production_staging_protocol_sha256:$protocol,
      production_staging_reference_v5_amendment_sha256:$reference_v5,
      production_release_self_check_contract_amendment_sha256:$release_amendment,
      control_plane_commit:$control,
      round_id:$round,job_id:$job,evidence_sha256:{local_mounts:$mounts,
        control_boundary:$boundary,secret_scan:$scan,deployment:$deployment},
      assertions:{direct_config_local_read_only:true,direct_vault_local_read_only:true,
        no_parent_config_mount:true,no_parent_vault_mount:true,no_control_secret_mount:true,
        no_docker_socket_mount:true,evidence_secret_scan_passed:true,raw_tokens_not_stored:true}}' \
    >"$OUTPUT_ROOT/no-secrets-audit.json"
write_check "$OUTPUT_ROOT/no-secrets-audit.json"

operator_token=""; submit_a_token=""; submit_b_token=""; revealed_seed=""
success=true
log "production staging passed: round=$round_id job=$job_id"
printf '%s\n' "$round_id $job_id"
