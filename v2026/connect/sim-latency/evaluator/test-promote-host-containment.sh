#!/usr/bin/env bash

# Deterministic positive and adversarial fixtures for the root-owned host
# containment promotion boundary.

set -Eeuo pipefail
umask 077

readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
readonly PROMOTER="$SCRIPT_DIR/promote-host-containment.sh"
readonly RESOURCE_BOUNDARY="$SCRIPT_DIR/container/resource-boundary.sh"

for command in jq mktemp sha256sum stat sudo; do
    command -v "$command" >/dev/null 2>&1 || {
        printf 'missing command: %s\n' "$command" >&2
        exit 1
    }
done

test_root="$(mktemp -d -t competition-host-promotion.XXXXXXXX)"
cleanup() {
    case "$test_root" in
        /tmp/competition-host-promotion.*) sudo -n rm -rf -- "$test_root" ;;
        *) printf 'refusing unsafe cleanup path: %s\n' "$test_root" >&2 ;;
    esac
}
trap cleanup EXIT

sha256_file() { sha256sum "$1" | awk '{print $1}'; }
file_bytes() { stat -c %s "$1"; }

readonly job_id=aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa
readonly round_id=bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb
readonly image_digest=sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
boundary="$($RESOURCE_BOUNDARY)"
evaluation_cpuset="$(jq -er '.evaluation_cpuset' <<<"$boundary")"
management_cpuset="$(jq -er '.management_cpuset' <<<"$boundary")"
active_memory_limit_bytes="$(jq -er '.active_memory_limit_bytes' <<<"$boundary")"
management_memory_reserve_bytes="$(jq -er '.minimum_management_memory_reserve_bytes' <<<"$boundary")"
runner_memory_limit_bytes="$(jq -er '.runner_memory_limit_bytes' <<<"$boundary")"

make_containers() {
    local path="$1"
    jq -n \
        '[{name:"/fixture-runner-1",
           host_config:{readonly_rootfs:true,network_mode:"fixture_evaluation"},
           mounts:[
             {type:"bind",destination:"/runtime/config/local",rw:false},
             {type:"bind",destination:"/runtime/vault/local",rw:false}
           ],state:{exit_code:0,oom_killed:false}},
          {name:"/fixture-postgres-1",host_config:{},mounts:[],state:{}},
          {name:"/fixture-redis-1",host_config:{},mounts:[],state:{}}]' > "$path"
}

write_evidence_manifest() {
    local evaluation="$1" baseline candidate local_mounts docker_id_map
    baseline="$evaluation/evidence/runs/baseline-01/containers.json"
    candidate="$evaluation/evidence/runs/candidate-01/containers.json"
    local_mounts="$evaluation/evidence/local-mounts.json"
    docker_id_map="$evaluation/evidence/docker-id-map.json"
    jq -n --arg job_id "$job_id" --arg round_id "$round_id" \
        --arg baseline_sha "$(sha256_file "$baseline")" \
        --arg candidate_sha "$(sha256_file "$candidate")" \
        --arg local_mounts_sha "$(sha256_file "$local_mounts")" \
        --arg docker_id_map_sha "$(sha256_file "$docker_id_map")" \
        --argjson baseline_bytes "$(file_bytes "$baseline")" \
        --argjson candidate_bytes "$(file_bytes "$candidate")" \
        --argjson local_mounts_bytes "$(file_bytes "$local_mounts")" \
        --argjson docker_id_map_bytes "$(file_bytes "$docker_id_map")" \
        '{schema:1,kind:"sim-latency-evidence-manifest",job_id:$job_id,round_id:$round_id,
          artifacts:[
            {path:"evidence/docker-id-map.json",sha256:$docker_id_map_sha,bytes:$docker_id_map_bytes},
            {path:"evidence/local-mounts.json",sha256:$local_mounts_sha,bytes:$local_mounts_bytes},
            {path:"evidence/runs/baseline-01/containers.json",sha256:$baseline_sha,bytes:$baseline_bytes},
            {path:"evidence/runs/candidate-01/containers.json",sha256:$candidate_sha,bytes:$candidate_bytes}
          ]}' > "$evaluation/evidence-manifest.json"
}

write_completion() {
    local evaluation="$1" evidence_sha
    evidence_sha="$(sha256_file "$evaluation/evidence-manifest.json")"
    jq -n --arg job_id "$job_id" --arg round_id "$round_id" \
        --arg image_digest "$image_digest" --arg evidence_sha "$evidence_sha" \
        '{schema:1,kind:"sim-latency-worker-evaluation-complete",job_id:$job_id,
          round_id:$round_id,base_image_id:$image_digest,cleanup_complete:true,
          artifacts:{evidence_manifest:$evidence_sha}}' > "$evaluation/evaluation.complete.json"
}

write_worker_result() {
    local evaluation="$1" completion evidence
    completion="$evaluation/evaluation.complete.json"
    evidence="$evaluation/evidence-manifest.json"
    jq -n --arg job_id "$job_id" \
        --arg completion_sha "$(sha256_file "$completion")" \
        --arg evidence_sha "$(sha256_file "$evidence")" \
        --argjson completion_bytes "$(file_bytes "$completion")" \
        --argjson evidence_bytes "$(file_bytes "$evidence")" \
        '{schema:1,job_id:$job_id,eval_error:null,
          score:{score_schema:1,placeable:true,gates:{G1:{passed:true}}},
          security:{template_database_reset:true,redis_reset:true,cgroup_contained:true,
            resource_limits:true,management_cpu_reserved:true,management_memory_reserved:true,
            default_deny_network:true,offline_build:true,offline_build_resource_limits:true,
            no_production_secrets:true,structural_patch_check:true,accounting_complete:true,
            resource_report_complete:true,cleanup_complete:true,immutable_reports:true,
            cgroup_id:"fixture.slice",template_database_id:"template",redis_generation_id:"redis"},
          artifacts:[
            {path:"evaluation.complete.json",sha256:$completion_sha,bytes:$completion_bytes},
            {path:"evidence-manifest.json",sha256:$evidence_sha,bytes:$evidence_bytes}
          ]}' > "$evaluation/worker-result.json"
}

make_evaluation() {
    local evaluation="$1"
    install -d -m 0700 "$evaluation/evidence/runs/baseline-01" "$evaluation/evidence/runs/candidate-01"
    make_containers "$evaluation/evidence/runs/baseline-01/containers.json"
    make_containers "$evaluation/evidence/runs/candidate-01/containers.json"
    jq -n --arg config_sha "$config_local_sha256" --arg vault_sha "$vault_local_sha256" \
        '{schema:1,kind:"sim-latency-local-mounts",direct_bind:true,read_only:true,
          parent_mounts:false,all_main_site_absent:true,
          config:{target:"/runtime/config/local",sha256:$config_sha},
          vault:{target:"/runtime/vault/local",sha256:$vault_sha}}' \
        > "$evaluation/evidence/local-mounts.json"
    jq -n --arg image_id "$image_digest" \
        '{schema:1,kind:"sim-latency-docker-id-map",image_id:$image_id,
          container_uid:65532,host_uid:165532,container_gid:65532,host_gid:165532,
          root_host_uid:100000,root_host_gid:100000,
          uid_map_sha256:("d" * 64),gid_map_sha256:("e" * 64),remapped:true,
          daemon_security_options:["name=seccomp,profile=builtin","name=userns"]}' \
        > "$evaluation/evidence/docker-id-map.json"
    write_evidence_manifest "$evaluation"
    write_completion "$evaluation"
    write_worker_result "$evaluation"
}

make_host_config() {
    local path="$1" marker="$2" pending marker_prefix
    pending="$test_root/host-config.$$.new"
    marker_prefix="${marker%.json}"
    jq -n --arg image_digest "$image_digest" \
        --arg config_sha "$config_local_sha256" --arg vault_sha "$vault_local_sha256" \
        --arg config_dir "$test_root/config/local" --arg vault_dir "$test_root/vault/local" \
        --arg evaluation_cpuset "$evaluation_cpuset" --arg management_cpuset "$management_cpuset" \
        --arg template_marker "$marker_prefix.template-database.json" \
        --arg redis_marker "$marker_prefix.redis-reset.json" \
        --arg marker "$marker" \
        --arg immutable_marker "$marker_prefix.immutable-reports.json" \
        --argjson active_memory "$active_memory_limit_bytes" \
        --argjson reserve_memory "$management_memory_reserve_bytes" \
        '{schema:1,image_digest:$image_digest,job_cgroup:"/urnetwork/competition.slice/evaluator.scope",
          postgres_image:"postgres:fixture",redis_image:"redis:fixture",
          evaluation_cpu_list:$evaluation_cpuset,management_cpu_list:$management_cpuset,
          artifact_quota_bytes:34359738368,active_memory_limit_bytes:$active_memory,
          management_memory_reserve_bytes:$reserve_memory,
          template_database_marker:$template_marker,
          template_database_marker_sha256:("0" * 64),
          redis_reset_marker:$redis_marker,redis_reset_marker_sha256:("0" * 64),
          cleanup_marker:$marker,
          immutable_reports_marker:$immutable_marker,
          immutable_reports_marker_sha256:("0" * 64),
          config_local_directory:$config_dir,vault_local_directory:$vault_dir,
          config_local_sha256:$config_sha,vault_local_sha256:$vault_sha,
          cleanup_marker_sha256:("0" * 64)}' > "$pending"
    sudo -n install -o 0 -g 0 -m 0600 "$pending" "$path"
    rm -f -- "$pending"
}

resource_bomb="$test_root/resource-bomb.json"
jq -n --arg evaluation_cpuset "$evaluation_cpuset" --arg management_cpuset "$management_cpuset" \
    --argjson memory_limit "$runner_memory_limit_bytes" \
    '{schema:1,kind:"sim-latency-resource-bomb-cleanup",
      evaluation_cpuset:$evaluation_cpuset,management_cpuset:$management_cpuset,
      cpu_bomb_observed_cpuset:$evaluation_cpuset,cpu_bomb_saturated_evaluation_set:true,
      memory_limit_bytes:$memory_limit,production_memory_limit:true,memory_exit_code:137,
      memory_oom_killed:true,cleanup_elapsed_ms:7,cleanup_limit_ms:10000,
      cleanup_complete:true,residual_containers:0,residual_networks:0}' > "$resource_bomb"

evaluation="$test_root/evaluation"
control_root="$test_root/control"
runtime_parent="$test_root/run/urnetwork"
install -d -m 0700 "$test_root/config/local" "$test_root/vault/local"
# The fixture hashes are the SHA-256 of an empty sorted-file manifest.
config_local_sha256="$("$SCRIPT_DIR/container/hash-local-mount.sh" "$test_root/config/local")"
vault_local_sha256="$("$SCRIPT_DIR/container/hash-local-mount.sh" "$test_root/vault/local")"
sudo -n install -d -o 0 -g 0 -m 0700 "$control_root" "$test_root/run" "$runtime_parent"
marker="$runtime_parent/containment.json"
template_marker="${marker%.json}.template-database.json"
redis_marker="${marker%.json}.redis-reset.json"
immutable_marker="${marker%.json}.immutable-reports.json"
config="$control_root/host.json"
make_evaluation "$evaluation"
make_host_config "$config" "$marker"

promotion="$(sudo -n "$PROMOTER" --host-config "$config" --evaluation-dir "$evaluation" \
    --resource-bomb-report "$resource_bomb")"
jq -e '.schema == 1 and .promoted == true and .job_id == "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa" and
       (.markers | keys) == ["cleanup","immutable_reports","redis_reset","template_database"]' \
    <<<"$promotion" >/dev/null
sudo -n jq -e \
    '.schema == 1 and .kind == "sim-latency-host-containment" and
     .default_deny_network_verified == true and .no_published_ports_verified == true and
     .scorer_network_none_verified == true and .local_only_read_only_mounts_verified == true and
     .no_production_secrets_verified == true and .docker_user_namespace_verified == true and
     (.docker_uid_map_sha256 | test("^[0-9a-f]{64}$")) and
     (.docker_gid_map_sha256 | test("^[0-9a-f]{64}$")) and .cleanup_complete == true' \
    "$marker" >/dev/null
[ "$(sudo -n sha256sum "$marker" | awk '{print $1}')" = "$(sudo -n jq -er '.cleanup_marker_sha256' "$config")" ]
[ "$(sudo -n stat -c %u "$marker")" -eq 0 ] && [ "$(sudo -n stat -c %a "$marker")" = 600 ]
for marker_spec in \
    "$template_marker:template_database_marker_sha256" \
    "$redis_marker:redis_reset_marker_sha256" \
    "$immutable_marker:immutable_reports_marker_sha256"; do
    runtime_marker="${marker_spec%%:*}"
    config_field="${marker_spec##*:}"
    [ "$(sudo -n sha256sum "$runtime_marker" | awk '{print $1}')" = \
        "$(sudo -n jq -er ".$config_field" "$config")" ]
    [ "$(sudo -n stat -c %u "$runtime_marker")" -eq 0 ] &&
        [ "$(sudo -n stat -c %a "$runtime_marker")" = 600 ]
done
sudo -n jq -e --slurpfile cleanup "$marker" --slurpfile redis "$redis_marker" \
    --slurpfile immutable "$immutable_marker" \
    '.schema == 1 and .kind == "sim-latency-template-database-reset" and .verified == true and
     .job_id == $cleanup[0].qualified_job_id and .round_id == $cleanup[0].qualified_round_id and
     .template_database_id == $cleanup[0].template_database_id and
     $redis[0].kind == "sim-latency-redis-reset" and $redis[0].verified == true and
     $redis[0].redis_generation_id == $cleanup[0].redis_generation_id and
     $immutable[0].kind == "sim-latency-immutable-reports" and
     $immutable[0].worker_result_sha256 == $cleanup[0].worker_result_sha256' \
    "$template_marker" >/dev/null

# /run is transient. Removing the complete marker directory deterministically
# reproduces a reboot and proves that one promotion restores every readiness
# marker plus its root-owned directory, rather than cleanup evidence alone.
sudo -n rm -f -- "$template_marker" "$redis_marker" "$marker" "$immutable_marker"
sudo -n rmdir -- "$runtime_parent"
promotion="$(sudo -n "$PROMOTER" --host-config "$config" --evaluation-dir "$evaluation" \
    --resource-bomb-report "$resource_bomb")"
if ! sudo -n test -d "$runtime_parent" || ! sudo -n test -f "$template_marker" ||
   ! sudo -n test -f "$redis_marker" || ! sudo -n test -f "$marker" ||
   ! sudo -n test -f "$immutable_marker"; then
    printf 'simulated reboot did not recreate runtime markers\n' >&2
    exit 1
fi
jq -e '.promoted == true and (.markers | length) == 4' <<<"$promotion" >/dev/null

bad_evaluation="$test_root/bad-evaluation"
bad_marker="$control_root/bad-containment.json"
bad_config="$control_root/bad-host.json"
cp -a "$evaluation" "$bad_evaluation"
jq '(.[] | select(.name | endswith("-runner-1")) | .mounts[0].destination) = "/runtime/config"' \
    "$bad_evaluation/evidence/runs/candidate-01/containers.json" > "$bad_evaluation/candidate.new"
mv "$bad_evaluation/candidate.new" "$bad_evaluation/evidence/runs/candidate-01/containers.json"
write_evidence_manifest "$bad_evaluation"
write_completion "$bad_evaluation"
write_worker_result "$bad_evaluation"
make_host_config "$bad_config" "$bad_marker"
if sudo -n "$PROMOTER" --host-config "$bad_config" --evaluation-dir "$bad_evaluation" \
    --resource-bomb-report "$resource_bomb" >/dev/null 2>&1; then
    printf 'unsafe parent config mount was promoted\n' >&2
    exit 1
fi
for bad_runtime_marker in "$bad_marker" "${bad_marker%.json}.template-database.json" \
    "${bad_marker%.json}.redis-reset.json" "${bad_marker%.json}.immutable-reports.json"; do
    [ ! -e "$bad_runtime_marker" ] || {
        printf 'failed promotion left a marker\n' >&2
        exit 1
    }
done

bad_userns_evaluation="$test_root/bad-userns-evaluation"
bad_userns_marker="$control_root/bad-userns-containment.json"
bad_userns_config="$control_root/bad-userns-host.json"
cp -a "$evaluation" "$bad_userns_evaluation"
jq '.remapped = false | .host_uid = .container_uid | .host_gid = .container_gid |
    .root_host_uid = 0 | .root_host_gid = 0 |
    .daemon_security_options = ["name=seccomp,profile=builtin"]' \
    "$bad_userns_evaluation/evidence/docker-id-map.json" > "$bad_userns_evaluation/id-map.new"
mv "$bad_userns_evaluation/id-map.new" "$bad_userns_evaluation/evidence/docker-id-map.json"
write_evidence_manifest "$bad_userns_evaluation"
write_completion "$bad_userns_evaluation"
write_worker_result "$bad_userns_evaluation"
make_host_config "$bad_userns_config" "$bad_userns_marker"
if sudo -n "$PROMOTER" --host-config "$bad_userns_config" \
    --evaluation-dir "$bad_userns_evaluation" \
    --resource-bomb-report "$resource_bomb" >/dev/null 2>&1; then
    printf 'identity Docker user namespace was promoted\n' >&2
    exit 1
fi
for bad_runtime_marker in "$bad_userns_marker" \
    "${bad_userns_marker%.json}.template-database.json" \
    "${bad_userns_marker%.json}.redis-reset.json" \
    "${bad_userns_marker%.json}.immutable-reports.json"; do
    [ ! -e "$bad_runtime_marker" ] || {
        printf 'failed user-namespace promotion left a marker\n' >&2
        exit 1
    }
done

printf 'host containment promotion test passed\n'
