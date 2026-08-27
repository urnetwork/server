#!/usr/bin/env bash

# Promote authenticated evaluator and adversarial-resource evidence into the
# root-owned runtime markers consumed by host-self-check.sh. This command is
# intentionally separate from the evaluator: candidate code cannot update its
# own host qualification. Runtime markers are recreated after every reboot.

set -Eeuo pipefail
umask 077

readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
readonly RESOURCE_BOUNDARY="$SCRIPT_DIR/container/resource-boundary.sh"
readonly HASH_LOCAL_MOUNT="$SCRIPT_DIR/container/hash-local-mount.sh"

host_config=""
evaluation_dir=""
resource_bomb_report=""

usage() {
    printf 'usage: promote-host-containment --host-config PATH --evaluation-dir PATH --resource-bomb-report PATH\n' >&2
    exit 2
}

die() {
    printf '[competition-host-promotion] ERROR: %s\n' "$*" >&2
    exit 1
}

while [ "$#" -gt 0 ]; do
    case "$1" in
        --host-config)
            [ "$#" -ge 2 ] || usage
            host_config="$2"
            shift 2
            ;;
        --evaluation-dir)
            [ "$#" -ge 2 ] || usage
            evaluation_dir="$2"
            shift 2
            ;;
        --resource-bomb-report)
            [ "$#" -ge 2 ] || usage
            resource_bomb_report="$2"
            shift 2
            ;;
        *) usage ;;
    esac
done

[ "$(id -u)" -eq 0 ] || die "promotion must run as root"
[ -n "$host_config" ] && [ -n "$evaluation_dir" ] && [ -n "$resource_bomb_report" ] || usage
for command in find install jq mv realpath sha256sum stat sync; do
    command -v "$command" >/dev/null 2>&1 || die "required command missing: $command"
done
[ -x "$RESOURCE_BOUNDARY" ] || die "resource-boundary helper is unavailable"
[ -x "$HASH_LOCAL_MOUNT" ] || die "local-mount digest helper is unavailable"

secure_regular() {
    local path="$1"
    [ -f "$path" ] && [ ! -L "$path" ]
}

secure_root_owned() {
    local path="$1" mode
    secure_regular "$path" || return 1
    [ "$(stat -c %u "$path")" -eq 0 ] || return 1
    mode="$(stat -c %a "$path")"
    [ $((8#$mode & 0022)) -eq 0 ]
}

sha256_file() {
    sha256sum "$1" | awk '{print $1}'
}

file_bytes() {
    stat -c %s "$1"
}

host_config="$(realpath -e -- "$host_config")"
evaluation_dir="$(realpath -e -- "$evaluation_dir")"
resource_bomb_report="$(realpath -e -- "$resource_bomb_report")"
secure_root_owned "$host_config" || die "host config must be a root-owned, non-writable regular file"
host_config_parent="$(dirname -- "$host_config")"
[ "$(stat -c %u "$host_config_parent")" -eq 0 ] || die "host config parent is not root-owned"
host_config_parent_mode="$(stat -c %a "$host_config_parent")"
[ $((8#$host_config_parent_mode & 0022)) -eq 0 ] || die "host config parent is group/world writable"
[ -d "$evaluation_dir" ] && [ ! -L "$evaluation_dir" ] || die "evaluation directory is unsafe"
secure_regular "$resource_bomb_report" || die "resource-bomb report is unsafe"

readonly worker_result="$evaluation_dir/worker-result.json"
readonly completion="$evaluation_dir/evaluation.complete.json"
readonly evidence_manifest="$evaluation_dir/evidence-manifest.json"
for path in "$worker_result" "$completion" "$evidence_manifest"; do
    secure_regular "$path" || die "mandatory evaluator artifact is missing: ${path##*/}"
done

readonly image_digest="$(jq -er '.image_digest' "$host_config")"
readonly job_cgroup="$(jq -er '.job_cgroup' "$host_config")"
readonly postgres_image="$(jq -er '.postgres_image' "$host_config")"
readonly redis_image="$(jq -er '.redis_image' "$host_config")"
readonly evaluation_cpuset="$(jq -er '.evaluation_cpu_list' "$host_config")"
readonly management_cpuset="$(jq -er '.management_cpu_list' "$host_config")"
readonly artifact_quota_bytes="$(jq -er '.artifact_quota_bytes' "$host_config")"
readonly active_memory_limit_bytes="$(jq -er '.active_memory_limit_bytes' "$host_config")"
readonly management_memory_reserve_bytes="$(jq -er '.management_memory_reserve_bytes' "$host_config")"
readonly config_local_directory="$(jq -er '.config_local_directory' "$host_config")"
readonly vault_local_directory="$(jq -er '.vault_local_directory' "$host_config")"
readonly config_local_sha256="$(jq -er '.config_local_sha256' "$host_config")"
readonly vault_local_sha256="$(jq -er '.vault_local_sha256' "$host_config")"
readonly template_output_path="$(jq -er '.template_database_marker' "$host_config")"
readonly redis_output_path="$(jq -er '.redis_reset_marker' "$host_config")"
readonly output_path="$(jq -er '.cleanup_marker' "$host_config")"
readonly immutable_output_path="$(jq -er '.immutable_reports_marker' "$host_config")"

[[ "$image_digest" =~ ^sha256:[0-9a-f]{64}$ ]] || die "host image digest is invalid"
[[ "$job_cgroup" =~ ^/[A-Za-z0-9._/-]+$ ]] || die "host job cgroup is invalid"
[[ "$evaluation_cpuset" =~ ^[0-9,-]+$ ]] || die "evaluation CPU list is invalid"
[[ "$management_cpuset" =~ ^[0-9,-]+$ ]] || die "management CPU list is invalid"
[[ "$artifact_quota_bytes" =~ ^[0-9]+$ ]] || die "artifact quota is invalid"
[[ "$active_memory_limit_bytes" =~ ^[0-9]+$ ]] || die "active memory limit is invalid"
[[ "$management_memory_reserve_bytes" =~ ^[0-9]+$ ]] || die "management reserve is invalid"
[[ "$config_local_sha256" =~ ^[0-9a-f]{64}$ ]] || die "config/local digest is invalid"
[[ "$vault_local_sha256" =~ ^[0-9a-f]{64}$ ]] || die "vault/local digest is invalid"
declare -A output_path_seen=()
for marker_path in "$template_output_path" "$redis_output_path" "$output_path" "$immutable_output_path"; do
    [[ "$marker_path" == /* ]] && [ "$marker_path" != / ] || die "runtime marker path is unsafe"
    [ "$(realpath -m -- "$marker_path")" = "$marker_path" ] || die "runtime marker path is non-canonical"
    [ -z "${output_path_seen[$marker_path]:-}" ] || die "runtime marker paths must be unique"
    output_path_seen[$marker_path]=1
done
[ "$config_local_directory" = "$(realpath -e "$config_local_directory")" ] || die "config/local path is unsafe"
[ "$vault_local_directory" = "$(realpath -e "$vault_local_directory")" ] || die "vault/local path is unsafe"
[ "$($HASH_LOCAL_MOUNT "$config_local_directory")" = "$config_local_sha256" ] ||
    die "live config/local content does not match host config"
[ "$($HASH_LOCAL_MOUNT "$vault_local_directory")" = "$vault_local_sha256" ] ||
    die "live vault/local content does not match host config"

boundary_json="$($RESOURCE_BOUNDARY)"
jq -e \
    --arg evaluation_cpuset "$evaluation_cpuset" \
    --arg management_cpuset "$management_cpuset" \
    --argjson active_memory_limit_bytes "$active_memory_limit_bytes" \
    --argjson management_memory_reserve_bytes "$management_memory_reserve_bytes" \
    '.schema == 1 and .evaluation_cpuset == $evaluation_cpuset and
     .management_cpuset == $management_cpuset and
     .active_memory_limit_bytes == $active_memory_limit_bytes and
     .minimum_management_memory_reserve_bytes == $management_memory_reserve_bytes and
     .evaluation_physical_core_count == 10 and .management_physical_core_count == 2 and
     .disjoint_cpu_sets == true and .memory_capacity_passed == true' \
    <<<"$boundary_json" >/dev/null || die "live host resource boundary does not match config"
runner_memory_limit_bytes="$(jq -er '.runner_memory_limit_bytes' <<<"$boundary_json")"

readonly security_keys='["accounting_complete","cgroup_contained","cleanup_complete","default_deny_network","immutable_reports","management_cpu_reserved","management_memory_reserved","no_production_secrets","offline_build","offline_build_resource_limits","redis_reset","resource_limits","resource_report_complete","structural_patch_check","template_database_reset"]'
readonly score_gate_keys='["G1_success","G2_volume","G3_path_integrity","G4_matchmaking","G5_stability","G6_resources"]'
jq -e --argjson security_keys "$security_keys" --argjson score_gate_keys "$score_gate_keys" \
    '.schema == 1 and .eval_error == null and .score != null and
     .score.score_schema == 1 and .score.placeable == true and
     ((.score.gates | keys | sort) == $score_gate_keys) and
     ([.score.gates[] | .passed == true] | all) and
     ([.security | to_entries[] | select(.value | type == "boolean") | .key] | sort) == $security_keys and
     ([.security | to_entries[] | select(.value | type == "boolean") | .value == true] | all) and
     (.security.cgroup_id | type == "string" and length > 0) and
     (.security.template_database_id | type == "string" and length > 0) and
     (.security.redis_generation_id | type == "string" and length > 0)' \
    "$worker_result" >/dev/null || die "worker result did not pass every score and containment gate"

job_id="$(jq -er '.job_id' "$worker_result")"
round_id="$(jq -er '.round_id' "$completion")"
[[ "$job_id" =~ ^[0-9a-f-]{36}$ ]] || die "worker job id is invalid"
[[ "$round_id" =~ ^[0-9a-f-]{36}$ ]] || die "completion round id is invalid"

authenticate_declared_artifact() {
    local relative="$1" path expected_sha expected_bytes
    path="$evaluation_dir/$relative"
    secure_regular "$path" || die "declared artifact is missing: $relative"
    expected_sha="$(jq -er --arg path "$relative" '.artifacts[] | select(.path == $path) | .sha256' "$worker_result")"
    expected_bytes="$(jq -er --arg path "$relative" '.artifacts[] | select(.path == $path) | .bytes' "$worker_result")"
    [ "$(sha256_file "$path")" = "$expected_sha" ] || die "declared artifact hash mismatch: $relative"
    [ "$(file_bytes "$path")" = "$expected_bytes" ] || die "declared artifact size mismatch: $relative"
}
authenticate_declared_artifact evaluation.complete.json
authenticate_declared_artifact evidence-manifest.json

completion_sha256="$(sha256_file "$completion")"
evidence_manifest_sha256="$(sha256_file "$evidence_manifest")"
jq -e --arg job_id "$job_id" --arg round_id "$round_id" \
    --arg image_digest "$image_digest" --arg evidence_sha "$evidence_manifest_sha256" \
    '.schema == 1 and .kind == "sim-latency-worker-evaluation-complete" and
     .job_id == $job_id and .round_id == $round_id and .base_image_id == $image_digest and
     .cleanup_complete == true and .artifacts.evidence_manifest == $evidence_sha' \
    "$completion" >/dev/null || die "completion marker identity is invalid"
jq -e --arg job_id "$job_id" --arg round_id "$round_id" \
    '.schema == 1 and .kind == "sim-latency-evidence-manifest" and
     .job_id == $job_id and .round_id == $round_id and
     (.artifacts | type == "array" and length > 0)' \
    "$evidence_manifest" >/dev/null || die "evidence manifest identity is invalid"

container_evidence_count=0
local_mount_evidence_count=0
docker_id_map_evidence_count=0
docker_uid_map_sha256=""
docker_gid_map_sha256=""
while IFS=$'\t' read -r relative expected_sha expected_bytes; do
    case "$relative" in
        evidence/*) ;;
        *) die "evidence manifest path is outside evidence/: $relative" ;;
    esac
    case "/$relative/" in
        *'/../'*|*'/./'*) die "evidence manifest path is non-canonical: $relative" ;;
    esac
    path="$evaluation_dir/$relative"
    secure_regular "$path" || die "evidence file is missing or non-regular: $relative"
    [ "$(sha256_file "$path")" = "$expected_sha" ] || die "evidence hash mismatch: $relative"
    [ "$(file_bytes "$path")" = "$expected_bytes" ] || die "evidence size mismatch: $relative"
    case "$relative" in
        evidence/docker-id-map.json)
            jq -e --arg image_id "$image_digest" \
                '.schema == 1 and .kind == "sim-latency-docker-id-map" and
                 .image_id == $image_id and .container_uid == 65532 and .container_gid == 65532 and
                 .host_uid != .container_uid and .host_gid != .container_gid and
                 .root_host_uid != 0 and .root_host_gid != 0 and .remapped == true and
                 (.uid_map_sha256 | test("^[0-9a-f]{64}$")) and
                 (.gid_map_sha256 | test("^[0-9a-f]{64}$")) and
                 (.daemon_security_options | type == "array" and
                   any(.[]; test("name=(userns|rootless)")))' \
                "$path" >/dev/null || die "Docker user-namespace evidence is invalid"
            docker_uid_map_sha256="$(jq -er '.uid_map_sha256' "$path")"
            docker_gid_map_sha256="$(jq -er '.gid_map_sha256' "$path")"
            docker_id_map_evidence_count=$((docker_id_map_evidence_count + 1))
            ;;
        evidence/local-mounts.json)
            jq -e --arg config_sha "$config_local_sha256" --arg vault_sha "$vault_local_sha256" \
                '.schema == 1 and .kind == "sim-latency-local-mounts" and
                 .direct_bind == true and .read_only == true and
                 .parent_mounts == false and .all_main_site_absent == true and
                 .config.target == "/runtime/config/local" and .config.sha256 == $config_sha and
                 .vault.target == "/runtime/vault/local" and .vault.sha256 == $vault_sha' \
                "$path" >/dev/null || die "direct local mount evidence is invalid"
            local_mount_evidence_count=$((local_mount_evidence_count + 1))
            ;;
        evidence/runs/baseline-*/containers.json|evidence/runs/candidate-*/containers.json)
            jq -e \
                'type == "array" and length == 3 and
                 (map(select(.name | endswith("-runner-1"))) | length == 1) and
                 (map(select(.name | endswith("-runner-1")))[0] |
                   ([.mounts[] | select(.destination | startswith("/runtime"))] | length == 2) and
                   (any(.mounts[]; .type == "bind" and
                     .destination == "/runtime/config/local" and .rw == false)) and
                   (any(.mounts[]; .type == "bind" and
                     .destination == "/runtime/vault/local" and .rw == false)) and
                   .host_config.readonly_rootfs == true and
                   .host_config.network_mode != "host" and
                   .state.exit_code == 0 and .state.oom_killed == false)' \
                "$path" >/dev/null || die "container evidence violates the local-only boundary: $relative"
            container_evidence_count=$((container_evidence_count + 1))
            ;;
    esac
done < <(jq -er '.artifacts[] | [.path,.sha256,(.bytes|tostring)] | @tsv' "$evidence_manifest")
[ "$container_evidence_count" -ge 2 ] || die "baseline/candidate container evidence is incomplete"
[ "$local_mount_evidence_count" -eq 1 ] || die "direct local mount evidence is missing or duplicated"
[ "$docker_id_map_evidence_count" -eq 1 ] || die "Docker user-namespace evidence is missing or duplicated"

jq -e \
    --arg evaluation_cpuset "$evaluation_cpuset" \
    --arg management_cpuset "$management_cpuset" \
    --argjson runner_memory_limit_bytes "$runner_memory_limit_bytes" \
    '.schema == 1 and .kind == "sim-latency-resource-bomb-cleanup" and
     .evaluation_cpuset == $evaluation_cpuset and .management_cpuset == $management_cpuset and
     .cpu_bomb_observed_cpuset == $evaluation_cpuset and
     .cpu_bomb_saturated_evaluation_set == true and
     .memory_limit_bytes == $runner_memory_limit_bytes and
     .production_memory_limit == true and .memory_exit_code == 137 and
     .memory_oom_killed == true and .cleanup_complete == true and
     .cleanup_elapsed_ms >= 0 and .cleanup_limit_ms == 10000 and
     .cleanup_elapsed_ms <= .cleanup_limit_ms and
     .residual_containers == 0 and .residual_networks == 0' \
    "$resource_bomb_report" >/dev/null || die "production resource-bomb evidence is invalid"

prepare_output_parent() {
    local path="$1" parent grandparent mode
    parent="$(dirname -- "$path")"
    if [ ! -e "$parent" ]; then
        grandparent="$(dirname -- "$parent")"
        [ -d "$grandparent" ] && [ ! -L "$grandparent" ] ||
            die "runtime marker grandparent is unsafe"
        [ "$(stat -c %u "$grandparent")" -eq 0 ] ||
            die "runtime marker grandparent is not root-owned"
        mode="$(stat -c %a "$grandparent")"
        [ $((8#$mode & 0022)) -eq 0 ] ||
            die "runtime marker grandparent is group/world writable"
        install -d -o 0 -g 0 -m 0700 "$parent"
    fi
    [ -d "$parent" ] && [ ! -L "$parent" ] || die "runtime marker parent is unsafe"
    [ "$(stat -c %u "$parent")" -eq 0 ] || die "runtime marker parent is not root-owned"
    mode="$(stat -c %a "$parent")"
    [ $((8#$mode & 0022)) -eq 0 ] || die "runtime marker parent is group/world writable"
    if [ -e "$path" ]; then
        secure_root_owned "$path" || die "existing runtime marker is unsafe"
    fi
    printf '%s' "$parent"
}

template_output_parent="$(prepare_output_parent "$template_output_path")"
redis_output_parent="$(prepare_output_parent "$redis_output_path")"
output_parent="$(prepare_output_parent "$output_path")"
immutable_output_parent="$(prepare_output_parent "$immutable_output_path")"

template_database_id="$(jq -er '.security.template_database_id' "$worker_result")"
redis_generation_id="$(jq -er '.security.redis_generation_id' "$worker_result")"
worker_result_sha256="$(sha256_file "$worker_result")"
evidence_artifact_count="$(jq -er '.artifacts | length' "$evidence_manifest")"
promoted_at="$(date -u '+%FT%TZ')"

template_tmp="$template_output_parent/.competition-template-database.$$.new"
redis_tmp="$redis_output_parent/.competition-redis-reset.$$.new"
marker_tmp="$output_parent/.competition-containment.$$.new"
immutable_tmp="$immutable_output_parent/.competition-immutable-reports.$$.new"
config_tmp="$host_config_parent/.competition-host.$$.new"
cleanup_tmp() {
    rm -f -- "$template_tmp" "$redis_tmp" "$marker_tmp" "$immutable_tmp" "$config_tmp"
}
trap cleanup_tmp EXIT

jq -n --arg promoted_at "$promoted_at" --arg job_id "$job_id" \
    --arg round_id "$round_id" --arg image_digest "$image_digest" \
    --arg template_database_id "$template_database_id" \
    --arg evidence_manifest_sha256 "$evidence_manifest_sha256" \
    --arg evaluation_complete_sha256 "$completion_sha256" \
    '{schema:1,kind:"sim-latency-template-database-reset",promoted_at:$promoted_at,
      job_id:$job_id,round_id:$round_id,image_digest:$image_digest,
      template_database_id:$template_database_id,
      evidence_manifest_sha256:$evidence_manifest_sha256,
      evaluation_complete_sha256:$evaluation_complete_sha256,verified:true}' > "$template_tmp"

jq -n --arg promoted_at "$promoted_at" --arg job_id "$job_id" \
    --arg round_id "$round_id" --arg image_digest "$image_digest" \
    --arg redis_generation_id "$redis_generation_id" \
    --arg evidence_manifest_sha256 "$evidence_manifest_sha256" \
    --arg evaluation_complete_sha256 "$completion_sha256" \
    '{schema:1,kind:"sim-latency-redis-reset",promoted_at:$promoted_at,
      job_id:$job_id,round_id:$round_id,image_digest:$image_digest,
      redis_generation_id:$redis_generation_id,
      evidence_manifest_sha256:$evidence_manifest_sha256,
      evaluation_complete_sha256:$evaluation_complete_sha256,verified:true}' > "$redis_tmp"

jq -n --arg promoted_at "$promoted_at" --arg job_id "$job_id" \
    --arg round_id "$round_id" --arg image_digest "$image_digest" \
    --arg worker_result_sha256 "$worker_result_sha256" \
    --arg evidence_manifest_sha256 "$evidence_manifest_sha256" \
    --arg evaluation_complete_sha256 "$completion_sha256" \
    --argjson evidence_artifact_count "$evidence_artifact_count" \
    '{schema:1,kind:"sim-latency-immutable-reports",promoted_at:$promoted_at,
      job_id:$job_id,round_id:$round_id,image_digest:$image_digest,
      worker_result_sha256:$worker_result_sha256,
      evidence_manifest_sha256:$evidence_manifest_sha256,
      evaluation_complete_sha256:$evaluation_complete_sha256,
      evidence_artifact_count:$evidence_artifact_count,verified:true}' > "$immutable_tmp"

jq -n \
    --arg promoted_at "$promoted_at" \
    --arg image_digest "$image_digest" \
    --arg job_cgroup "$job_cgroup" \
    --arg postgres_image "$postgres_image" \
    --arg redis_image "$redis_image" \
    --arg evaluation_cpu_list "$evaluation_cpuset" \
    --arg management_cpu_list "$management_cpuset" \
    --arg job_id "$job_id" --arg round_id "$round_id" \
    --arg template_database_id "$template_database_id" \
    --arg redis_generation_id "$redis_generation_id" \
    --arg worker_result_sha256 "$worker_result_sha256" \
    --arg evidence_manifest_sha256 "$evidence_manifest_sha256" \
    --arg evaluation_complete_sha256 "$completion_sha256" \
    --arg config_local_sha256 "$config_local_sha256" \
    --arg vault_local_sha256 "$vault_local_sha256" \
    --arg docker_uid_map_sha256 "$docker_uid_map_sha256" \
    --arg docker_gid_map_sha256 "$docker_gid_map_sha256" \
    --argjson artifact_quota_bytes "$artifact_quota_bytes" \
    --argjson active_memory_limit_bytes "$active_memory_limit_bytes" \
    --argjson management_memory_reserve_bytes "$management_memory_reserve_bytes" \
    --argjson runner_memory_limit_bytes "$runner_memory_limit_bytes" \
    --argjson cleanup_elapsed_ms "$(jq -er '.cleanup_elapsed_ms' "$resource_bomb_report")" \
    '{schema:1,kind:"sim-latency-host-containment",promoted_at:$promoted_at,
      qualified_job_id:$job_id,qualified_round_id:$round_id,
      template_database_id:$template_database_id,
      redis_generation_id:$redis_generation_id,
      image_digest:$image_digest,job_cgroup:$job_cgroup,
      postgres_image:$postgres_image,redis_image:$redis_image,
      artifact_quota_bytes:$artifact_quota_bytes,
      evaluation_cpu_list:$evaluation_cpu_list,
      management_cpu_list:$management_cpu_list,
      active_memory_limit_bytes:$active_memory_limit_bytes,
      management_memory_reserve_bytes:$management_memory_reserve_bytes,
      services_in_job_cgroup:true,resource_limits_verified:true,
      cpu_bomb_cleanup_verified:true,memory_bomb_oom_verified:true,
      production_memory_limit_verified:true,
      memory_bomb_limit_bytes:$runner_memory_limit_bytes,
      memory_bomb_exit_code:137,memory_bomb_oom_killed:true,
      default_deny_network_verified:true,no_published_ports_verified:true,
      scorer_network_none_verified:true,local_only_read_only_mounts_verified:true,
      no_production_secrets_verified:true,
      docker_user_namespace_verified:true,
      docker_uid_map_sha256:$docker_uid_map_sha256,
      docker_gid_map_sha256:$docker_gid_map_sha256,
      config_local_sha256:$config_local_sha256,
      vault_local_sha256:$vault_local_sha256,
      worker_result_sha256:$worker_result_sha256,
      evidence_manifest_sha256:$evidence_manifest_sha256,
      evaluation_complete_sha256:$evaluation_complete_sha256,
      cleanup_elapsed_ms:$cleanup_elapsed_ms,cleanup_limit_ms:10000,
      residual_containers:0,residual_networks:0,cleanup_complete:true}' > "$marker_tmp"
for pending_marker in "$template_tmp" "$redis_tmp" "$marker_tmp" "$immutable_tmp"; do
    chmod 0600 "$pending_marker"
    sync -d "$pending_marker"
done
template_sha256="$(sha256_file "$template_tmp")"
redis_sha256="$(sha256_file "$redis_tmp")"
marker_sha256="$(sha256_file "$marker_tmp")"
immutable_sha256="$(sha256_file "$immutable_tmp")"

jq --arg template_sha "$template_sha256" --arg redis_sha "$redis_sha256" \
    --arg marker_sha "$marker_sha256" --arg immutable_sha "$immutable_sha256" \
    '.template_database_marker_sha256 = $template_sha |
     .redis_reset_marker_sha256 = $redis_sha |
     .cleanup_marker_sha256 = $marker_sha |
     .immutable_reports_marker_sha256 = $immutable_sha' "$host_config" > "$config_tmp"
chmod 0600 "$config_tmp"
sync -d "$config_tmp"
mv -f -- "$template_tmp" "$template_output_path"
mv -f -- "$redis_tmp" "$redis_output_path"
mv -f -- "$marker_tmp" "$output_path"
mv -f -- "$immutable_tmp" "$immutable_output_path"
mv -f -- "$config_tmp" "$host_config"
sync -d "$template_output_path" "$redis_output_path" "$output_path" \
    "$immutable_output_path" "$host_config"
sync "$template_output_parent" "$redis_output_parent" "$output_parent" \
    "$immutable_output_parent" "$host_config_parent"

jq -cn --arg path "$output_path" --arg sha256 "$marker_sha256" \
    --arg template_path "$template_output_path" --arg template_sha "$template_sha256" \
    --arg redis_path "$redis_output_path" --arg redis_sha "$redis_sha256" \
    --arg immutable_path "$immutable_output_path" --arg immutable_sha "$immutable_sha256" \
    --arg job_id "$job_id" --arg round_id "$round_id" \
    '{schema:1,kind:"sim-latency-host-containment-promotion",path:$path,
      sha256:$sha256,job_id:$job_id,round_id:$round_id,promoted:true,
      markers:{template_database:{path:$template_path,sha256:$template_sha},
        redis_reset:{path:$redis_path,sha256:$redis_sha},
        cleanup:{path:$path,sha256:$sha256},
        immutable_reports:{path:$immutable_path,sha256:$immutable_sha}}}'
