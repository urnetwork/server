#!/usr/bin/env bash

# Trusted worker-to-Compose evaluator for competition evaluator protocol v1.
# Submission bytes choose only the canonical patch. Every image, command,
# resource ceiling, mount, and artifact name below is evaluator-owned.

set -Eeuo pipefail
umask 077
export LANG=C LC_ALL=C

readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
readonly COMPOSE_FILE="$SCRIPT_DIR/compose.yml"
readonly BUILD_SUBMISSION="$SCRIPT_DIR/build-submission.sh"
readonly PREPARE_EVALUATION_SOURCE="$SCRIPT_DIR/prepare-evaluation-source.sh"
readonly RESOURCE_BOUNDARY="$SCRIPT_DIR/resource-boundary.sh"
readonly DOCKER_ID_MAP="$SCRIPT_DIR/docker-id-map.sh"
readonly HASH_LOCAL_MOUNT="$SCRIPT_DIR/hash-local-mount.sh"
readonly RETAIN_FAILURE_EVIDENCE="$SCRIPT_DIR/retain-failure-evidence.sh"
readonly POSTGRES_INIT="$SCRIPT_DIR/postgres-init.sh"
readonly TIMEOUT_BUDGET="$SCRIPT_DIR/timeout-budget.sh"
readonly EMPTY_PATCH_SHA256=e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
readonly MAX_REQUEST_BYTES=$((2 * 1024 * 1024))
readonly MAX_PROVIDERS_BYTES=$((1024 * 1024 * 1024))
readonly MAX_BUILD_LOG_BYTES=$((4 * 1024 * 1024))
readonly EVIDENCE_WORK_LIMIT=32g
readonly EVIDENCE_WORK_BYTES=34359738368

# Season-frozen container ceilings. Host qualification verifies that their sum
# plus the host reserve fits the physical box.
readonly RUNNER_MEMORY_LIMIT=72g
readonly RUNNER_MEMORY_BYTES=77309411328
readonly RUNNER_PIDS_LIMIT=65536
readonly RUNNER_TMP_LIMIT=2g
readonly RUNNER_WORK_LIMIT=8g
readonly MIGRATOR_MEMORY_LIMIT=4g
readonly MIGRATOR_PIDS_LIMIT=4096
readonly MIGRATOR_TMP_LIMIT=1g
readonly POSTGRES_MEMORY_LIMIT=16g
readonly POSTGRES_MEMORY_BYTES=17179869184
readonly POSTGRES_PIDS_LIMIT=4096
readonly POSTGRES_DATA_LIMIT=12g
readonly POSTGRES_MAX_CONNECTIONS=512
readonly POSTGRES_SHARED_BUFFERS=2GB
readonly REDIS_MEMORY_LIMIT=8g
readonly REDIS_MEMORY_BYTES=8589934592
readonly REDIS_PIDS_LIMIT=4096
readonly REDIS_DATA_LIMIT=6g
readonly REDIS_MAX_CLIENTS=32768
readonly REDIS_TCP_BACKLOG=65535
readonly SCORER_MEMORY_LIMIT=4g
readonly SCORER_MEMORY_BYTES=4294967296
readonly SCORER_PIDS_LIMIT=1024
readonly SCORER_TMP_LIMIT=1g
readonly EVALUATION_NOFILE=1048576
readonly ACTIVE_EVALUATION_MEMORY_BYTES=103079215104
readonly MINIMUM_MANAGEMENT_MEMORY_RESERVE_BYTES=25769803776

request_path=""
result_path=""
artifact_dir=""
work_dir=""
active_project=""
active_compose_env=""
active_sampler_pid=""
active_work_mount=""
active_build_log_pid=""
active_build_log_pipe=""
active_source_root=""
cleanup_complete=false
failure_line=0
worker_uid="$(id -u)"
worker_gid="$(id -g)"

log() { printf '[competition-evaluator] %s\n' "$*" >&2; }
die() {
    failure_line="${BASH_LINENO[0]:-0}"
    log "ERROR: $*"
    exit 1
}
require_command() { command -v "$1" >/dev/null 2>&1 || die "required command missing: $1"; }
sha256_file() { sha256sum "$1" | awk '{print $1}'; }
file_bytes() { stat -c '%s' "$1"; }

local_tree_sha256() {
    "$HASH_LOCAL_MOUNT" "$1"
}

authenticate_local_mounts() {
    [ "$(local_tree_sha256 "$config_local_directory")" = "$config_local_sha256" ] ||
        die "direct config/local content does not match the frozen digest"
    [ "$(local_tree_sha256 "$vault_local_directory")" = "$vault_local_sha256" ] ||
        die "direct vault/local content does not match the frozen digest"
}

on_error() {
    local line="$1" rc="$2"
    if [ "$BASH_SUBSHELL" -eq 0 ]; then
        failure_line="$line"
        log "ERROR: evaluator command failed at line $line (exit $rc)"
    fi
    return "$rc"
}

usage() {
    printf '%s\n' 'usage: competition-evaluator --request ABSOLUTE_REQUEST --result ABSOLUTE_RESULT'
}

while [ "$#" -gt 0 ]; do
    case "$1" in
        --request|--result)
            [ "$#" -ge 2 ] || { usage >&2; exit 2; }
            case "$1" in
                --request) request_path="$2" ;;
                --result) result_path="$2" ;;
            esac
            shift 2
            ;;
        -h|--help)
            usage
            exit 0
            ;;
        *)
            usage >&2
            exit 2
            ;;
    esac
done

cleanup() {
    local rc=$?
    set +e
    if [ -n "${active_build_log_pid:-}" ]; then
        kill "$active_build_log_pid" >/dev/null 2>&1 || true
        wait "$active_build_log_pid" >/dev/null 2>&1 || true
        active_build_log_pid=""
    fi
    if [ -n "${active_build_log_pipe:-}" ]; then
        rm -f -- "$active_build_log_pipe"
        active_build_log_pipe=""
    fi
    if [ -n "${active_sampler_pid:-}" ]; then
        kill "$active_sampler_pid" >/dev/null 2>&1 || true
        wait "$active_sampler_pid" >/dev/null 2>&1 || true
        active_sampler_pid=""
    fi
    if [ -n "${active_project:-}" ] && [ -n "${active_compose_env:-}" ] && [ -f "$active_compose_env" ]; then
        compose_with "$active_project" "$active_compose_env" --profile run --profile score \
            down --volumes --remove-orphans >/dev/null 2>&1 || true
    fi
    if [ -n "${job_id:-}" ]; then
        while IFS= read -r container_id; do
            [ -n "$container_id" ] && sudo -n docker rm -f "$container_id" >/dev/null 2>&1 || true
        done < <(sudo -n docker ps -aq --filter "label=com.urnetwork.competition.job-id=$job_id" 2>/dev/null || true)
        while IFS= read -r network_id; do
            [ -n "$network_id" ] && sudo -n docker network rm "$network_id" >/dev/null 2>&1 || true
        done < <(sudo -n docker network ls -q --filter "label=com.urnetwork.competition.job-id=$job_id" 2>/dev/null || true)
    fi
    if [ "$rc" -ne 0 ] && [ -n "${active_work_mount:-}" ] &&
       [ "$active_work_mount" = "${artifact_dir:-}/.evidence-runtime" ] &&
       mountpoint -q "$active_work_mount" && [ -x "$RETAIN_FAILURE_EVIDENCE" ]; then
        if env \
            FAILURE_JOB_ID="${job_id:-}" \
            FAILURE_ROUND_ID="${round_id:-}" \
            FAILURE_ATTEMPT="${attempt:-}" \
            FAILURE_EXIT_CODE="$rc" \
            FAILURE_EVALUATOR_LINE="$failure_line" \
            "$RETAIN_FAILURE_EVIDENCE" \
                "$active_work_mount" "$artifact_dir/failed-evidence"; then
            log "retained sanitized failure evidence: $artifact_dir/failed-evidence"
        else
            log "ERROR: could not retain sanitized failure evidence"
        fi
    fi
    if [ -n "${active_work_mount:-}" ] && mountpoint -q "$active_work_mount"; then
        sudo -n umount "$active_work_mount" >/dev/null 2>&1 || true
    fi
    if [ -n "${active_work_mount:-}" ] && [ -d "$active_work_mount" ]; then
        rmdir "$active_work_mount" >/dev/null 2>&1 || true
    fi
    if [ -n "${artifact_dir:-}" ] && [ -d "$artifact_dir" ]; then
        sudo -n chown -R "$worker_uid:$worker_gid" "$artifact_dir" >/dev/null 2>&1 || true
    fi
    exit "$rc"
}
trap 'on_error "$LINENO" "$?"' ERR
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

for command in awk cat cp date df find findmnt git head install jq mkfifo mount mountpoint openssl realpath sed sha256sum sort stat sudo sync tail taskset umount; do
    require_command "$command"
done
sudo -n docker info >/dev/null
[ -x "$BUILD_SUBMISSION" ] || die "trusted submission builder is not executable"
[ -x "$PREPARE_EVALUATION_SOURCE" ] || die "trusted evaluation source preparer is not executable"
[ -x "$RESOURCE_BOUNDARY" ] || die "trusted resource boundary is not executable"
[ -x "$DOCKER_ID_MAP" ] || die "trusted Docker id-map resolver is not executable"
[ -x "$HASH_LOCAL_MOUNT" ] || die "trusted local-mount digest helper is not executable"
[ -x "$RETAIN_FAILURE_EVIDENCE" ] || die "trusted failure evidence retainer is not executable"
[ -f "$POSTGRES_INIT" ] || die "trusted PostgreSQL initializer is missing"
[ -x "$TIMEOUT_BUDGET" ] || die "trusted timeout-budget calculator is not executable"
[ -f "$COMPOSE_FILE" ] || die "trusted Compose definition is missing"

[[ "$request_path" = /* && "$result_path" = /* ]] || die "request and result paths must be absolute"
[ -f "$request_path" ] && [ ! -L "$request_path" ] || die "request must be a regular non-symlink file"
[ ! -e "$result_path" ] || die "result path already exists"
[ "$(file_bytes "$request_path")" -le "$MAX_REQUEST_BYTES" ] || die "request is oversized"
request_path="$(realpath -e "$request_path")"
artifact_dir="$(dirname "$request_path")"
[ "$result_path" = "$artifact_dir/worker-result.json" ] || die "result path is outside the attempt directory"

readonly request_keys='["api_image_digest","artifact_directory","attempt","base_sha","competition_id","config_local_directory","evaluation_policy","evaluator_image_digest","job_id","patch_path","patch_policy","patch_sha256","providers_path","providers_sha256","round_id","round_seed_hex","schema","scorer_version","source_epoch","vault_local_directory","worker_image_digest"]'
readonly patch_policy_keys='["allowed_paths","forbidden_paths","max_patch_bytes"]'
readonly evaluation_policy_keys='["announce_timeout_ms","api_port","arrivals_per_minute","client_pool_size","client_warmup_timeout_ms","config_local_sha256","duration_ms","exchange_hosts","fleet_shards","hardware_id","host_qualification_sha256","impairment_enabled","pipeline_interval_ms","prewarm_ms","provider_count","quality_window_size","queue_limit","ramp_ms","replicates","request_timeout_ms","score_timeout_seconds","scorer_sha256","settle_ms","simulator_sha256","site_listen","takeover_margin","test_timeout_ms","vault_local_sha256"]'
jq -e \
    --argjson request_keys "$request_keys" \
    --argjson patch_policy_keys "$patch_policy_keys" \
    --argjson evaluation_policy_keys "$evaluation_policy_keys" \
    'type == "object" and (.schema == 1) and
     ((keys | sort) == $request_keys) and
     (.patch_policy | type == "object" and ((keys | sort) == $patch_policy_keys)) and
     (.evaluation_policy | type == "object" and ((keys | sort) == $evaluation_policy_keys))' \
    "$request_path" >/dev/null || die "request schema or fields are invalid"

job_id="$(jq -er '.job_id' "$request_path")"
round_id="$(jq -er '.round_id' "$request_path")"
source_epoch="$(jq -er '.source_epoch' "$request_path")"
attempt="$(jq -er '.attempt' "$request_path")"
competition_id="$(jq -er '.competition_id' "$request_path")"
base_sha="$(jq -er '.base_sha' "$request_path")"
base_image="$(jq -er '.evaluator_image_digest' "$request_path")"
api_control_image="$(jq -er '.api_image_digest' "$request_path")"
worker_control_image="$(jq -er '.worker_image_digest' "$request_path")"
scorer_version="$(jq -er '.scorer_version' "$request_path")"
providers_path="$(jq -er '.providers_path' "$request_path")"
providers_sha256="$(jq -er '.providers_sha256' "$request_path")"
patch_path="$(jq -er '.patch_path' "$request_path")"
patch_sha256="$(jq -er '.patch_sha256' "$request_path")"
request_artifact_dir="$(jq -er '.artifact_directory' "$request_path")"
config_local_directory="$(jq -er '.config_local_directory' "$request_path")"
vault_local_directory="$(jq -er '.vault_local_directory' "$request_path")"
config_local_sha256="$(jq -er '.evaluation_policy.config_local_sha256' "$request_path")"
vault_local_sha256="$(jq -er '.evaluation_policy.vault_local_sha256' "$request_path")"
replicates="$(jq -er '.evaluation_policy.replicates' "$request_path")"

[[ "$job_id" =~ ^[0-9a-f-]{36}$ ]] || die "job id is invalid"
[[ "$round_id" =~ ^[0-9a-f-]{36}$ ]] || die "round id is invalid"
[[ "$source_epoch" =~ ^[0-6]$ ]] || die "source epoch is invalid"
[[ "$attempt" =~ ^[1-9][0-9]*$ ]] || die "attempt is invalid"
[[ "$competition_id" =~ ^[A-Za-z0-9._-]{1,128}$ ]] || die "competition id is invalid"
[[ "$base_sha" =~ ^[0-9a-f]{40}$ ]] || die "base SHA is invalid"
[[ "$base_image" =~ ^sha256:[0-9a-f]{64}$ ]] || die "base image id is invalid"
[[ "$api_control_image" =~ ^sha256:[0-9a-f]{64}$ ]] || die "API control image id is invalid"
[[ "$worker_control_image" =~ ^sha256:[0-9a-f]{64}$ ]] || die "worker control image id is invalid"
[ "$scorer_version" = sim-latency-score/1 ] || die "scorer version is invalid"
[[ "$providers_sha256" =~ ^[0-9a-f]{64}$ ]] || die "providers SHA-256 is invalid"
[[ "$patch_sha256" =~ ^[0-9a-f]{64}$ ]] || die "patch SHA-256 is invalid"
[[ "$config_local_sha256" =~ ^[0-9a-f]{64}$ ]] || die "config/local SHA-256 is invalid"
[[ "$vault_local_sha256" =~ ^[0-9a-f]{64}$ ]] || die "vault/local SHA-256 is invalid"
[[ "$replicates" =~ ^[13579]$ ]] || die "replicate count must be odd and in 1..9"
[ "$request_artifact_dir" = "$artifact_dir" ] || die "request artifact directory identity mismatch"
[ "$patch_path" = "$artifact_dir/canonical.patch" ] || die "patch path identity mismatch"
[ -f "$patch_path" ] && [ ! -L "$patch_path" ] || die "canonical patch is missing or unsafe"
[ "$(sha256_file "$patch_path")" = "$patch_sha256" ] || die "canonical patch hash mismatch"
[ -f "$providers_path" ] && [ ! -L "$providers_path" ] || die "providers file is missing or unsafe"
[ "$(file_bytes "$providers_path")" -le "$MAX_PROVIDERS_BYTES" ] || die "providers file is oversized"
[ "$(sha256_file "$providers_path")" = "$providers_sha256" ] || die "providers file hash mismatch"
jq -e '.round_seed_hex | type == "string" and test("^[0-9a-f]{64}$")' "$request_path" >/dev/null ||
    die "hidden round seed is malformed"

for local_directory in "$config_local_directory" "$vault_local_directory"; do
    [[ "$local_directory" = /* ]] || die "direct local mount path must be absolute"
    [ "$local_directory" = "$(realpath -e "$local_directory")" ] ||
        die "direct local mount path is not canonical"
done
[ "${config_local_directory##*/}" = local ] &&
    [ "${config_local_directory%/*}" != "$config_local_directory" ] &&
    [ "${config_local_directory%/*}" = "${config_local_directory%/config/local}/config" ] ||
    die "config mount is not the exact config/local leaf"
[ "${vault_local_directory##*/}" = local ] &&
    [ "${vault_local_directory%/*}" != "$vault_local_directory" ] &&
    [ "${vault_local_directory%/*}" = "${vault_local_directory%/vault/local}/vault" ] ||
    die "vault mount is not the exact vault/local leaf"
authenticate_local_mounts

resource_boundary_json="$($RESOURCE_BOUNDARY)" || die "host resource boundary is invalid"
jq -e \
    --arg runner_memory_limit "$RUNNER_MEMORY_LIMIT" \
    --argjson runner_memory_bytes "$RUNNER_MEMORY_BYTES" \
    --argjson postgres_memory_bytes "$POSTGRES_MEMORY_BYTES" \
    --argjson redis_memory_bytes "$REDIS_MEMORY_BYTES" \
    --argjson active_memory_bytes "$ACTIVE_EVALUATION_MEMORY_BYTES" \
    --argjson reserve_bytes "$MINIMUM_MANAGEMENT_MEMORY_RESERVE_BYTES" \
    '.schema == 1 and .kind == "sim-latency-resource-boundary" and
     .evaluation_physical_core_count == 10 and
     .management_physical_core_count == 2 and
     .evaluation_cpuset != "" and .management_cpuset != "" and
     .disjoint_cpu_sets == true and .memory_capacity_passed == true and
     .runner_memory_limit == $runner_memory_limit and
     .runner_memory_limit_bytes == $runner_memory_bytes and
     .postgres_memory_limit_bytes == $postgres_memory_bytes and
     .redis_memory_limit_bytes == $redis_memory_bytes and
     .active_memory_limit_bytes == $active_memory_bytes and
     .minimum_management_memory_reserve_bytes == $reserve_bytes and
     .capacity_reserve_bytes >= $reserve_bytes' \
    <<<"$resource_boundary_json" >/dev/null || die "resource boundary does not match the frozen evaluator policy"
cpuset="$(jq -er '.evaluation_cpuset' <<<"$resource_boundary_json")"
management_cpuset="$(jq -er '.management_cpuset' <<<"$resource_boundary_json")"
evaluation_cpu_count="$(jq -er '.evaluation_physical_core_count' <<<"$resource_boundary_json")"
expected_management_affinity="$(taskset -c "$management_cpuset" awk -F: \
    '$1 == "Cpus_allowed_list" {gsub(/[[:space:]]/, "", $2); print $2}' /proc/self/status)"
if [ "${COMPETITION_MANAGEMENT_CPUSET_APPLIED:-}" != "$management_cpuset" ]; then
    exec env COMPETITION_MANAGEMENT_CPUSET_APPLIED="$management_cpuset" \
        taskset -c "$management_cpuset" "$(realpath -e "$0")" \
        --request "$request_path" --result "$result_path"
fi
actual_management_affinity="$(awk -F: \
    '$1 == "Cpus_allowed_list" {gsub(/[[:space:]]/, "", $2); print $2}' /proc/self/status)"
[ "$actual_management_affinity" = "$expected_management_affinity" ] ||
    die "evaluator worker is not confined to the dedicated management CPUs"

work_dir="$artifact_dir/.evidence-runtime"
[ ! -e "$work_dir" ] || die "runtime evidence directory already exists"
install -d -m 0700 "$work_dir"
sudo -n mount -t tmpfs \
    -o "size=$EVIDENCE_WORK_LIMIT,mode=0700,uid=$worker_uid,gid=$worker_gid,nosuid,nodev,noexec" \
    "urnetwork-evidence-${job_id//-/}-$attempt" "$work_dir"
active_work_mount="$work_dir"
[ "$(stat -f -c '%T' "$work_dir")" = tmpfs ] || die "runtime evidence filesystem is not tmpfs"
work_capacity="$(df -B1 --output=size "$work_dir" | awk 'NR == 2 {print $1}')"
[ "$work_capacity" = "$EVIDENCE_WORK_BYTES" ] || die "runtime evidence byte limit is not frozen"
work_mount_options="$(findmnt -n -o OPTIONS --target "$work_dir")"
for required_option in nosuid nodev noexec; do
    case ",$work_mount_options," in
        *",$required_option,"*) ;;
        *) die "runtime evidence mount is missing $required_option" ;;
    esac
done
install -d -m 0700 "$work_dir/input" "$work_dir/runs" \
    "$work_dir/scorer-input" "$work_dir/score-output"
jq -n --arg filesystem tmpfs --arg mount_options "$work_mount_options" \
    --argjson limit_bytes "$EVIDENCE_WORK_BYTES" \
    '{schema:1,kind:"sim-latency-evidence-quota",filesystem:$filesystem,
      mount_options:$mount_options,limit_bytes:$limit_bytes,hard_limit:true}' \
    > "$work_dir/evidence-quota.json"
chmod 0400 "$work_dir/evidence-quota.json"
make_local_mount_evidence() {
    jq -n --arg config_local_sha256 "$config_local_sha256" \
        --arg vault_local_sha256 "$vault_local_sha256" \
        '{schema:1,kind:"sim-latency-local-mounts",direct_bind:true,
          read_only:true,parent_mounts:false,all_main_site_absent:true,
          config:{target:"/runtime/config/local",sha256:$config_local_sha256},
          vault:{target:"/runtime/vault/local",sha256:$vault_local_sha256}}' \
        > "$work_dir/local-mounts.json"
    chmod 0400 "$work_dir/local-mounts.json"
}
make_local_mount_evidence
printf '%s\n' "$resource_boundary_json" > "$work_dir/resource-boundary.json"
chmod 0400 "$work_dir/resource-boundary.json"
policy_path="$work_dir/policy.json"
jq -S '.patch_policy' "$request_path" > "$policy_path"
chmod 0400 "$policy_path"
policy_sha256="$(sha256_file "$policy_path")"
builder_sha256="$(sha256_file "$SCRIPT_DIR/Dockerfile.submission")"
base_image_id="$(sudo -n docker image inspect --format '{{.Id}}' "$base_image")" || die "base image is unavailable offline"
[ "$base_image_id" = "$base_image" ] || die "base image id does not match frozen digest"
docker_id_map_json="$($DOCKER_ID_MAP --image "$base_image_id" --uid 65532 --gid 65532)" ||
    die "could not resolve the live Docker user-namespace map"
jq -e --arg image_id "$base_image_id" \
    '.schema == 1 and .kind == "sim-latency-docker-id-map" and
     .image_id == $image_id and .container_uid == 65532 and .container_gid == 65532 and
     (.host_uid | type == "number" and . >= 0) and
     (.host_gid | type == "number" and . >= 0) and
     (.root_host_uid | type == "number" and . >= 0) and
     (.root_host_gid | type == "number" and . >= 0) and
     (.uid_map_sha256 | test("^[0-9a-f]{64}$")) and
     (.gid_map_sha256 | test("^[0-9a-f]{64}$")) and
     (.remapped | type == "boolean") and (.daemon_security_options | type == "array")' \
    <<<"$docker_id_map_json" >/dev/null || die "Docker id-map evidence is invalid"
container_host_uid="$(jq -er '.host_uid' <<<"$docker_id_map_json")"
container_host_gid="$(jq -er '.host_gid' <<<"$docker_id_map_json")"
printf '%s\n' "$docker_id_map_json" > "$work_dir/docker-id-map.json"
chmod 0400 "$work_dir/docker-id-map.json"
sudo -n install -o "$container_host_uid" -g "$container_host_gid" -m 0400 \
    "$providers_path" "$work_dir/input/providers.yml"
sudo -n install -o "$container_host_uid" -g "$container_host_gid" -m 0400 \
    "$providers_path" "$work_dir/scorer-input/providers.yml"
sudo -n chown "$container_host_uid:$container_host_gid" "$work_dir/input" "$work_dir/scorer-input"
sudo -n chmod 0500 "$work_dir/input" "$work_dir/scorer-input"

base_build_ref="urnetwork/sim-latency-evaluator-base-id:${base_image_id#sha256:}"
if sudo -n docker image inspect "$base_build_ref" >/dev/null 2>&1; then
    [ "$(sudo -n docker image inspect --format '{{.Id}}' "$base_build_ref")" = "$base_image_id" ] ||
        die "daemon-local base alias points at the wrong image"
else
    sudo -n docker image tag "$base_image_id" "$base_build_ref"
fi
image_key="$(printf '%s\000%s\000%s\000%s' \
    "$base_image_id" "$patch_sha256" "$policy_sha256" "$builder_sha256" | sha256sum | awk '{print $1}')"
base_identity="$(sudo -n docker run --rm --network none --read-only --cap-drop ALL \
    --security-opt no-new-privileges:true "$base_image_id" identity)"
jq -e --arg base_sha "$base_sha" --arg empty_patch "$EMPTY_PATCH_SHA256" --argjson source_epoch "$source_epoch" \
    '.schema == 1 and .image_kind == "evaluator-base" and
     .base_sha == $base_sha and .build_sha == $base_sha and
     .source_epoch == $source_epoch and
     .patch_sha256 == $empty_patch and (.simulator_sha256 | test("^[0-9a-f]{64}$"))' \
    <<<"$base_identity" >/dev/null || die "base image identity is invalid"
base_simulator_sha256="$(jq -er '.simulator_sha256' <<<"$base_identity")"
policy_scorer_sha256="$(jq -er '.evaluation_policy.scorer_sha256' "$request_path")"
policy_simulator_sha256="$(jq -er '.evaluation_policy.simulator_sha256' "$request_path")"
[ "$policy_scorer_sha256" = "$base_simulator_sha256" ] || die "frozen scorer digest does not match pristine image"
[ "$policy_simulator_sha256" = "$base_simulator_sha256" ] || die "frozen simulator digest does not match pristine image"

# Every attempt gets independent baseline and candidate source checkouts. They
# are copied from the authenticated base image into this attempt's bounded
# tmpfs, never from the API/worker repository worktrees. The preparer creates
# a local sim-latency branch at each source-lock commit; only the candidate
# checkout is subsequently patched by the trusted builder.
active_source_root="$(mktemp -d "$work_dir/evaluation-sources.XXXXXXXX")"
baseline_source_root="$active_source_root/baseline"
candidate_source_root="$active_source_root/candidate"
baseline_source_identity="$($PREPARE_EVALUATION_SOURCE \
    --base-image "$base_image_id" --destination "$baseline_source_root")" ||
    die "could not prepare the temporary baseline source checkout"
candidate_source_identity="$($PREPARE_EVALUATION_SOURCE \
    --base-image "$base_image_id" --destination "$candidate_source_root")" ||
    die "could not prepare the temporary candidate source checkout"
for prepared_identity in "$baseline_source_identity" "$candidate_source_identity"; do
    jq -e --arg base_image_id "$base_image_id" --arg base_sha "$base_sha" \
        --argjson source_epoch "$source_epoch" \
        '.schema == 1 and .kind == "sim-latency-evaluation-source" and
         .temporary == true and .base_image_id == $base_image_id and
         .base_sha == $base_sha and .source_epoch == $source_epoch and
         .branch == "sim-latency" and .candidate_patch_sha256 == null' \
        <<<"$prepared_identity" >/dev/null || die "temporary evaluation source identity is invalid"
done
source_evidence_path="$work_dir/evaluation-sources.json"
write_source_evidence() {
    local cleanup_complete="$1"
    local pending="${source_evidence_path}.new"
    jq -nS \
        --argjson baseline "$(jq -c . "$baseline_source_root/.evaluation-source.json")" \
        --argjson candidate "$(jq -c . "$candidate_source_root/.evaluation-source.json")" \
        --argjson cleanup_complete "$cleanup_complete" \
        '{schema:1,kind:"sim-latency-temporary-evaluation-sources",
          origin:"authenticated_evaluator_image",host_repositories_used:false,
          runtime_target:"/workspace",read_only_mount:true,
          baseline:$baseline,candidate:$candidate,
          cleanup_complete:$cleanup_complete}' > "$pending"
    chmod 0400 "$pending"
    mv -f -- "$pending" "$source_evidence_path"
}
write_source_evidence false

kernel_release="$(uname -r)"
microcode_revision="$(awk -F: '$1 ~ /^microcode/ {gsub(/[[:space:]]/, "", $2); print $2}' /proc/cpuinfo | sort -u | paste -sd, -)"
[ -n "$microcode_revision" ] || die "microcode identity is unavailable"
hardware_id="$(jq -er '.evaluation_policy.hardware_id' "$request_path")"
qualification_sha256="$(jq -er '.evaluation_policy.host_qualification_sha256' "$request_path")"
[[ "$qualification_sha256" =~ ^[0-9a-f]{64}$ ]] || die "qualification digest is invalid"

duration_ms="$(jq -er '.evaluation_policy.duration_ms' "$request_path")"
request_timeout_ms="$(jq -er '.evaluation_policy.request_timeout_ms' "$request_path")"
ramp_ms="$(jq -er '.evaluation_policy.ramp_ms' "$request_path")"
prewarm_ms="$(jq -er '.evaluation_policy.prewarm_ms' "$request_path")"
settle_ms="$(jq -er '.evaluation_policy.settle_ms' "$request_path")"
client_warmup_timeout_ms="$(jq -er '.evaluation_policy.client_warmup_timeout_ms' "$request_path")"
pipeline_interval_ms="$(jq -er '.evaluation_policy.pipeline_interval_ms' "$request_path")"
test_timeout_ms="$(jq -er '.evaluation_policy.test_timeout_ms' "$request_path")"
announce_timeout_ms="$(jq -er '.evaluation_policy.announce_timeout_ms' "$request_path")"
fleet_shards="$(jq -er '.evaluation_policy.fleet_shards' "$request_path")"
exchange_hosts="$(jq -er '.evaluation_policy.exchange_hosts' "$request_path")"
site_listen="$(jq -er '.evaluation_policy.site_listen' "$request_path")"
api_port="$(jq -er '.evaluation_policy.api_port' "$request_path")"
impairment_enabled="$(jq -r '.evaluation_policy.impairment_enabled' "$request_path")"
takeover_margin="$(jq -er '.evaluation_policy.takeover_margin' "$request_path")"
queue_limit="$(jq -er '.evaluation_policy.queue_limit' "$request_path")"
score_timeout_seconds="$(jq -er '.evaluation_policy.score_timeout_seconds' "$request_path")"
for numeric in client_warmup_timeout_ms duration_ms request_timeout_ms pipeline_interval_ms test_timeout_ms announce_timeout_ms exchange_hosts api_port score_timeout_seconds; do
    [[ "${!numeric}" =~ ^[1-9][0-9]*$ ]] || die "invalid positive policy field: $numeric"
done
for numeric in ramp_ms prewarm_ms settle_ms fleet_shards; do
    [[ "${!numeric}" =~ ^[0-9]+$ ]] || die "invalid nonnegative policy field: $numeric"
done
[ "$impairment_enabled" = true ] || [ "$impairment_enabled" = false ] || die "impairment policy is invalid"
[ "$queue_limit" -eq 0 ] || die "queue limit is not the frozen unbounded sentinel"
if [ "$impairment_enabled" = true ]; then no_impair=no; else no_impair=yes; fi
wall_timeout_seconds="$(
    "$TIMEOUT_BUDGET" stage \
        "$ramp_ms" "$settle_ms" "$client_warmup_timeout_ms" "$duration_ms" "$request_timeout_ms"
)" || die "could not calculate the stage timeout budget"
[ "$score_timeout_seconds" -eq 10800 ] || die "score timeout is not the frozen three-hour submission limit"

compose_with() {
    local project="$1" env_file="$2"
    shift 2
    sudo -n docker compose --env-file "$env_file" --file "$COMPOSE_FILE" \
        --project-name "$project" "$@"
}

new_secret() {
    openssl rand -hex 32
}

write_runner_env() {
    local path="$1" evaluation_id="$2" build_sha="$3" simulator_sha="$4" image_id="$5" candidate_patch_sha="$6"
    local pending="${path}.new"
    [ ! -e "$pending" ] || die "pending runner environment already exists"
    printf '%s\n' \
        "EVALUATION_DB_PASSWORD=$stage_db_password" \
        "EVALUATION_REDIS_PASSWORD=$stage_redis_password" \
        "APEX_BASE_SHA=$base_sha" \
        "APEX_BUILD_SHA=$build_sha" \
        'APEX_SCORER_BIN=/opt/urnetwork/bin/sim-latency' \
        "APEX_SCORER_SHA256=$simulator_sha" \
        'APEX_SIM_BIN=/opt/urnetwork/bin/sim-latency' \
        "APEX_SIM_SHA256=$simulator_sha" \
        'APEX_PROVIDERS_FILE=/input/providers.yml' \
        "APEX_PROVIDERS_SHA256=$providers_sha256" \
        'APEX_ARTIFACT_ROOT=/artifacts' \
        "APEX_EVALUATION_ID=$evaluation_id" \
        "APEX_EPOCH=$source_epoch" \
        "APEX_API_IMAGE_DIGEST=$image_id" \
        "APEX_HARDWARE_ID=$hardware_id" \
        "APEX_HOST_QUALIFICATION_SHA256=$qualification_sha256" \
        "APEX_KERNEL_RELEASE=$kernel_release" \
        "APEX_MICROCODE_REVISION=$microcode_revision" \
        'APEX_PATCH_FILE=/opt/urnetwork/submission/canonical.patch' \
        "APEX_PATCH_SHA256=$candidate_patch_sha" \
        "APEX_CPU_COUNT=$evaluation_cpu_count" \
        "APEX_DURATION=${duration_ms}ms" \
        "APEX_REQUEST_TIMEOUT=${request_timeout_ms}ms" \
        "APEX_RAMP=${ramp_ms}ms" \
        "APEX_PREWARM=${prewarm_ms}ms" \
        "APEX_SETTLE=${settle_ms}ms" \
        "APEX_CLIENT_WARMUP_TIMEOUT=${client_warmup_timeout_ms}ms" \
        "APEX_FLEET_SHARDS=$fleet_shards" \
        "APEX_HOSTS=$exchange_hosts" \
        "APEX_PIPELINE_INTERVAL=${pipeline_interval_ms}ms" \
        "APEX_TEST_TIMEOUT=${test_timeout_ms}ms" \
        "APEX_ANNOUNCE_TIMEOUT=${announce_timeout_ms}ms" \
        "APEX_SITE_LISTEN=$site_listen" \
        "APEX_API_PORT=$api_port" \
        "APEX_NO_IMPAIR=$no_impair" \
        "APEX_WALL_TIMEOUT=${wall_timeout_seconds}s" \
        'APEX_KILL_AFTER=30s' \
        'APEX_CALIBRATION_ACCEPTED=yes' \
        > "$pending"
    chmod 0400 "$pending"
    mv -f -- "$pending" "$path"
}

write_compose_env() {
    local path="$1" project="$2" stage="$3" image="$4" runner_env="$5" input="$6" output="$7" cgroup_parent="$8" source="$9"
    local pending="${path}.new"
    [ ! -e "$pending" ] || die "pending Compose environment already exists"
    printf '%s\n' \
        "COMPOSE_PROJECT_NAME=$project" \
        "EVALUATION_JOB_ID=$job_id" \
        "EVALUATION_ROUND_ID=$round_id" \
        "EVALUATION_STAGE=$stage" \
        'EVALUATION_ACTION=run' \
        "EVALUATION_CGROUP_PARENT=$cgroup_parent" \
        "EVALUATION_CPUSET=$cpuset" \
        "EVALUATION_IMAGE=$image" \
        "EVALUATOR_BASE_IMAGE=$base_image_id" \
        "EVALUATION_ENV_FILE=$runner_env" \
        "EVALUATION_SOURCE_DIR=$source" \
        "EVALUATION_CONFIG_LOCAL_DIR=$config_local_directory" \
        "EVALUATION_VAULT_LOCAL_DIR=$vault_local_directory" \
        "EVALUATION_INPUT_DIR=$input" \
        "EVALUATION_OUTPUT_DIR=$output" \
        "EVALUATION_POSTGRES_INIT=$POSTGRES_INIT" \
        "RUNNER_MEMORY_LIMIT=$RUNNER_MEMORY_LIMIT" \
        "RUNNER_PIDS_LIMIT=$RUNNER_PIDS_LIMIT" \
        "RUNNER_TMP_LIMIT=$RUNNER_TMP_LIMIT" \
        "RUNNER_WORK_LIMIT=$RUNNER_WORK_LIMIT" \
        "MIGRATOR_MEMORY_LIMIT=$MIGRATOR_MEMORY_LIMIT" \
        "MIGRATOR_PIDS_LIMIT=$MIGRATOR_PIDS_LIMIT" \
        "MIGRATOR_TMP_LIMIT=$MIGRATOR_TMP_LIMIT" \
        "POSTGRES_MEMORY_LIMIT=$POSTGRES_MEMORY_LIMIT" \
        "POSTGRES_PIDS_LIMIT=$POSTGRES_PIDS_LIMIT" \
        "POSTGRES_DATA_LIMIT=$POSTGRES_DATA_LIMIT" \
        "POSTGRES_MAX_CONNECTIONS=$POSTGRES_MAX_CONNECTIONS" \
        "POSTGRES_SHARED_BUFFERS=$POSTGRES_SHARED_BUFFERS" \
        "REDIS_MEMORY_LIMIT=$REDIS_MEMORY_LIMIT" \
        "REDIS_PIDS_LIMIT=$REDIS_PIDS_LIMIT" \
        "REDIS_DATA_LIMIT=$REDIS_DATA_LIMIT" \
        "REDIS_MAX_CLIENTS=$REDIS_MAX_CLIENTS" \
        "REDIS_TCP_BACKLOG=$REDIS_TCP_BACKLOG" \
        "SCORER_MEMORY_LIMIT=$SCORER_MEMORY_LIMIT" \
        "SCORER_PIDS_LIMIT=$SCORER_PIDS_LIMIT" \
        "SCORER_TMP_LIMIT=$SCORER_TMP_LIMIT" \
        "EVALUATION_NOFILE=$EVALUATION_NOFILE" \
        'EVALUATION_DB_USER=bringyour' \
        "EVALUATION_DB_ADMIN_PASSWORD=$stage_db_admin_password" \
        "EVALUATION_DB_PASSWORD=$stage_db_password" \
        'EVALUATION_DB_NAME=bringyour' \
        "EVALUATION_REDIS_PASSWORD=$stage_redis_password" \
        'EVALUATION_DB_MIN_CONNECTIONS=4' \
        'EVALUATION_DB_MAX_CONNECTIONS=32' \
        'EVALUATION_REDIS_MIN_CONNECTIONS=4' \
        'EVALUATION_REDIS_MAX_CONNECTIONS=32' \
        > "$pending"
    chmod 0400 "$pending"
    mv -f -- "$pending" "$path"
}

validate_output_tree() {
    local root="$1"
    [ -z "$(sudo -n find "$root" -mindepth 1 ! -type f ! -type d -print -quit)" ] ||
        die "candidate output contains a non-regular entry"
}

seal_evaluation_source() {
    local root="$1" expected_server="$2" expected_patch="$3"
    [ "$root" = "$baseline_source_root" ] || [ "$root" = "$candidate_source_root" ] ||
        die "refusing to seal an unexpected source directory"
    for repository in server connect sdk proxy; do
        local repository_root="$root/$repository"
        local expected_commit
        expected_commit="$(jq -er --arg repository "$repository" \
            '.repositories[$repository]' "$root/.evaluation-source.json")"
        [ "$(git -C "$repository_root" symbolic-ref --quiet --short HEAD)" = sim-latency ] &&
            [ "$(git -C "$repository_root" rev-parse HEAD)" = "$expected_commit" ] &&
            [ -z "$(git -C "$repository_root" status --porcelain=v1 --untracked-files=all)" ] ||
            die "temporary $repository source checkout is not clean and identity-bound"
    done
    [ "$(git -C "$root/server" rev-parse HEAD)" = "$expected_server" ] ||
        die "temporary server source checkout has the wrong commit"
    [ "$(jq -er '.candidate_patch_sha256 // ""' "$root/.evaluation-source.json")" = "$expected_patch" ] ||
        die "temporary source patch identity mismatch"

    # Preserve tracked executable bits while making both the bind itself and
    # the underlying host tree non-writable. The Docker bind adds a second,
    # independently inspected read-only boundary.
    sudo -n find "$root" -type d -exec chmod 0555 {} +
    sudo -n find "$root" -type f -perm /111 -exec chmod 0555 {} +
    sudo -n find "$root" -type f ! -perm /111 -exec chmod 0444 {} +
    sudo -n chown -R "$container_host_uid:$container_host_gid" "$root"
}

remove_evaluation_sources() {
    [ -n "${active_source_root:-}" ] || return 0
    case "$active_source_root" in
        "$work_dir"/evaluation-sources.*) ;;
        *) die "refusing to remove an unexpected source directory" ;;
    esac
    [ "$(realpath -e "$(dirname "$active_source_root")")" = "$work_dir" ] ||
        die "temporary source directory escaped the attempt tmpfs"
    sudo -n rm -rf -- "$active_source_root"
    [ ! -e "$active_source_root" ] || die "temporary evaluation sources survived cleanup"
    active_source_root=""
    jq -S '.cleanup_complete = true' "$source_evidence_path" > "$source_evidence_path.new"
    chmod 0400 "$source_evidence_path.new"
    mv -f -- "$source_evidence_path.new" "$source_evidence_path"
}

persist_evidence_tree() {
    sudo -n chown -R "$worker_uid:$worker_gid" "$work_dir"
    find "$work_dir" -type d -exec chmod u+rwx,go-rwx {} +
    find "$work_dir" -type f -exec chmod 0400 {} +
    validate_output_tree "$work_dir"
    local runtime_work_dir="$work_dir"
    work_dir="$artifact_dir/evidence"
    [ ! -e "$work_dir" ] || die "retained evidence directory already exists"
    install -d -m 0700 "$work_dir"
    cp -a "$runtime_work_dir/." "$work_dir/"
    sync "$work_dir"
    sudo -n umount "$runtime_work_dir"
    active_work_mount=""
    rmdir "$runtime_work_dir"
    validate_output_tree "$work_dir"
    find "$work_dir" -type f -exec chmod 0400 {} +
    find "$work_dir" -type d -exec chmod 0500 {} +
}

write_evidence_manifest() {
    evidence_manifest="$artifact_dir/evidence-manifest.json"
    local evidence_lines="$artifact_dir/.evidence-lines"
    while IFS= read -r -d '' path; do
        local relative="${path#"$artifact_dir/"}"
        printf '%s\t%s\t%s\n' "$relative" "$(sha256_file "$path")" "$(file_bytes "$path")"
    done < <(find "$work_dir" -type f -print0 | sort -z) > "$evidence_lines"
    jq -Rn --arg job_id "$job_id" --arg round_id "$round_id" \
        '[inputs | split("\t") | {path:.[0],sha256:.[1],bytes:(.[2]|tonumber)}] |
         {schema:1,kind:"sim-latency-evidence-manifest",job_id:$job_id,
          round_id:$round_id,artifacts:.}' < "$evidence_lines" > "$evidence_manifest"
    rm -f -- "$evidence_lines"
    chmod 0400 "$evidence_manifest"
}

emit_candidate_build_failure() {
    local build_log="$1"
    write_evaluation_progress failed 0 0
    if [ -f "$build_log" ] && [ "$MAX_BUILD_LOG_BYTES" -lt "$(file_bytes "$build_log")" ]; then
        tail -c "$MAX_BUILD_LOG_BYTES" "$build_log" > "$build_log.tail"
        mv "$build_log.tail" "$build_log"
    fi
    rm -f -- "$work_dir/candidate-build.json"
    [ -z "$(sudo -n docker ps -aq --filter "label=com.urnetwork.competition.job-id=$job_id")" ] ||
        die "candidate build failure left job containers"
    [ -z "$(sudo -n docker network ls -q --filter "label=com.urnetwork.competition.job-id=$job_id")" ] ||
        die "candidate build failure left job networks"
    remove_evaluation_sources

    local submission_error="$artifact_dir/submission-error.json"
    jq -n --arg job_id "$job_id" --arg round_id "$round_id" \
        --arg patch_sha256 "$patch_sha256" \
        '{schema:1,kind:"submission",code:"candidate_build_failed",
          message:"candidate did not pass the frozen offline build",
          retriable:false,job_id:$job_id,round_id:$round_id,
          patch_sha256:$patch_sha256}' > "$submission_error"
    chmod 0400 "$submission_error"

    persist_evidence_tree
    write_evidence_manifest
    local complete_path="$artifact_dir/evaluation.complete.json"
    jq -n --arg job_id "$job_id" --arg round_id "$round_id" --argjson attempt "$attempt" \
        --arg base_image_id "$base_image_id" --arg patch_sha256 "$patch_sha256" \
        --arg providers_sha256 "$providers_sha256" \
        --arg submission_error_sha256 "$(sha256_file "$submission_error")" \
        --arg evidence_manifest_sha256 "$(sha256_file "$evidence_manifest")" \
        '{schema:1,kind:"sim-latency-worker-evaluation-complete",job_id:$job_id,
          round_id:$round_id,attempt:$attempt,base_image_id:$base_image_id,
          candidate_image_id:null,patch_sha256:$patch_sha256,
          providers_sha256:$providers_sha256,cleanup_complete:true,
          terminal_error:"candidate_build_failed",
          artifacts:{submission_error:$submission_error_sha256,
            evidence_manifest:$evidence_manifest_sha256}}' > "$complete_path"
    chmod 0400 "$complete_path"
    sync -d "$submission_error" "$evidence_manifest" "$complete_path"
    sync "$artifact_dir"

    local artifact_records=()
    local relative path
    for relative in submission-error.json evaluation-progress.json evaluation.complete.json evidence-manifest.json; do
        path="$artifact_dir/$relative"
        artifact_records+=("$(jq -cn --arg path "$relative" --arg sha256 "$(sha256_file "$path")" \
            --argjson bytes "$(file_bytes "$path")" '{path:$path,sha256:$sha256,bytes:$bytes}')")
    done
    local artifacts_json
    artifacts_json="$(printf '%s\n' "${artifact_records[@]}" | jq -s '.')"
    local security_json
    security_json="$(jq -cn \
        '{template_database_reset:false,redis_reset:false,cgroup_contained:false,
          resource_limits:false,management_cpu_reserved:true,
          management_memory_reserved:true,default_deny_network:true,offline_build:true,
          offline_build_resource_limits:true,
          no_production_secrets:true,structural_patch_check:true,
          accounting_complete:false,resource_report_complete:false,
          cleanup_complete:true,immutable_reports:true,cgroup_id:"",
          template_database_id:"",redis_generation_id:""}')"
    local eval_error_json
    eval_error_json="$(jq '{kind,code,message,retriable}' "$submission_error")"
    local result_tmp="$artifact_dir/.worker-result.tmp"
    jq -n --arg job_id "$job_id" --argjson eval_error "$eval_error_json" \
        --argjson security "$security_json" --argjson artifacts "$artifacts_json" \
        '{schema:1,job_id:$job_id,score:null,eval_error:$eval_error,
          security:$security,artifacts:$artifacts}' > "$result_tmp"
    sync -d "$result_tmp"
    [ ! -e "$result_path" ] || die "worker result path appeared during evaluation"
    mv "$result_tmp" "$result_path"
    chmod 0400 "$result_path"
    sync -d "$result_path"
    sync "$artifact_dir"
    log "terminal submission build failure: job=$job_id"
}

container_cgroup() {
    local container_id="$1" pid rel
    pid="$(sudo -n docker inspect --format '{{.State.Pid}}' "$container_id")"
    [[ "$pid" =~ ^[1-9][0-9]*$ ]] || return 1
    rel="$(sudo -n awk -F: '$1 == "0" {print $3}' "/proc/$pid/cgroup")"
    [ -n "$rel" ] && [ -d "/sys/fs/cgroup$rel" ] || return 1
    printf '%s' "$rel"
}

read_cgroup_usage_usec() {
    awk '$1 == "usage_usec" {print $2}' "/sys/fs/cgroup$1/cpu.stat"
}

read_cgroup_peak() {
    local value
    value="$(<"/sys/fs/cgroup$1/memory.peak")"
    [[ "$value" =~ ^[0-9]+$ ]] || return 1
    printf '%s' "$value"
}

sample_cgroup_counters() {
    local target="$1"
    shift
    local rel usage peak cpu_usec peak_bytes valid sample_tmp
    sample_tmp="$target.new"
    while :; do
        cpu_usec=0
        peak_bytes=0
        valid=true
        for rel in "$@"; do
            if ! usage="$(read_cgroup_usage_usec "$rel" 2>/dev/null)"; then
                valid=false
                break
            fi
            if ! peak="$(read_cgroup_peak "$rel" 2>/dev/null)"; then
                valid=false
                break
            fi
            if [[ ! "$usage" =~ ^[0-9]+$ ]] || [[ ! "$peak" =~ ^[0-9]+$ ]]; then
                valid=false
                break
            fi
            cpu_usec=$((cpu_usec + usage))
            peak_bytes=$((peak_bytes + peak))
        done
        if [ "$valid" = true ]; then
            printf '%s %s\n' "$cpu_usec" "$peak_bytes" > "$sample_tmp"
            mv "$sample_tmp" "$target"
        fi
        sleep 0.1
    done
}

baseline_csv=()
baseline_stderr=()
baseline_accounting=()
baseline_samples=()
baseline_resources=()
baseline_markers=()
baseline_manifests=()
candidate_csv=()
candidate_stderr=()
candidate_accounting=()
candidate_samples=()
candidate_resources=()
candidate_markers=()
candidate_manifests=()
run_accounting_records=()
run_resource_records=()
migration_hashes=()
redis_generations=()
progress_records=()
progress_path="$artifact_dir/evaluation-progress.json"

join_csv() {
    local IFS=,
    printf '%s' "$*"
}

write_evaluation_progress() {
    local phase="$1" baseline_completed="$2" candidate_completed="$3"
    local pending="$progress_path.new"
    printf '%s\n' "${progress_records[@]}" | jq -s \
        --arg job_id "$job_id" --arg round_id "$round_id" --arg phase "$phase" \
        --argjson replicate_count "$replicates" \
        --argjson baseline_completed "$baseline_completed" \
        --argjson candidate_completed "$candidate_completed" \
        --argjson updated_unix_ms "$(date +%s%3N)" \
        '{schema:1,kind:"sim-latency-evaluation-progress",job_id:$job_id,
          round_id:$round_id,phase:$phase,replicate_count:$replicate_count,
          baseline_completed:$baseline_completed,
          candidate_completed:$candidate_completed,
          updated_unix_ms:$updated_unix_ms,metrics:.}' > "$pending"
    chmod 0400 "$pending"
    mv -f -- "$pending" "$progress_path"
    sync -d "$progress_path"
}

run_live_comparison() {
    local index="$1" ordinal compare_name compare_cgroup comparison
    ordinal="$(printf '%02d' "$index")"
    compare_name="urnetwork-eval-${job_id//-/}-live-${ordinal}"
    compare_name="${compare_name:0:63}"
    compare_cgroup="urnetwork-evaluation-${job_id//-/}-live-${ordinal}.slice"
    compare_cgroup="${compare_cgroup:0:95}.slice"
    compare_cgroup="${compare_cgroup/.slice.slice/.slice}"
    comparison="$(sudo -n docker run --rm --name "$compare_name" \
        --network none --read-only --user 65532:65532 \
        --cpuset-cpus "$cpuset" \
        --memory "$SCORER_MEMORY_BYTES" --memory-swap "$SCORER_MEMORY_BYTES" \
        --pids-limit "$SCORER_PIDS_LIMIT" --cgroup-parent "$compare_cgroup" \
        --cap-drop ALL --security-opt no-new-privileges:true \
        --label "com.urnetwork.competition.job-id=$job_id" \
        --label 'com.urnetwork.competition.stage=live-progress' \
        --label "com.urnetwork.competition.round-id=$round_id" \
        --mount "type=bind,src=$work_dir/scorer-input,dst=/artifacts,readonly" \
        --entrypoint /opt/urnetwork/bin/sim-latency \
        "$base_image_id" compare \
        --a "$(join_csv "${candidate_manifests[@]}")" \
        --b "$(join_csv "${baseline_manifests[@]}")" \
        --p 0.05 --json)" || die "candidate-$ordinal live comparison failed"
    jq -e \
        '.alpha == 0.05 and (.metrics | type == "array") and
         ([.metrics[] | select(.name == "ttfb_p50_ms" or
             .name == "ttfb_p95_ms" or
             .name == "throughput_p50_bytes_per_s" or
             .name == "throughput_p95_bytes_per_s")] | length) == 4' \
        <<<"$comparison" >/dev/null || die "candidate-$ordinal live comparison is invalid"
    printf '%s' "$comparison"
}

record_stage_progress() {
    local role="$1" index="$2" run_manifest="$3"
    local comparison_json=null metric quantile value metric_comparison
    local p_improvement p_regression significance
    if [ "$role" = candidate ]; then
        comparison_json="$(run_live_comparison "$index")"
    fi
    for metric in \
        ttfb_p50_ms ttfb_p95_ms \
        throughput_p50_bytes_per_s throughput_p95_bytes_per_s; do
        value="$(sudo -n jq -er --arg metric "$metric" \
            '.metrics[$metric].value |
             select(type == "number" and isfinite and . >= 0)' \
            "$run_manifest")" || die "$role-$index live metric $metric is invalid"
        case "$metric" in
            *_p50_*) quantile=p50 ;;
            *_p95_*) quantile=p95 ;;
            *) die "unsupported live metric $metric" ;;
        esac
        if [ "$role" = baseline ]; then
            p_improvement=null
            p_regression=null
            significance=baseline
        else
            metric_comparison="$(jq -ec --arg metric "$metric" \
                '.metrics[] | select(.name == $metric)' <<<"$comparison_json")" ||
                die "candidate-$index comparison omitted $metric"
            p_improvement="$(jq -c 'if .testable then (.p_a // 0) else null end' \
                <<<"$metric_comparison")"
            p_regression="$(jq -c 'if .testable then (.p_b // 0) else null end' \
                <<<"$metric_comparison")"
            significance="$(jq -r --argjson alpha 0.05 \
                'if .testable != true then "not_testable"
                 elif (.p_a // 0) <= $alpha then "improved"
                 elif (.p_b // 0) <= $alpha then "regressed"
                 else "not_significant" end' <<<"$metric_comparison")"
        fi
        progress_records+=("$(jq -cn \
            --arg role "$role" --argjson replicate "$index" \
            --arg metric "$metric" --arg quantile "$quantile" \
            --argjson value "$value" \
            --argjson p_improvement "$p_improvement" \
            --argjson p_regression "$p_regression" \
            --arg significance "$significance" \
            '{role:$role,replicate:$replicate,metric:$metric,
              quantile:$quantile,value:$value,
              p_improvement:$p_improvement,p_regression:$p_regression,
              significance:$significance}')")
    done
    if [ "$role" = baseline ]; then
        write_evaluation_progress baseline "$index" 0
    else
        write_evaluation_progress candidate "$replicates" "$index"
    fi
}

write_evaluation_progress preparing 0 0

run_stage() {
    local role="$1" index="$2" image="$3" build_sha="$4" simulator_sha="$5" candidate_patch_sha="$6"
    local ordinal evaluation_id stage_token project cgroup_parent stage_root output runner_env compose_env source
    ordinal="$(printf '%02d' "$index")"
    evaluation_id="${role}-${ordinal}-${job_id:0:8}"
    stage_token="${role}-${ordinal}"
    project="urnetwork-eval-${job_id//-/}-${stage_token}"
    project="${project:0:63}"
    cgroup_parent="urnetwork-evaluation-${job_id//-/}-${stage_token}.slice"
    cgroup_parent="${cgroup_parent:0:95}.slice"
    cgroup_parent="${cgroup_parent/.slice.slice/.slice}"
    stage_root="$work_dir/runs/$stage_token"
    output="$stage_root/output"
    runner_env="$stage_root/runner.env"
    compose_env="$stage_root/compose.env"
    if [ "$role" = baseline ]; then
        source="$baseline_source_root"
    else
        source="$candidate_source_root"
    fi
    install -d -m 0700 "$stage_root" "$output"

    stage_db_admin_password="$(new_secret)"
    stage_db_password="$(new_secret)"
    stage_redis_password="$(new_secret)"
    authenticate_local_mounts
    write_runner_env "$runner_env" "$evaluation_id" "$build_sha" "$simulator_sha" "$image" "$candidate_patch_sha"
    write_compose_env "$compose_env" "$project" "$role" "$image" "$runner_env" \
        "$work_dir/input" "$output" "$cgroup_parent" "$source"
    sudo -n chown -R "$container_host_uid:$container_host_gid" "$output"
    sudo -n chmod 0700 "$output"

    active_project="$project"
    active_compose_env="$compose_env"
    compose_with "$project" "$compose_env" --profile run config >/dev/null
    compose_with "$project" "$compose_env" --profile run up --detach --wait postgres redis >&2
    local migration_json migration_path migration_sha
    migration_json="$(compose_with "$project" "$compose_env" --profile run run --rm --no-deps --no-tty migrate)"
    jq -e '.schema == 1 and .database_version > 0 and .database_version == .migration_count' \
        <<<"$migration_json" >/dev/null || die "$stage_token migration gate failed"
    migration_path="$stage_root/migration.json"
    printf '%s\n' "$migration_json" > "$migration_path"
    migration_sha="$(sha256_file "$migration_path")"
    migration_hashes+=("$migration_sha")
    redis_generations+=("$(new_secret)")

    compose_with "$project" "$compose_env" --profile run run --rm --no-deps --no-tty runner preflight >&2
    compose_with "$project" "$compose_env" --profile run up --no-deps --detach runner >&2
    local runner_id postgres_id redis_id runner_cgroup postgres_cgroup redis_cgroup
    runner_id="$(compose_with "$project" "$compose_env" --profile run ps --all --quiet runner)"
    postgres_id="$(compose_with "$project" "$compose_env" --profile run ps --all --quiet postgres)"
    redis_id="$(compose_with "$project" "$compose_env" --profile run ps --all --quiet redis)"
    [ -n "$runner_id" ] && [ -n "$postgres_id" ] && [ -n "$redis_id" ] || die "$stage_token container identity missing"
    for _ in $(seq 1 200); do
        runner_cgroup="$(container_cgroup "$runner_id" 2>/dev/null || true)"
        postgres_cgroup="$(container_cgroup "$postgres_id" 2>/dev/null || true)"
        redis_cgroup="$(container_cgroup "$redis_id" 2>/dev/null || true)"
        [ -n "$runner_cgroup" ] && [ -n "$postgres_cgroup" ] && [ -n "$redis_cgroup" ] && break
        sleep 0.1
    done
    [ -n "$runner_cgroup" ] && [ -n "$postgres_cgroup" ] && [ -n "$redis_cgroup" ] || die "$stage_token cgroup membership unavailable"
    for rel in "$runner_cgroup" "$postgres_cgroup" "$redis_cgroup"; do
        case "$rel" in
            *"/$cgroup_parent/"*) ;;
            *) die "$stage_token container escaped the dedicated cgroup parent" ;;
        esac
    done

    local exit_code resource_sample sampler_pid cpu_usec peak_bytes
    resource_sample="$stage_root/resource-counters.txt"
    sample_cgroup_counters "$resource_sample" "$runner_cgroup" "$postgres_cgroup" "$redis_cgroup" &
    sampler_pid=$!
    active_sampler_pid="$sampler_pid"
    exit_code="$(sudo -n docker wait "$runner_id")"
    kill "$sampler_pid" >/dev/null 2>&1 || true
    wait "$sampler_pid" >/dev/null 2>&1 || true
    active_sampler_pid=""
    sudo -n docker logs "$runner_id" >&2 || true
    [ "$exit_code" -eq 0 ] || die "$stage_token runner exited $exit_code"
    [ -s "$resource_sample" ] || die "$stage_token resource counter sample missing"
    read -r cpu_usec peak_bytes < "$resource_sample"
    [[ "$cpu_usec" =~ ^[0-9]+$ ]] && [[ "$peak_bytes" =~ ^[0-9]+$ ]] ||
        die "$stage_token resource counter sample invalid"
    rm -f -- "$resource_sample"
    local inspect_path="$stage_root/containers.json" inspect_json
    inspect_json="$(sudo -n docker inspect "$runner_id" "$postgres_id" "$redis_id")"
    jq -e --arg parent "$cgroup_parent" --arg image "$image" \
        --arg cpuset "$cpuset" \
        --arg config_local "$config_local_directory" \
        --arg vault_local "$vault_local_directory" \
        --arg source "$source" \
        --argjson runner_memory "$RUNNER_MEMORY_BYTES" \
        --argjson runner_pids "$RUNNER_PIDS_LIMIT" \
        --argjson postgres_memory "$POSTGRES_MEMORY_BYTES" \
        --argjson postgres_pids "$POSTGRES_PIDS_LIMIT" \
        --argjson redis_memory "$REDIS_MEMORY_BYTES" \
        --argjson redis_pids "$REDIS_PIDS_LIMIT" \
        'length == 3 and
         (map(select(.Name | endswith("-runner-1"))) | length == 1) and
         (map(select(.Name | endswith("-runner-1")))[0] |
          .Config.Image == $image and .Config.User == "65532:65532" and
          .HostConfig.ReadonlyRootfs == true and .HostConfig.Memory == $runner_memory and
          .HostConfig.MemorySwap == $runner_memory and .HostConfig.PidsLimit == $runner_pids and
          .HostConfig.CgroupParent == $parent and .State.ExitCode == 0 and .State.OOMKilled == false and
          (.HostConfig.CapDrop | index("ALL") != null) and
          (.HostConfig.SecurityOpt | index("no-new-privileges:true") != null) and
          ([.Mounts[] | select(.Destination | startswith("/runtime"))] | length == 2) and
          (any(.Mounts[]; .Type == "bind" and .Source == $config_local and
            .Destination == "/runtime/config/local" and .RW == false)) and
          (any(.Mounts[]; .Type == "bind" and .Source == $vault_local and
            .Destination == "/runtime/vault/local" and .RW == false)) and
          (any(.Mounts[]; .Type == "bind" and .Source == $source and
            .Destination == "/workspace" and .RW == false))) and
         (map(select(.Name | endswith("-postgres-1"))) | length == 1) and
         (map(select(.Name | endswith("-postgres-1")))[0] |
          .Config.User == "999:999" and .HostConfig.Memory == $postgres_memory and
          .HostConfig.MemorySwap == $postgres_memory and .HostConfig.PidsLimit == $postgres_pids) and
         (map(select(.Name | endswith("-redis-1"))) | length == 1) and
         (map(select(.Name | endswith("-redis-1")))[0] |
          .Config.User == "999:999" and .HostConfig.Memory == $redis_memory and
          .HostConfig.MemorySwap == $redis_memory and .HostConfig.PidsLimit == $redis_pids) and
         ([.[] | .HostConfig.CgroupParent == $parent and .HostConfig.ReadonlyRootfs == true and
           .HostConfig.CpusetCpus == $cpuset and
           ((.HostConfig.CapDrop // []) | index("ALL") != null) and
           ((.HostConfig.SecurityOpt // []) | index("no-new-privileges:true") != null)] | all)' \
        <<<"$inspect_json" >/dev/null || die "$stage_token live container policy mismatch"
    jq '[.[] | {
          id:.Id,name:.Name,image_id:.Image,
          config:{image:.Config.Image,user:.Config.User,labels:.Config.Labels},
          host_config:{readonly_rootfs:.HostConfig.ReadonlyRootfs,
            memory:.HostConfig.Memory,memory_swap:.HostConfig.MemorySwap,
            pids_limit:.HostConfig.PidsLimit,cgroup_parent:.HostConfig.CgroupParent,
            cpuset_cpus:.HostConfig.CpusetCpus,cap_drop:.HostConfig.CapDrop,
            security_opt:.HostConfig.SecurityOpt,network_mode:.HostConfig.NetworkMode},
          mounts:[.Mounts[] | {type:.Type,destination:.Destination,rw:.RW}],
          state:{status:.State.Status,exit_code:.State.ExitCode,
            oom_killed:.State.OOMKilled,started_at:.State.StartedAt,
            finished_at:.State.FinishedAt}}]' <<<"$inspect_json" > "$inspect_path"
    unset inspect_json
    local network_id="${project}_evaluation"
    sudo -n docker network inspect "$network_id" | jq -e '.[0].Internal == true' >/dev/null ||
        die "$stage_token network is not internal"

    local started_at finished_at resource_start_ms resource_end_ms
    started_at="$(jq -er 'map(select(.name | endswith("-runner-1")))[0].state.started_at' "$inspect_path")"
    finished_at="$(jq -er 'map(select(.name | endswith("-runner-1")))[0].state.finished_at' "$inspect_path")"
    resource_start_ms="$(date --date="$started_at" +%s%3N)"
    resource_end_ms="$(date --date="$finished_at" +%s%3N)"

    local run_dir="$output/$evaluation_id"
    sudo -n test -s "$run_dir/run.json" || die "$stage_token run manifest missing"
    sudo -n test -s "$run_dir/accounting.source.json" || die "$stage_token provider accounting source missing"
    sudo -n jq -e --arg evaluation_id "$evaluation_id" \
        '.schema == 1 and .kind == "sim-latency-provider-accounting-source" and
         .evaluation_id == $evaluation_id and .complete == true and .provider_egress_bytes >= 0' \
        "$run_dir/accounting.source.json" >/dev/null || die "$stage_token provider accounting source invalid"
    local accounting_tmp="$stage_root/accounting.json"
    sudo -n jq \
        '{schema:1,kind:"sim-latency-accounting",evaluation_id:.evaluation_id,
          complete:.complete,measure_start_ms:.measure_start_ms,
          measure_end_ms:.measure_end_ms,provider_egress_bytes:.provider_egress_bytes}' \
        "$run_dir/accounting.source.json" > "$accounting_tmp"
    sudo -n install -o "$container_host_uid" -g "$container_host_gid" -m 0400 \
        "$accounting_tmp" "$run_dir/accounting.json"
    local resource_tmp="$stage_root/resources.json"
    jq -n \
        --arg evaluation_id "$evaluation_id" \
        --arg cgroup_id "$cgroup_parent" \
        --argjson measurement_start_ms "$resource_start_ms" \
        --argjson measurement_end_ms "$resource_end_ms" \
        --argjson cpu_seconds "$(awk -v value="$cpu_usec" 'BEGIN {printf "%.6f", value / 1000000}')" \
        --argjson peak_rss_bytes "$peak_bytes" \
        '{schema:1,kind:"sim-latency-resource-report",evaluation_id:$evaluation_id,
          cgroup_id:$cgroup_id,measurement_start_ms:$measurement_start_ms,
          measurement_end_ms:$measurement_end_ms,complete:true,exit_code:0,
          oom_killed:false,hard_killed:false,limit_escape:false,
          measurement_missing:false,cpu_seconds:$cpu_seconds,
          peak_rss_bytes:$peak_rss_bytes}' > "$resource_tmp"
    sudo -n install -o "$container_host_uid" -g "$container_host_gid" -m 0400 \
        "$resource_tmp" "$run_dir/resources.json"
    sudo -n sync -d "$run_dir/accounting.json" "$run_dir/resources.json"
    validate_output_tree "$output"

    local stats_root
    stats_root="$(sudo -n jq -er '.stats_root' "$run_dir/run.json")"
    case "$stats_root" in
        "/artifacts/$evaluation_id/site/stats/"*) ;;
        *) die "$stage_token stats root identity mismatch" ;;
    esac
    local csv="/artifacts/$evaluation_id/results.csv"
    local stderr="/artifacts/$evaluation_id/stderr.log"
    local accounting="/artifacts/$evaluation_id/accounting.json"
    local resources="/artifacts/$evaluation_id/resources.json"
    local marker="/artifacts/$evaluation_id/run.complete.json"
    local manifest="/artifacts/$evaluation_id/run.json"
    if [ "$role" = baseline ]; then
        baseline_csv+=("$csv")
        baseline_stderr+=("$stderr")
        baseline_accounting+=("$accounting")
        baseline_samples+=("$stats_root")
        baseline_resources+=("$resources")
        baseline_markers+=("$marker")
        baseline_manifests+=("$manifest")
    else
        candidate_csv+=("$csv")
        candidate_stderr+=("$stderr")
        candidate_accounting+=("$accounting")
        candidate_samples+=("$stats_root")
        candidate_resources+=("$resources")
        candidate_markers+=("$marker")
        candidate_manifests+=("$manifest")
    fi
    local provider_bytes
    provider_bytes="$(sudo -n jq -er '.provider_egress_bytes' "$run_dir/accounting.json")"
    run_accounting_records+=("$(jq -cn --arg evaluation_id "$evaluation_id" --arg role "$role" \
        --arg path "$accounting" --arg sha256 "$(sudo -n sha256sum "$run_dir/accounting.json" | awk '{print $1}')" \
        --argjson bytes "$(sudo -n stat -c '%s' "$run_dir/accounting.json")" \
        --argjson provider_egress_bytes "$provider_bytes" \
        '{evaluation_id:$evaluation_id,role:$role,path:$path,sha256:$sha256,bytes:$bytes,
          provider_egress_bytes:$provider_egress_bytes}')")
    run_resource_records+=("$(jq -cn --arg evaluation_id "$evaluation_id" --arg role "$role" \
        --arg path "$resources" --arg sha256 "$(sudo -n sha256sum "$run_dir/resources.json" | awk '{print $1}')" \
        --argjson bytes "$(sudo -n stat -c '%s' "$run_dir/resources.json")" \
        --argjson cpu_seconds "$(awk -v value="$cpu_usec" 'BEGIN {printf "%.6f", value / 1000000}')" \
        --argjson peak_rss_bytes "$peak_bytes" \
        '{evaluation_id:$evaluation_id,role:$role,path:$path,sha256:$sha256,bytes:$bytes,
          cpu_seconds:$cpu_seconds,peak_rss_bytes:$peak_rss_bytes}')")

    compose_with "$project" "$compose_env" --profile run down --volumes --remove-orphans >&2
    [ -z "$(sudo -n docker ps -aq --filter "label=com.urnetwork.competition.job-id=$job_id" \
        --filter "label=com.urnetwork.competition.stage=$role")" ] || die "$stage_token cleanup left containers"
    sudo -n cp -a "$output/." "$work_dir/scorer-input/"
    validate_output_tree "$work_dir/scorer-input"
    record_stage_progress "$role" "$index" "$run_dir/run.json"
    authenticate_local_mounts
    active_project=""
    active_compose_env=""
    rm -f -- "$runner_env" "$compose_env" "$accounting_tmp" "$resource_tmp"
    sudo -n chown -R "$worker_uid:$worker_gid" "$stage_root"
    log "$stage_token complete"
}

write_evaluation_progress building 0 0
log "building authenticated candidate image offline"
candidate_build_json="$work_dir/candidate-build.json"
candidate_build_log="$work_dir/candidate-build.log"
candidate_build_pipe="$work_dir/.candidate-build.pipe"
mkfifo -m 0600 "$candidate_build_pipe"
{ head -c "$MAX_BUILD_LOG_BYTES"; cat >/dev/null; } \
    < "$candidate_build_pipe" > "$candidate_build_log" &
active_build_log_pid=$!
active_build_log_pipe="$candidate_build_pipe"
trap - ERR
set +e
$BUILD_SUBMISSION --allow-local-base --base-image "$base_build_ref" \
    --source-root "$candidate_source_root" \
    --patch "$patch_path" --policy "$policy_path" \
    > "$candidate_build_json" 2> "$candidate_build_pipe"
candidate_build_status=$?
wait "$active_build_log_pid"
candidate_build_log_status=$?
active_build_log_pid=""
rm -f -- "$candidate_build_pipe"
active_build_log_pipe=""
set -e
trap 'on_error "$LINENO" "$?"' ERR
[ "$candidate_build_log_status" -eq 0 ] || die "bounded candidate build log capture failed"
if [ "$candidate_build_status" -ne 0 ]; then
    if [ "$candidate_build_status" -eq 2 ] ||
       ! sudo -n docker info >/dev/null 2>&1 ||
       ! sudo -n docker image inspect "$base_image_id" >/dev/null 2>&1; then
        die "candidate builder failed because evaluator infrastructure is unavailable"
    fi
    emit_candidate_build_failure "$candidate_build_log"
    exit 0
fi
chmod 0400 "$candidate_build_json" "$candidate_build_log"
readonly candidate_build_keys='["base_image_id","base_sha","builder_sha256","candidate_sha","image","image_id","image_key","patch_sha256","policy_sha256","schema"]'
jq -e --argjson candidate_build_keys "$candidate_build_keys" \
    'type == "object" and .schema == 1 and ((keys | sort) == $candidate_build_keys) and
     (.image | type == "string" and length > 0) and
     (.image_id | type == "string" and test("^sha256:[0-9a-f]{64}$")) and
     (.base_image_id | type == "string" and test("^sha256:[0-9a-f]{64}$")) and
     (.base_sha | type == "string" and test("^[0-9a-f]{40}$")) and
     (.candidate_sha | type == "string" and test("^[0-9a-f]{40}$")) and
     (.patch_sha256 | type == "string" and test("^[0-9a-f]{64}$")) and
     (.policy_sha256 | type == "string" and test("^[0-9a-f]{64}$")) and
     (.builder_sha256 | type == "string" and test("^[0-9a-f]{64}$")) and
     (.image_key | type == "string" and test("^[0-9a-f]{64}$"))' \
    "$candidate_build_json" >/dev/null || die "candidate build record is invalid"
candidate_image_id="$(jq -er '.image_id' "$candidate_build_json")"
candidate_sha="$(jq -er '.candidate_sha' "$candidate_build_json")"
[ "$(jq -er '.base_image_id' "$candidate_build_json")" = "$base_image_id" ] || die "candidate base identity mismatch"
[ "$(jq -er '.base_sha' "$candidate_build_json")" = "$base_sha" ] || die "candidate base SHA mismatch"
[ "$(jq -er '.patch_sha256' "$candidate_build_json")" = "$patch_sha256" ] || die "candidate patch identity mismatch"
[ "$(jq -er '.policy_sha256' "$candidate_build_json")" = "$policy_sha256" ] || die "candidate policy identity mismatch"
[ "$(jq -er '.builder_sha256' "$candidate_build_json")" = "$builder_sha256" ] || die "candidate builder identity mismatch"
[ "$(jq -er '.image_key' "$candidate_build_json")" = "$image_key" ] || die "candidate image key mismatch"
[ "$(jq -er '.repositories.server' "$candidate_source_root/.evaluation-source.json")" = "$candidate_sha" ] ||
    die "temporary candidate checkout does not match the candidate image"
[ "$(jq -er '.candidate_patch_sha256' "$candidate_source_root/.evaluation-source.json")" = "$patch_sha256" ] ||
    die "temporary candidate checkout does not match the canonical patch"
[ "$(sudo -n docker image inspect --format '{{.Id}}' "$candidate_image_id")" = "$candidate_image_id" ] ||
    die "candidate image id is unavailable or changed"
candidate_identity="$(sudo -n docker run --rm --network none --read-only --cap-drop ALL \
    --security-opt no-new-privileges:true "$candidate_image_id" identity)"
candidate_simulator_sha256="$(jq -er '.simulator_sha256' <<<"$candidate_identity")"
jq -e --arg base_sha "$base_sha" --arg build_sha "$candidate_sha" \
    --arg patch_sha "$patch_sha256" --arg policy_sha "$policy_sha256" \
    --arg builder_sha "$builder_sha256" --arg image_key "$image_key" \
    '.schema == 1 and .image_kind == "submission" and .base_sha == $base_sha and
     .build_sha == $build_sha and .patch_sha256 == $patch_sha and
     .policy_sha256 == $policy_sha and .builder_sha256 == $builder_sha and
     .image_key == $image_key and (.simulator_sha256 | test("^[0-9a-f]{64}$")) and
     (.paths | type == "array")' \
    <<<"$candidate_identity" >/dev/null || die "candidate image identity is invalid"
[ "$(git -C "$candidate_source_root/server" rev-parse HEAD:connect/sim-latency)" = \
    "$(git -C "$baseline_source_root/server" rev-parse HEAD:connect/sim-latency)" ] ||
    die "candidate changed the protected sim-latency source tree"
write_source_evidence false
seal_evaluation_source "$baseline_source_root" "$base_sha" ""
seal_evaluation_source "$candidate_source_root" "$candidate_sha" "$patch_sha256"

for ((i = 1; i <= replicates; i++)); do
    run_stage baseline "$i" "$base_image_id" "$base_sha" "$base_simulator_sha256" "$EMPTY_PATCH_SHA256"
done
for ((i = 1; i <= replicates; i++)); do
    run_stage candidate "$i" "$candidate_image_id" "$candidate_sha" "$candidate_simulator_sha256" "$patch_sha256"
done
write_evaluation_progress scoring "$replicates" "$replicates"

score_runner_env="$work_dir/scorer.env"
score_compose_env="$work_dir/scorer-compose.env"
stage_db_admin_password="$(new_secret)"
stage_db_password="$(new_secret)"
stage_redis_password="$(new_secret)"
write_runner_env "$score_runner_env" "score-${job_id:0:8}" "$base_sha" "$base_simulator_sha256" "$base_image_id" "$EMPTY_PATCH_SHA256"
sed -i 's|^APEX_PROVIDERS_FILE=.*|APEX_PROVIDERS_FILE=/artifacts/providers.yml|' "$score_runner_env"
chmod 0600 "$score_runner_env"
printf '%s\n' \
    "APEX_ROUND_ID=$round_id" \
    "APEX_TAKEOVER_MARGIN=$takeover_margin" \
    "APEX_BASELINE_RUNS=$(join_csv "${baseline_csv[@]}")" \
    "APEX_BASELINE_STDERR=$(join_csv "${baseline_stderr[@]}")" \
    "APEX_BASELINE_ACCOUNTING=$(join_csv "${baseline_accounting[@]}")" \
    "APEX_BASELINE_SAMPLES=$(join_csv "${baseline_samples[@]}")" \
    "APEX_BASELINE_RESOURCES=$(join_csv "${baseline_resources[@]}")" \
    "APEX_BASELINE_MARKERS=$(join_csv "${baseline_markers[@]}")" \
    'APEX_BASELINE_MANIFEST=/score-output/baseline.json' \
    >> "$score_runner_env"
chmod 0400 "$score_runner_env"

baseline_score_output="$work_dir/baseline-score-output"
install -d -m 0700 "$baseline_score_output"
sudo -n chown "$container_host_uid:$container_host_gid" "$baseline_score_output"
score_project="urnetwork-eval-${job_id//-/}-baseline-score"
score_project="${score_project:0:63}"
score_cgroup_parent="urnetwork-evaluation-${job_id//-/}-baseline-score.slice"
write_compose_env "$score_compose_env" "$score_project" score "$base_image_id" "$score_runner_env" \
    "$work_dir/scorer-input" "$baseline_score_output" "$score_cgroup_parent" "$baseline_source_root"
sed -i 's/^EVALUATION_ACTION=.*/EVALUATION_ACTION=baseline/' "$score_compose_env"
active_project="$score_project"
active_compose_env="$score_compose_env"
compose_with "$score_project" "$score_compose_env" --profile score up --detach scorer >&2
scorer_id="$(compose_with "$score_project" "$score_compose_env" --profile score ps --all --quiet scorer)"
[ -n "$scorer_id" ] || die "baseline scorer container missing"
scorer_exit="$(sudo -n docker wait "$scorer_id")"
baseline_scorer_log="$work_dir/baseline-scorer.log"
sudo -n docker logs "$scorer_id" > "$baseline_scorer_log" 2>&1 || true
cat "$baseline_scorer_log" >&2
chmod 0400 "$baseline_scorer_log"
[ "$scorer_exit" -eq 0 ] || die "baseline scorer exited $scorer_exit"
sudo -n docker inspect "$scorer_id" | jq -e --arg parent "$score_cgroup_parent" \
    --arg image "$base_image_id" --arg cpuset "$cpuset" \
    '.[0].Config.User == "65532:65532" and .[0].HostConfig.ReadonlyRootfs == true and
     .[0].HostConfig.NetworkMode == "none" and .[0].HostConfig.Memory == 4294967296 and
     .[0].HostConfig.MemorySwap == 4294967296 and .[0].HostConfig.PidsLimit == 1024 and
     .[0].Config.Image == $image and .[0].HostConfig.CgroupParent == $parent and
     .[0].HostConfig.CpusetCpus == $cpuset and
     (.[0].HostConfig.CapDrop | index("ALL") != null) and
     (.[0].HostConfig.SecurityOpt | index("no-new-privileges:true") != null)' >/dev/null ||
    die "baseline scorer containment mismatch"
sudo -n test -s "$baseline_score_output/baseline.json" || die "baseline manifest missing"
compose_with "$score_project" "$score_compose_env" --profile score down --volumes --remove-orphans >&2
active_project=""
active_compose_env=""
sudo -n install -o "$worker_uid" -g "$worker_gid" -m 0400 \
    "$baseline_score_output/baseline.json" "$artifact_dir/baseline.json"
sudo -n install -o "$container_host_uid" -g "$container_host_gid" -m 0400 \
    "$artifact_dir/baseline.json" "$work_dir/scorer-input/baseline.json"
baseline_sha256="$(sha256_file "$artifact_dir/baseline.json")"

# Rebuild the scorer environment for candidate scoring; no baseline path was
# ever mounted into a candidate runner.
chmod 0600 "$score_runner_env"
write_runner_env "$score_runner_env" "score-${job_id:0:8}" "$base_sha" "$base_simulator_sha256" "$base_image_id" "$EMPTY_PATCH_SHA256"
sed -i 's|^APEX_PROVIDERS_FILE=.*|APEX_PROVIDERS_FILE=/artifacts/providers.yml|' "$score_runner_env"
chmod 0600 "$score_runner_env"
printf '%s\n' \
    'APEX_BASELINE_MANIFEST=/artifacts/baseline.json' \
    "APEX_BASELINE_SHA256=$baseline_sha256" \
    "APEX_CANDIDATE_RUNS=$(join_csv "${candidate_csv[@]}")" \
    "APEX_CANDIDATE_STDERR=$(join_csv "${candidate_stderr[@]}")" \
    "APEX_CANDIDATE_ACCOUNTING=$(join_csv "${candidate_accounting[@]}")" \
    "APEX_CANDIDATE_SAMPLES=$(join_csv "${candidate_samples[@]}")" \
    "APEX_CANDIDATE_RESOURCES=$(join_csv "${candidate_resources[@]}")" \
    "APEX_CANDIDATE_MARKERS=$(join_csv "${candidate_markers[@]}")" \
    'APEX_SCORE_OUTPUT=/score-output/score.json' \
    >> "$score_runner_env"
chmod 0400 "$score_runner_env"

candidate_score_output="$work_dir/candidate-score-output"
install -d -m 0700 "$candidate_score_output"
sudo -n chown "$container_host_uid:$container_host_gid" "$candidate_score_output"
score_project="urnetwork-eval-${job_id//-/}-candidate-score"
score_project="${score_project:0:63}"
score_cgroup_parent="urnetwork-evaluation-${job_id//-/}-candidate-score.slice"
write_compose_env "$score_compose_env" "$score_project" score "$base_image_id" "$score_runner_env" \
    "$work_dir/scorer-input" "$candidate_score_output" "$score_cgroup_parent" "$baseline_source_root"
sed -i 's/^EVALUATION_ACTION=.*/EVALUATION_ACTION=score/' "$score_compose_env"
active_project="$score_project"
active_compose_env="$score_compose_env"
compose_with "$score_project" "$score_compose_env" --profile score up --detach scorer >&2
scorer_id="$(compose_with "$score_project" "$score_compose_env" --profile score ps --all --quiet scorer)"
[ -n "$scorer_id" ] || die "candidate scorer container missing"
scorer_exit="$(sudo -n docker wait "$scorer_id")"
candidate_scorer_log="$work_dir/candidate-scorer.log"
sudo -n docker logs "$scorer_id" > "$candidate_scorer_log" 2>&1 || true
cat "$candidate_scorer_log" >&2
chmod 0400 "$candidate_scorer_log"
[ "$scorer_exit" -eq 0 ] || die "candidate scorer exited $scorer_exit"
sudo -n docker inspect "$scorer_id" | jq -e --arg parent "$score_cgroup_parent" \
    --arg image "$base_image_id" --arg cpuset "$cpuset" \
    '.[0].Config.User == "65532:65532" and .[0].HostConfig.ReadonlyRootfs == true and
     .[0].HostConfig.NetworkMode == "none" and .[0].HostConfig.Memory == 4294967296 and
     .[0].HostConfig.MemorySwap == 4294967296 and .[0].HostConfig.PidsLimit == 1024 and
     .[0].Config.Image == $image and .[0].HostConfig.CgroupParent == $parent and
     .[0].HostConfig.CpusetCpus == $cpuset and
     (.[0].HostConfig.CapDrop | index("ALL") != null) and
     (.[0].HostConfig.SecurityOpt | index("no-new-privileges:true") != null)' >/dev/null ||
    die "candidate scorer containment mismatch"
sudo -n test -s "$candidate_score_output/score.json" || die "score output missing"
compose_with "$score_project" "$score_compose_env" --profile score down --volumes --remove-orphans >&2
active_project=""
active_compose_env=""
sudo -n install -o "$worker_uid" -g "$worker_gid" -m 0400 \
    "$candidate_score_output/score.json" "$artifact_dir/score.json"

printf '%s\n' "${run_accounting_records[@]}" | jq -s \
    --arg job_id "$job_id" --arg round_id "$round_id" \
    '{schema:1,kind:"sim-latency-evaluation-accounting",job_id:$job_id,
      round_id:$round_id,complete:true,runs:.}' > "$artifact_dir/accounting.json"
printf '%s\n' "${run_resource_records[@]}" | jq -s \
    --arg job_id "$job_id" --arg round_id "$round_id" \
    '{schema:1,kind:"sim-latency-evaluation-resources",job_id:$job_id,
      round_id:$round_id,complete:true,runs:.}' > "$artifact_dir/resources.json"
chmod 0400 "$artifact_dir/accounting.json" "$artifact_dir/resources.json"

rm -f -- "$score_runner_env" "$score_compose_env"
authenticate_local_mounts
remove_evaluation_sources
persist_evidence_tree
write_evidence_manifest

template_database_id="$(printf '%s\n' "${migration_hashes[@]}" | sha256sum | awk '{print $1}')"
redis_generation_id="$(printf '%s\n' "${redis_generations[@]}" | sha256sum | awk '{print $1}')"
[ -z "$(sudo -n docker ps -aq --filter "label=com.urnetwork.competition.job-id=$job_id")" ] ||
    die "final cleanup left job containers"
[ -z "$(sudo -n docker network ls -q --filter "label=com.urnetwork.competition.job-id=$job_id")" ] ||
    die "final cleanup left job networks"
cleanup_complete=true
write_evaluation_progress complete "$replicates" "$replicates"

complete_path="$artifact_dir/evaluation.complete.json"
jq -n \
    --arg job_id "$job_id" --arg round_id "$round_id" --argjson attempt "$attempt" \
    --arg base_image_id "$base_image_id" --arg candidate_image_id "$candidate_image_id" \
    --arg patch_sha256 "$patch_sha256" --arg providers_sha256 "$providers_sha256" \
    --arg accounting_sha256 "$(sha256_file "$artifact_dir/accounting.json")" \
    --arg baseline_sha256 "$baseline_sha256" \
    --arg resources_sha256 "$(sha256_file "$artifact_dir/resources.json")" \
    --arg score_sha256 "$(sha256_file "$artifact_dir/score.json")" \
    --arg evidence_manifest_sha256 "$(sha256_file "$evidence_manifest")" \
    '{schema:1,kind:"sim-latency-worker-evaluation-complete",job_id:$job_id,
      round_id:$round_id,attempt:$attempt,base_image_id:$base_image_id,
      candidate_image_id:$candidate_image_id,patch_sha256:$patch_sha256,
      providers_sha256:$providers_sha256,cleanup_complete:true,
      artifacts:{accounting:$accounting_sha256,baseline:$baseline_sha256,
        resources:$resources_sha256,score:$score_sha256,
        evidence_manifest:$evidence_manifest_sha256}}' > "$complete_path"
chmod 0400 "$complete_path"
sync -d "$artifact_dir/accounting.json" "$artifact_dir/baseline.json" \
    "$artifact_dir/resources.json" "$artifact_dir/score.json" "$evidence_manifest" "$complete_path"
sync "$artifact_dir"

artifact_records=()
for relative in accounting.json baseline.json resources.json score.json evaluation-progress.json evaluation.complete.json evidence-manifest.json; do
    path="$artifact_dir/$relative"
    artifact_records+=("$(jq -cn --arg path "$relative" --arg sha256 "$(sha256_file "$path")" \
        --argjson bytes "$(file_bytes "$path")" '{path:$path,sha256:$sha256,bytes:$bytes}')")
done
artifacts_json="$(printf '%s\n' "${artifact_records[@]}" | jq -s '.')"
security_json="$(jq -cn \
    --arg cgroup_id "urnetwork-evaluation-${job_id//-/}.slice" \
    --arg template_database_id "$template_database_id" \
    --arg redis_generation_id "$redis_generation_id" \
    '{template_database_reset:true,redis_reset:true,cgroup_contained:true,
      resource_limits:true,management_cpu_reserved:true,
      management_memory_reserved:true,default_deny_network:true,offline_build:true,
      offline_build_resource_limits:true,
      no_production_secrets:true,structural_patch_check:true,
      accounting_complete:true,resource_report_complete:true,
      cleanup_complete:true,immutable_reports:true,cgroup_id:$cgroup_id,
      template_database_id:$template_database_id,
      redis_generation_id:$redis_generation_id}')"

if jq -e '.eval_error == null' "$artifact_dir/score.json" >/dev/null; then
    score_json="$(jq \
        '{score_schema,raw_score,normalized_score,placeable,
          takeover_eligible:(.diagnostics.baseline_takeover_eligible // false),
          gates,significance,diagnostics}' "$artifact_dir/score.json")"
    eval_error_json=null
else
    score_json=null
    eval_error_json="$(jq \
        '.eval_error | {kind,code,message,retriable:(.kind == "infrastructure")}' \
        "$artifact_dir/score.json")"
fi

result_tmp="$artifact_dir/.worker-result.tmp"
jq -n \
    --arg job_id "$job_id" \
    --argjson score "$score_json" \
    --argjson eval_error "$eval_error_json" \
    --argjson security "$security_json" \
    --argjson artifacts "$artifacts_json" \
    '{schema:1,job_id:$job_id,score:$score,eval_error:$eval_error,
      security:$security,artifacts:$artifacts}' > "$result_tmp"
sync -d "$result_tmp"
[ ! -e "$result_path" ] || die "worker result path appeared during evaluation"
mv "$result_tmp" "$result_path"
chmod 0400 "$result_path"
sync -d "$result_path"
sync "$artifact_dir"
log "evaluation complete: job=$job_id candidate=$candidate_sha"
