#!/usr/bin/env bash
# Root-provisioned evaluator host attestation. This command is invoked with
# --json by competitionworker under a sanitized environment. It always emits a
# schema-1 report; a failed check also makes the process exit nonzero so the
# control plane can retain the negative heartbeat without admitting work.

set -uo pipefail
umask 077

readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
readonly HOST_CONFIG=/etc/urnetwork/competition-host.json
readonly DOCKER_DAEMON_CONFIG=/etc/docker/daemon.json
readonly RESOURCE_BOUNDARY="$SCRIPT_DIR/container/resource-boundary.sh"
readonly HASH_LOCAL_MOUNT="$SCRIPT_DIR/container/hash-local-mount.sh"
readonly DOCKER_ID_MAP="$SCRIPT_DIR/container/docker-id-map.sh"
readonly IRQ_CONTROL="$SCRIPT_DIR/authoritative-host-irqs.sh"

if [ "${1:-}" != --json ] || [ "$#" -ne 1 ]; then
    printf 'usage: competition-host-self-check --json\n' >&2
    exit 2
fi

command -v jq >/dev/null 2>&1 || exit 2
command -v sha256sum >/dev/null 2>&1 || exit 2
command -v docker >/dev/null 2>&1 || exit 2
command -v sudo >/dev/null 2>&1 || exit 2

config_secure=false
if [ -f "$HOST_CONFIG" ]; then
    config_owner="$(stat -c %u "$HOST_CONFIG" 2>/dev/null || printf 1)"
    config_mode="$(stat -c %a "$HOST_CONFIG" 2>/dev/null || printf 777)"
    if [ "$config_owner" = 0 ] && [ $((8#$config_mode & 0022)) -eq 0 ] && jq -e '.schema == 1' "$HOST_CONFIG" >/dev/null 2>&1; then
        config_secure=true
    fi
fi

cfg() {
    if [ "$config_secure" = true ]; then
        jq -er "$1 // empty" "$HOST_CONFIG" 2>/dev/null || true
    fi
}

host_id="$(cfg '.host_id')"
hardware_id="$(cfg '.hardware_id')"
image_digest="$(cfg '.image_digest')"
expected_qualification="$(cfg '.qualification_sha256')"
expected_kernel="$(cfg '.kernel_release')"
expected_microcode="$(cfg '.microcode_revision')"
expected_cpu_list="$(cfg '.cpu_list')"
expected_evaluation_cpu_list="$(cfg '.evaluation_cpu_list')"
expected_management_cpu_list="$(cfg '.management_cpu_list')"
expected_active_memory_limit_bytes="$(cfg '.active_memory_limit_bytes')"
expected_management_memory_reserve_bytes="$(cfg '.management_memory_reserve_bytes')"
config_local_directory="$(cfg '.config_local_directory')"
vault_local_directory="$(cfg '.vault_local_directory')"
expected_config_local_sha256="$(cfg '.config_local_sha256')"
expected_vault_local_sha256="$(cfg '.vault_local_sha256')"
expected_numa_list="$(cfg '.numa_list')"
expected_governor="$(cfg '.governor')"
expected_turbo="$(cfg '.turbo_state')"
expected_irq_policy_sha="$(cfg '.irq_policy_sha256')"
expected_docker_daemon_config_sha256="$(cfg '.docker_daemon_config_sha256')"
expected_cgroup="$(cfg '.job_cgroup')"
expected_artifact_quota_bytes="$(cfg '.artifact_quota_bytes')"
expected_postgres_image="$(cfg '.postgres_image')"
expected_redis_image="$(cfg '.redis_image')"
artifact_root="$(cfg '.artifact_root')"
offline_cache="$(cfg '.offline_build_cache')"
template_marker="$(cfg '.template_database_marker')"
template_marker_sha="$(cfg '.template_database_marker_sha256')"
redis_marker="$(cfg '.redis_reset_marker')"
redis_marker_sha="$(cfg '.redis_reset_marker_sha256')"
cleanup_marker="$(cfg '.cleanup_marker')"
cleanup_marker_sha="$(cfg '.cleanup_marker_sha256')"
immutable_marker="$(cfg '.immutable_reports_marker')"
immutable_marker_sha="$(cfg '.immutable_reports_marker_sha256')"
rebaseline_manifest="$(cfg '.rebaseline_manifest')"
rebaseline_manifest_sha="$(cfg '.rebaseline_manifest_sha256')"

[ -n "$host_id" ] || host_id="$(hostname 2>/dev/null || true)"
kernel_release="$(uname -r 2>/dev/null || true)"
microcode_revision="$(awk -F: '$1 ~ /^microcode/ {gsub(/[[:space:]]/, "", $2); print $2}' /proc/cpuinfo 2>/dev/null | sort -u | paste -sd, -)"
host_cpu_list="$(lscpu -p=CPU 2>/dev/null | awk -F, '!/^#/ {print $1}' | paste -sd, -)"
logical_cpu_count="$(lscpu -p=CPU 2>/dev/null | awk -F, '!/^#/ {count++} END {print count+0}')"
worker_cpu_list="$(awk '/^Cpus_allowed_list:/ {print $2}' /proc/self/status 2>/dev/null)"
numa_allowed="$(awk '/^Mems_allowed_list:/ {print $2}' /proc/self/status 2>/dev/null)"
threads_per_core="$(lscpu -p=CPU,CORE 2>/dev/null | awk -F, '!/^#/ {n[$2]++} END {m=0; for (k in n) if (m<n[k]) m=n[k]; print m+0}')"
smt_control="$(tr -d '\n' </sys/devices/system/cpu/smt/control 2>/dev/null || true)"
governors="$(for path in /sys/devices/system/cpu/cpu*/cpufreq/scaling_governor; do [ -r "$path" ] && tr -d '\n' <"$path" && printf '\n'; done | sort -u | paste -sd, -)"

turbo_state=unknown
if [ -r /sys/devices/system/cpu/intel_pstate/no_turbo ]; then
    if [ "$(tr -d '\n' </sys/devices/system/cpu/intel_pstate/no_turbo)" = 1 ]; then turbo_state=disabled; else turbo_state=enabled; fi
elif [ -r /sys/devices/system/cpu/cpufreq/boost ]; then
    if [ "$(tr -d '\n' </sys/devices/system/cpu/cpufreq/boost)" = 0 ]; then turbo_state=disabled; else turbo_state=enabled; fi
fi

irq_report=""
irq_affinity_sha256=""
irq_policy_sha256=""
irq_live_passed=false
if [ -x "$IRQ_CONTROL" ]; then
    irq_report="$($IRQ_CONTROL --check 2>/dev/null || true)"
    if jq -e '
        .schema == 1 and .kind == "sim-latency-authoritative-host-irqs" and
        (.irq_affinity_sha256 | test("^[0-9a-f]{64}$")) and
        (.irq_policy_sha256 | test("^[0-9a-f]{64}$")) and
        (.passed | type == "boolean")
    ' <<<"$irq_report" >/dev/null 2>&1; then
        irq_affinity_sha256="$(jq -er '.irq_affinity_sha256' <<<"$irq_report")"
        irq_policy_sha256="$(jq -er '.irq_policy_sha256' <<<"$irq_report")"
        irq_live_passed="$(jq -er '.passed' <<<"$irq_report")"
    fi
fi

os_identity="$(. /etc/os-release 2>/dev/null; printf '%s-%s' "${ID:-unknown}" "${VERSION_ID:-unknown}")"
docker_server_version="$(sudo -n docker version --format '{{.Server.Version}}' 2>/dev/null || true)"
docker_cgroup_version="$(sudo -n docker info --format '{{.CgroupVersion}}' 2>/dev/null || true)"
docker_security_options="$(sudo -n docker info --format '{{json .SecurityOptions}}' 2>/dev/null | jq -cS . 2>/dev/null || true)"
docker_daemon_config_secure=false
docker_daemon_config_sha256=""
if [ -f "$DOCKER_DAEMON_CONFIG" ] && [ ! -L "$DOCKER_DAEMON_CONFIG" ] &&
   [ "$(stat -c %u "$DOCKER_DAEMON_CONFIG" 2>/dev/null)" = 0 ] &&
   [ $((8#$(stat -c %a "$DOCKER_DAEMON_CONFIG" 2>/dev/null) & 0022)) -eq 0 ] &&
   jq -e '
       (."userns-remap" | type == "string" and length > 0) and
       ."no-new-privileges" == true and ."userland-proxy" == false and
       ."log-driver" == "local" and ."log-opts"."max-size" == "16m" and
       ."log-opts"."max-file" == "2" and ."log-opts".compress == "true" and
       ."shutdown-timeout" == 45 and .ipv6 == false' \
       "$DOCKER_DAEMON_CONFIG" >/dev/null 2>&1; then
    docker_daemon_config_sha256="$(sha256sum "$DOCKER_DAEMON_CONFIG" | awk '{print $1}')"
    docker_daemon_config_secure=true
fi
docker_id_map_json=""
docker_uid_map_sha256=""
docker_gid_map_sha256=""
docker_id_map_remapped=false
if [ -x "$DOCKER_ID_MAP" ] && [[ "$image_digest" =~ ^sha256:[0-9a-f]{64}$ ]]; then
    docker_id_map_json="$($DOCKER_ID_MAP --image "$image_digest" --uid 65532 --gid 65532 2>/dev/null || true)"
    if jq -e --arg image_id "$image_digest" \
        '.schema == 1 and .kind == "sim-latency-docker-id-map" and
         .image_id == $image_id and .container_uid == 65532 and .container_gid == 65532 and
         (.uid_map_sha256 | test("^[0-9a-f]{64}$")) and
         (.gid_map_sha256 | test("^[0-9a-f]{64}$")) and (.remapped | type == "boolean")' \
        <<<"$docker_id_map_json" >/dev/null 2>&1; then
        docker_uid_map_sha256="$(jq -er '.uid_map_sha256' <<<"$docker_id_map_json")"
        docker_gid_map_sha256="$(jq -er '.gid_map_sha256' <<<"$docker_id_map_json")"
        docker_id_map_remapped="$(jq -er '.remapped' <<<"$docker_id_map_json")"
    fi
fi
postgres_image_id="$(sudo -n docker image inspect --format '{{.Id}}' "$expected_postgres_image" 2>/dev/null || true)"
redis_image_id="$(sudo -n docker image inspect --format '{{.Id}}' "$expected_redis_image" 2>/dev/null || true)"
somaxconn="$(cat /proc/sys/net/core/somaxconn 2>/dev/null || true)"
port_range="$(tr '\t' '-' </proc/sys/net/ipv4/ip_local_port_range 2>/dev/null || true)"
resource_boundary_json=""
if [ -x "$RESOURCE_BOUNDARY" ]; then
    resource_boundary_json="$($RESOURCE_BOUNDARY 2>/dev/null || true)"
fi
evaluation_cpu_list="$(jq -r '.evaluation_cpuset // empty' <<<"$resource_boundary_json" 2>/dev/null || true)"
management_cpu_list="$(jq -r '.management_cpuset // empty' <<<"$resource_boundary_json" 2>/dev/null || true)"
active_memory_limit_bytes="$(jq -r '.active_memory_limit_bytes // 0' <<<"$resource_boundary_json" 2>/dev/null || printf 0)"
management_memory_reserve_bytes="$(jq -r '.minimum_management_memory_reserve_bytes // 0' <<<"$resource_boundary_json" 2>/dev/null || printf 0)"
capacity_reserve_bytes="$(jq -r '.capacity_reserve_bytes // 0' <<<"$resource_boundary_json" 2>/dev/null || printf 0)"
runner_memory_limit_bytes="$(jq -r '.runner_memory_limit_bytes // 0' <<<"$resource_boundary_json" 2>/dev/null || printf 0)"
config_local_sha256=""
vault_local_sha256=""
if [ -x "$HASH_LOCAL_MOUNT" ]; then
    config_local_sha256="$($HASH_LOCAL_MOUNT "$config_local_directory" 2>/dev/null || true)"
    vault_local_sha256="$($HASH_LOCAL_MOUNT "$vault_local_directory" 2>/dev/null || true)"
fi

facts="$(jq -cnS \
    --arg hardware_id "$hardware_id" \
    --arg image_digest "$image_digest" \
    --arg os "$os_identity" \
    --arg kernel "$kernel_release" \
    --arg microcode "$microcode_revision" \
    --arg cpu_list "$host_cpu_list" \
    --arg worker_cpu_list "$worker_cpu_list" \
    --arg evaluation_cpu_list "$evaluation_cpu_list" \
    --arg management_cpu_list "$management_cpu_list" \
    --arg numa_list "$numa_allowed" \
    --arg threads_per_core "$threads_per_core" \
    --arg smt "$smt_control" \
    --arg governor "$governors" \
    --arg turbo "$turbo_state" \
    --arg irq_policy_sha256 "$irq_policy_sha256" \
    --arg job_cgroup "$expected_cgroup" \
    --arg docker_server_version "$docker_server_version" \
    --arg docker_cgroup_version "$docker_cgroup_version" \
    --arg docker_security_options "$docker_security_options" \
    --arg docker_daemon_config_sha256 "$docker_daemon_config_sha256" \
    --arg docker_uid_map_sha256 "$docker_uid_map_sha256" \
    --arg docker_gid_map_sha256 "$docker_gid_map_sha256" \
    --arg postgres_image "$expected_postgres_image" \
    --arg postgres_image_id "$postgres_image_id" \
    --arg redis_image "$expected_redis_image" \
    --arg redis_image_id "$redis_image_id" \
    --arg artifact_quota_bytes "$expected_artifact_quota_bytes" \
    --arg somaxconn "$somaxconn" \
    --arg port_range "$port_range" \
    --arg active_memory_limit_bytes "$active_memory_limit_bytes" \
    --arg management_memory_reserve_bytes "$management_memory_reserve_bytes" \
    --arg config_local_sha256 "$config_local_sha256" \
    --arg vault_local_sha256 "$vault_local_sha256" \
    '{hardware_id:$hardware_id,image_digest:$image_digest,os:$os,kernel:$kernel,
      microcode:$microcode,cpu_list:$cpu_list,worker_cpu_list:$worker_cpu_list,
      evaluation_cpu_list:$evaluation_cpu_list,
      management_cpu_list:$management_cpu_list,numa_list:$numa_list,
      threads_per_core:$threads_per_core,smt:$smt,governor:$governor,
      turbo:$turbo,irq_policy_sha256:$irq_policy_sha256,
      job_cgroup:$job_cgroup,docker_server_version:$docker_server_version,
      docker_cgroup_version:$docker_cgroup_version,
      docker_security_options:$docker_security_options,
      docker_daemon_config_sha256:$docker_daemon_config_sha256,
      docker_uid_map_sha256:$docker_uid_map_sha256,
      docker_gid_map_sha256:$docker_gid_map_sha256,
      postgres_image:$postgres_image,postgres_image_id:$postgres_image_id,
      redis_image:$redis_image,redis_image_id:$redis_image_id,
      artifact_quota_bytes:$artifact_quota_bytes,
      somaxconn:$somaxconn,port_range:$port_range,
      active_memory_limit_bytes:$active_memory_limit_bytes,
      management_memory_reserve_bytes:$management_memory_reserve_bytes,
      config_local_sha256:$config_local_sha256,
      vault_local_sha256:$vault_local_sha256}')"
qualification_sha256="$(printf '%s' "$facts" | sha256sum | awk '{print $1}')"

qualification_match=false
[ "$config_secure" = true ] && [ "$qualification_sha256" = "$expected_qualification" ] && qualification_match=true
cpu_count_exact=false
[ "$logical_cpu_count" = 12 ] && [ "$host_cpu_list" = "$expected_cpu_list" ] && cpu_count_exact=true
worker_affinity_pinned=false
[ -n "$expected_management_cpu_list" ] && [ "$worker_cpu_list" = "$expected_management_cpu_list" ] &&
    worker_affinity_pinned=true
smt_disabled=false
[ "$threads_per_core" = 1 ] && { [ "$smt_control" = off ] || [ "$smt_control" = forceoff ] || [ "$smt_control" = notsupported ]; } && smt_disabled=true
governor_pinned=false
[ -n "$expected_governor" ] && [ "$governors" = "$expected_governor" ] && governor_pinned=true
turbo_pinned=false
[ -n "$expected_turbo" ] && [ "$turbo_state" = "$expected_turbo" ] && turbo_pinned=true
numa_pinned=false
[ -n "$expected_numa_list" ] && [ "$numa_allowed" = "$expected_numa_list" ] && numa_pinned=true
irq_pinned=false
[ "$irq_live_passed" = true ] && [ -n "$expected_irq_policy_sha" ] &&
    [ "$irq_policy_sha256" = "$expected_irq_policy_sha" ] && irq_pinned=true
kernel_pinned=false
[ -n "$expected_kernel" ] && [ "$kernel_release" = "$expected_kernel" ] && [ "$microcode_revision" = "$expected_microcode" ] && kernel_pinned=true
management_cpu_reserved=false
    [ -n "$expected_evaluation_cpu_list" ] && [ "$evaluation_cpu_list" = "$expected_evaluation_cpu_list" ] &&
    [ -n "$expected_management_cpu_list" ] && [ "$management_cpu_list" = "$expected_management_cpu_list" ] &&
    [ "$worker_affinity_pinned" = true ] &&
    jq -e '.disjoint_cpu_sets == true and .evaluation_physical_core_count == 10 and
        .management_physical_core_count == 2' <<<"$resource_boundary_json" >/dev/null 2>&1 &&
    management_cpu_reserved=true
management_memory_reserved=false
[ "$active_memory_limit_bytes" = "$expected_active_memory_limit_bytes" ] &&
    [ "$management_memory_reserve_bytes" = "$expected_management_memory_reserve_bytes" ] &&
    [ "${capacity_reserve_bytes:-0}" -ge "${expected_management_memory_reserve_bytes:-1}" ] 2>/dev/null &&
    management_memory_reserved=true
direct_local_mounts=false
[ "$config_local_sha256" = "$expected_config_local_sha256" ] &&
    [ "$vault_local_sha256" = "$expected_vault_local_sha256" ] &&
    [[ "$config_local_sha256" =~ ^[0-9a-f]{64}$ ]] &&
    [[ "$vault_local_sha256" =~ ^[0-9a-f]{64}$ ]] && direct_local_mounts=true

cgroup_v2=false
if [ "$(stat -f -c '%T' /sys/fs/cgroup 2>/dev/null)" = cgroup2fs ] &&
   [[ "$expected_cgroup" =~ ^/[A-Za-z0-9._/-]+$ ]]; then
    cgroup_v2=true
fi

docker_runtime=false
[[ "$docker_server_version" =~ ^[0-9]+\.[0-9]+ ]] &&
    [ "$docker_cgroup_version" = 2 ] &&
    [ "$docker_daemon_config_secure" = true ] &&
    [ "$docker_daemon_config_sha256" = "$expected_docker_daemon_config_sha256" ] &&
    [[ "$postgres_image_id" =~ ^sha256:[0-9a-f]{64}$ ]] &&
    [[ "$redis_image_id" =~ ^sha256:[0-9a-f]{64}$ ]] && docker_runtime=true
docker_user_namespace=false
if [ "$docker_id_map_remapped" = true ] &&
   grep -Eq 'name=(userns|rootless)' <<<"$docker_security_options"; then
    docker_user_namespace=true
fi

secure_root_path() {
    local path="$1"
    [ -n "$path" ] && [ -e "$path" ] && [ "$(stat -c %u "$path" 2>/dev/null)" = 0 ] &&
        [ $((8#$(stat -c %a "$path" 2>/dev/null) & 0022)) -eq 0 ]
}

hash_marker() {
    local path="$1" expected="$2"
    [ -n "$path" ] && [ -f "$path" ] && [ -n "$expected" ] &&
        [ "$(sha256sum "$path" 2>/dev/null | awk '{print $1}')" = "$expected" ] && secure_root_path "$path"
}

offline_build_cache=false
secure_root_path "$offline_cache" && offline_build_cache=true
template_database=false
redis_reset=false
cleanup_verified=false
services_in_job_cgroup=false
resource_limits_verified=false
resource_bomb_cleanup_verified=false
default_deny_network=false
containment_no_production_secrets=false
if hash_marker "$cleanup_marker" "$cleanup_marker_sha" &&
   jq -e \
       --arg image_digest "$image_digest" \
       --arg job_cgroup "$expected_cgroup" \
       --arg postgres_image "$expected_postgres_image" \
       --arg redis_image "$expected_redis_image" \
       --arg evaluation_cpu_list "$expected_evaluation_cpu_list" \
       --arg management_cpu_list "$expected_management_cpu_list" \
       --argjson artifact_quota_bytes "${expected_artifact_quota_bytes:-0}" \
       --argjson active_memory_limit_bytes "${expected_active_memory_limit_bytes:-0}" \
       --argjson management_memory_reserve_bytes "${expected_management_memory_reserve_bytes:-0}" \
       --argjson runner_memory_limit_bytes "${runner_memory_limit_bytes:-0}" \
       --arg config_local_sha256 "$expected_config_local_sha256" \
       --arg vault_local_sha256 "$expected_vault_local_sha256" \
       --arg docker_uid_map_sha256 "$docker_uid_map_sha256" \
       --arg docker_gid_map_sha256 "$docker_gid_map_sha256" \
       '.schema == 1 and .kind == "sim-latency-host-containment" and
        .image_digest == $image_digest and .job_cgroup == $job_cgroup and
        .postgres_image == $postgres_image and .redis_image == $redis_image and
        .artifact_quota_bytes == $artifact_quota_bytes and
        .evaluation_cpu_list == $evaluation_cpu_list and
        .management_cpu_list == $management_cpu_list and
        .active_memory_limit_bytes == $active_memory_limit_bytes and
        .management_memory_reserve_bytes == $management_memory_reserve_bytes and
        .services_in_job_cgroup == true and
        .resource_limits_verified == true and
        .cpu_bomb_cleanup_verified == true and
        .memory_bomb_oom_verified == true and
        .production_memory_limit_verified == true and
        .memory_bomb_limit_bytes == $runner_memory_limit_bytes and
        .memory_bomb_exit_code == 137 and
        .memory_bomb_oom_killed == true and
        .default_deny_network_verified == true and
        .no_published_ports_verified == true and
        .scorer_network_none_verified == true and
        .local_only_read_only_mounts_verified == true and
        .no_production_secrets_verified == true and
        .docker_user_namespace_verified == true and
        .docker_uid_map_sha256 == $docker_uid_map_sha256 and
        .docker_gid_map_sha256 == $docker_gid_map_sha256 and
        .config_local_sha256 == $config_local_sha256 and
        .vault_local_sha256 == $vault_local_sha256 and
        (.template_database_id | type == "string" and length > 0) and
        (.redis_generation_id | type == "string" and length > 0) and
        (.worker_result_sha256 | type == "string" and
          test("^[0-9a-f]{64}$")) and
        (.evidence_manifest_sha256 | type == "string" and
          test("^[0-9a-f]{64}$")) and
        (.evaluation_complete_sha256 | type == "string" and
          test("^[0-9a-f]{64}$")) and
        .cleanup_elapsed_ms >= 0 and
        .cleanup_limit_ms == 10000 and
        .cleanup_elapsed_ms <= .cleanup_limit_ms and
        .residual_containers == 0 and .residual_networks == 0 and
        .cleanup_complete == true' \
       "$cleanup_marker" >/dev/null 2>&1; then
    cleanup_verified=true
    services_in_job_cgroup=true
    resource_limits_verified=true
    resource_bomb_cleanup_verified=true
    default_deny_network=true
    containment_no_production_secrets=true
fi
immutable_reports=false
if [ "$cleanup_verified" = true ] && hash_marker "$template_marker" "$template_marker_sha" &&
   jq -e --slurpfile containment "$cleanup_marker" --arg image_digest "$image_digest" \
       '.schema == 1 and .kind == "sim-latency-template-database-reset" and
        .verified == true and .image_digest == $image_digest and
        .job_id == $containment[0].qualified_job_id and
        .round_id == $containment[0].qualified_round_id and
        .template_database_id == $containment[0].template_database_id and
        .evidence_manifest_sha256 == $containment[0].evidence_manifest_sha256 and
        .evaluation_complete_sha256 == $containment[0].evaluation_complete_sha256' \
       "$template_marker" >/dev/null 2>&1; then
    template_database=true
fi
if [ "$cleanup_verified" = true ] && hash_marker "$redis_marker" "$redis_marker_sha" &&
   jq -e --slurpfile containment "$cleanup_marker" --arg image_digest "$image_digest" \
       '.schema == 1 and .kind == "sim-latency-redis-reset" and
        .verified == true and .image_digest == $image_digest and
        .job_id == $containment[0].qualified_job_id and
        .round_id == $containment[0].qualified_round_id and
        .redis_generation_id == $containment[0].redis_generation_id and
        .evidence_manifest_sha256 == $containment[0].evidence_manifest_sha256 and
        .evaluation_complete_sha256 == $containment[0].evaluation_complete_sha256' \
       "$redis_marker" >/dev/null 2>&1; then
    redis_reset=true
fi
if [ "$cleanup_verified" = true ] && hash_marker "$immutable_marker" "$immutable_marker_sha" &&
   jq -e --slurpfile containment "$cleanup_marker" --arg image_digest "$image_digest" \
       '.schema == 1 and .kind == "sim-latency-immutable-reports" and
        .verified == true and .image_digest == $image_digest and
        .job_id == $containment[0].qualified_job_id and
        .round_id == $containment[0].qualified_round_id and
        .worker_result_sha256 == $containment[0].worker_result_sha256 and
        .evidence_manifest_sha256 == $containment[0].evidence_manifest_sha256 and
        .evaluation_complete_sha256 == $containment[0].evaluation_complete_sha256 and
        (.evidence_artifact_count | type == "number" and . > 0)' \
       "$immutable_marker" >/dev/null 2>&1; then
    immutable_reports=true
fi
artifact_storage=false
secure_root_path "$artifact_root" && artifact_storage=true

no_production_secrets="$containment_no_production_secrets"
if [ "$direct_local_mounts" != true ] ||
   grep -Eiq '/srv/warp/vault/(all|main)|/root/\.aws|/run/secrets' /proc/self/mountinfo 2>/dev/null ||
   tr '\0' '\n' </proc/self/environ 2>/dev/null | grep -Eiq '^(AWS_|.*TOKEN=|.*SECRET=|.*PASSWORD=)'; then
    no_production_secrets=false
fi

rebaseline_passed=false
rebaseline_round_id=""
if hash_marker "$rebaseline_manifest" "$rebaseline_manifest_sha" && jq -e '.schema == 1 and .passed == true and (.round_id | type == "string")' "$rebaseline_manifest" >/dev/null 2>&1; then
    rebaseline_round_id="$(jq -er '.round_id' "$rebaseline_manifest")"
    rebaseline_passed=true
fi

report="$(jq -cn \
    --arg host_id "$host_id" \
    --arg hardware_id "$hardware_id" \
    --arg qualification_sha256 "$qualification_sha256" \
    --arg image_digest "$image_digest" \
    --arg kernel_release "$kernel_release" \
    --arg microcode_revision "$microcode_revision" \
    --arg rebaseline_round_id "$rebaseline_round_id" \
    --arg irq_affinity_sha256 "$irq_affinity_sha256" \
    --arg irq_policy_sha256 "$irq_policy_sha256" \
    --argjson logical_cpu_count "${logical_cpu_count:-0}" \
    --argjson smt_disabled "$smt_disabled" \
    --argjson governor_pinned "$governor_pinned" \
    --argjson turbo_pinned "$turbo_pinned" \
    --argjson numa_pinned "$numa_pinned" \
    --argjson irq_pinned "$irq_pinned" \
    --argjson cgroup_v2 "$cgroup_v2" \
    --argjson services_in_job_cgroup "$services_in_job_cgroup" \
    --argjson default_deny_network "$default_deny_network" \
    --argjson offline_build_cache "$offline_build_cache" \
    --argjson template_database "$template_database" \
    --argjson redis_reset "$redis_reset" \
    --argjson artifact_storage "$artifact_storage" \
    --argjson immutable_reports "$immutable_reports" \
    --argjson no_production_secrets "$no_production_secrets" \
    --argjson cleanup_verified "$cleanup_verified" \
    --argjson resource_limits_verified "$resource_limits_verified" \
    --argjson management_cpu_reserved "$management_cpu_reserved" \
    --argjson management_memory_reserved "$management_memory_reserved" \
    --argjson resource_bomb_cleanup_verified "$resource_bomb_cleanup_verified" \
    --argjson rebaseline_passed "$rebaseline_passed" \
    --argjson config_secure "$config_secure" \
    --argjson qualification_match "$qualification_match" \
    --argjson cpu_count_exact "$cpu_count_exact" \
    --argjson worker_affinity_pinned "$worker_affinity_pinned" \
    --argjson kernel_pinned "$kernel_pinned" \
    --argjson docker_runtime "$docker_runtime" \
    --argjson docker_user_namespace "$docker_user_namespace" \
    '{schema:1,host_id:$host_id,hardware_id:$hardware_id,
      qualification_sha256:$qualification_sha256,image_digest:$image_digest,
      kernel_release:$kernel_release,microcode_revision:$microcode_revision,
      irq_affinity_sha256:$irq_affinity_sha256,
      irq_policy_sha256:$irq_policy_sha256,
      logical_cpu_count:$logical_cpu_count,smt_disabled:$smt_disabled,
      governor_pinned:$governor_pinned,turbo_pinned:$turbo_pinned,
      numa_pinned:$numa_pinned,irq_pinned:$irq_pinned,cgroup_v2:$cgroup_v2,
      services_in_job_cgroup:$services_in_job_cgroup,
      default_deny_network:$default_deny_network,
      offline_build_cache:$offline_build_cache,template_database:$template_database,
      redis_reset:$redis_reset,artifact_storage:$artifact_storage,
      immutable_reports:$immutable_reports,no_production_secrets:$no_production_secrets,
      cleanup_verified:$cleanup_verified,
      resource_limits_verified:$resource_limits_verified,
      management_cpu_reserved:$management_cpu_reserved,
      management_memory_reserved:$management_memory_reserved,
      resource_bomb_cleanup_verified:$resource_bomb_cleanup_verified,
      rebaseline_passed:$rebaseline_passed,
      rebaseline_round_id:(if $rebaseline_round_id == "" then null else $rebaseline_round_id end),
      checks:{config_secure:$config_secure,qualification_match:$qualification_match,
        cpu_count_exact:$cpu_count_exact,worker_affinity_pinned:$worker_affinity_pinned,
        kernel_microcode_pinned:$kernel_pinned,
        docker_runtime:$docker_runtime,docker_user_namespace:$docker_user_namespace}}')"

printf '%s\n' "$report"
jq -e '
    ([$r.checks[]] | all) and
    $r.smt_disabled and $r.governor_pinned and $r.turbo_pinned and
    $r.numa_pinned and $r.irq_pinned and $r.cgroup_v2 and
    $r.services_in_job_cgroup and $r.default_deny_network and
    $r.offline_build_cache and $r.template_database and $r.redis_reset and
    $r.artifact_storage and $r.immutable_reports and $r.no_production_secrets and
    $r.cleanup_verified and $r.resource_limits_verified and
    $r.management_cpu_reserved and $r.management_memory_reserved and
    $r.resource_bomb_cleanup_verified
' --argjson r "$report" <<<"$report" >/dev/null
