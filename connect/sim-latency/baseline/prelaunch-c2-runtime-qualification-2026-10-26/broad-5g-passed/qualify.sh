#!/usr/bin/env bash
# Low-reasoning operator only: real offline builder/runtime qualification, no score.
set -Eeuo pipefail
umask 077
[[ ${1:-} == --execute-reviewed-broad-capability && $# == 1 ]]
[[ ${AUTHORIZE_C2_BROAD_QUALIFICATION:-} == yes && $(id -u) == 0 ]]
readonly ROOT=/home/by/urnetwork/temp/mono-root-c2-release-20261026.PUHdHLML
readonly QUAL="$ROOT/evidence5g-v2/qualification"
readonly BUNDLE=/usr/local/libexec/urnetwork/competition-c2-a9e8e1a7-control-79e96f0ed-go1273-evidence5g-v2/container
readonly IMAGE=sha256:b1632a55b415f823871c60b220a194947d280894111c9fa765ad88100e206f24
readonly BASE_ALIAS=urnetwork/sim-latency-evaluator-base-id:b1632a55b415f823871c60b220a194947d280894111c9fa765ad88100e206f24
readonly BASE=a9e8e1a710b24c749e6499a0606938252c83e136
readonly SOURCE_LOCK=239cea7fe6f77f989b75ba6efa677e8731640ba2fdd3690a35e6fe72103f3d11
readonly PATCH_SHA256=1685325bd0246cf9ae81f83de53b8dca3581d5876d6894ba88a95757ab0de090
readonly POLICY_SHA256=861349e1a972fa9373436aaaf70b0923638e254087dcdd4bcaaf763282af7b16
readonly REFUSAL_SHA256=3524323fc5ee4ca598f9c6bd316d849abce238a75894f82dece6a1a9bd9d90ad
readonly SLOT=/home/by/urnetwork/temp/production-frontier-20261024.63AtMXS5/scoring.lock
readonly TMPFS="$QUAL/evidence-tmpfs"
readonly LIMIT=5368709120
export PATH=/home/by/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.27.3.linux-amd64/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
export GOTOOLCHAIN=local GOMAXPROCS=2 GOFLAGS=-p=2 LANG=C LC_ALL=C
if [[ ${C2_BROAD_BOUNDED_OWNER:-} != yes ]]; then
    exec env C2_BROAD_BOUNDED_OWNER=yes taskset -c 20,22 \
        timeout --signal=TERM --kill-after=45s 1800s "$0" "$@"
fi
[[ $(awk '/Cpus_allowed_list/ {print $2}' /proc/self/status) == 20,22 ]]
exec 9<>"$SLOT"
flock -n 9
[[ ! -e "$QUAL/started" && ! -e "$QUAL/terminal.json" && ! -e "$TMPFS" ]]
[[ $(sha256sum "$QUAL/candidate.patch" | cut -d' ' -f1) == "$PATCH_SHA256" ]]
[[ $(sha256sum "$QUAL/policy.json" | cut -d' ' -f1) == "$POLICY_SHA256" ]]
[[ $(sha256sum "$QUAL/protected-refusal.patch" | cut -d' ' -f1) == "$REFUSAL_SHA256" ]]
readonly EXPECTED_CONTAINERS="$(printf '%s\n' buildx_buildkit_warp-multiarch0 urnetwork-local-pg urnetwork-local-redis | sort)"
readonly EXPECTED_NETWORKS="$(printf '%s\n' bridge host none urnetwork-local | sort)"
[[ $(docker ps -a --format '{{.Names}}' | sort) == "$EXPECTED_CONTAINERS" ]]
[[ $(docker network ls --format '{{.Name}}' | sort) == "$EXPECTED_NETWORKS" ]]
[[ $(docker image inspect --format '{{.Id}}' "$IMAGE") == "$IMAGE" ]]
! systemctl is-active --quiet competition-v2-staging.service
builder_identity="$("$BUNDLE/submission-builder-identity.sh")"
jq -e --arg revision 79e96f0eddf784038d4ed7601da8cc12d3d94e41 \
    --arg image "$IMAGE" --argjson builder "$builder_identity" \
    '.schema == 1 and .kind == "sim-latency-c2-inactive-builder-release" and
     .control_revision == $revision and .evaluator_image_digest == $image and
     .builder == $builder and .activated == false' \
    "$ROOT/evidence5g-v2/bundle-install-receipt.json" >/dev/null
printf '%s\n' "$builder_identity" > "$QUAL/builder-identity.json"
date -u +started_at=%Y-%m-%dT%H:%M:%SZ > "$QUAL/started"
started_seconds=$SECONDS
cleanup() {
    local rc=$? clean=true
    set +e
    if mountpoint -q "$TMPFS"; then
        df -B1 --output=size,used,avail "$TMPFS" > "$QUAL/tmpfs-final.txt"
        timeout --signal=TERM --kill-after=5s 15s umount "$TMPFS" || clean=false
    fi
    if [[ -d "$TMPFS" ]] && ! mountpoint -q "$TMPFS"; then
        rmdir "$TMPFS" || clean=false
    fi
    [[ $(docker ps -a --format '{{.Names}}' | sort) == "$EXPECTED_CONTAINERS" ]] || clean=false
    [[ $(docker network ls --format '{{.Name}}' | sort) == "$EXPECTED_NETWORKS" ]] || clean=false
    [[ "$clean" == true ]] || rc=1
    jq -nS --argjson exit_code "$rc" --argjson cleanup_complete "$clean" \
        --argjson duration_seconds "$((SECONDS-started_seconds))" --arg ended_at "$(date -u +%FT%TZ)" \
        '{schema:1,kind:"sim-latency-c2-broad-capability-terminal",exit_code:$exit_code,
          cleanup_complete:$cleanup_complete,duration_seconds:$duration_seconds,ended_at:$ended_at,
          qualified:($exit_code == 0 and $cleanup_complete),
          scored:false,production_activated:false,evaluator_controls_invoked:0}' \
        > "$QUAL/terminal.json"
    sha256sum "$QUAL/terminal.json"
    exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# Admission is exercised independently before any evaluator/control invocation.
set +e
timeout --signal=TERM --kill-after=5s 70s "$BUNDLE/competitionpatch" \
    --base-sha "$BASE" --patch "$QUAL/protected-refusal.patch" \
    --expected-patch-sha256 "$REFUSAL_SHA256" --policy "$QUAL/policy.json" \
    --expected-policy-sha256 "$POLICY_SHA256" \
    > "$QUAL/protected-admission.stdout" 2> "$QUAL/protected-admission.stderr"
refusal_status=$?
set -e
[[ "$refusal_status" == 1 && ! -s "$QUAL/protected-admission.stdout" ]]
grep -qx 'validate canonical patch: path_not_allowed' "$QUAL/protected-admission.stderr"
timeout --signal=TERM --kill-after=5s 70s "$BUNDLE/competitionpatch" \
    --base-sha "$BASE" --patch "$QUAL/candidate.patch" --expected-patch-sha256 "$PATCH_SHA256" \
    --policy "$QUAL/policy.json" --expected-policy-sha256 "$POLICY_SHA256" \
    > "$QUAL/candidate-admission.json" 2> "$QUAL/candidate-admission.stderr"
jq -e --arg base "$BASE" --arg patch "$PATCH_SHA256" --arg policy "$POLICY_SHA256" \
    '.schema == 1 and .base_sha == $base and .patch_sha256 == $patch and .policy_sha256 == $policy and
     .paths == ["connect/subprotocol.go","server/connect/resident_contract_manager.go"]' \
    "$QUAL/candidate-admission.json" >/dev/null
printf 'admission_refusal_layer=trusted_cli\nevaluator_controls_invoked=0\n' > "$QUAL/admission-layer.txt"

# Exercise the real preparer twice under the exact production hard tmpfs cap.
install -d -m 0700 "$TMPFS"
mount -t tmpfs -o size=5g,mode=0700,nosuid,nodev,noexec \
    urnetwork-evidence-c2-broad-capability-evidence5g-v2 "$TMPFS"
[[ $(stat -f -c '%T' "$TMPFS") == tmpfs ]]
[[ $(df -B1 --output=size "$TMPFS" | awk 'NR == 2 {print $1}') == "$LIMIT" ]]
for role in baseline candidate; do
    timeout --signal=TERM --kill-after=15s 300s "$BUNDLE/prepare-evaluation-source.sh" \
        --base-image "$IMAGE" --destination "$TMPFS/$role" \
        > "$QUAL/$role-prepared.json" 2> "$QUAL/$role-preparation.stderr"
    jq -e --arg base "$BASE" --arg image "$IMAGE" --arg lock "$SOURCE_LOCK" \
        --slurpfile expected "$ROOT/source-lock.json" \
        '.schema == 1 and .kind == "sim-latency-evaluation-source" and
         .base_sha == $base and .base_image_id == $image and .source_lock_sha256 == $lock and
         .candidate_path_base == "mono_root_v1" and .repositories == $expected[0].repositories and
         .candidate_patch_sha256 == null and .branch == "sim-latency"' \
        "$QUAL/$role-prepared.json" >/dev/null
done
df -B1 --output=size,used,avail "$TMPFS" > "$QUAL/tmpfs-two-checkouts.txt"
du -s -B1 "$TMPFS/baseline" "$TMPFS/candidate" > "$QUAL/two-checkouts-allocated.txt"
read -r actual_limit actual_used actual_available < <(df -B1 --output=size,used,avail "$TMPFS" | awk 'NR == 2 {print $1, $2, $3}')
# This is observed launch-workload capacity, not an unlimited-output guarantee.
# Original A pairs retained at most380473344B; count hidden provider copies,
# the existing bounded build log, and maximum admitted patch separately.
observed_a_evidence=380473344
hidden_provider_copies=$((2 * 2758585))
bounded_build_log=$((4 * 1024 * 1024))
maximum_patch=262144
projected_used=$((actual_used + observed_a_evidence + hidden_provider_copies + bounded_build_log + maximum_patch))
jq -nS --argjson limit "$actual_limit" --argjson source_allocated "$actual_used" \
    --argjson available "$actual_available" --argjson observed_a_evidence "$observed_a_evidence" \
    --argjson hidden_provider_copies "$hidden_provider_copies" --argjson bounded_build_log "$bounded_build_log" \
    --argjson maximum_patch "$maximum_patch" --argjson projected_used "$projected_used" \
    '{schema:1,kind:"sim-latency-c2-observed-launch-capacity",hard_tmpfs_limit_bytes:$limit,
      two_source_checkouts_allocated_bytes:$source_allocated,remaining_bytes:$available,
      observed_max_original_a_evidence_bytes:$observed_a_evidence,
      extra_hidden_provider_copies_bytes:$hidden_provider_copies,bounded_build_log_bytes:$bounded_build_log,
      maximum_admitted_patch_bytes:$maximum_patch,projected_used_bytes:$projected_used,
      projected_headroom_bytes:($limit-$projected_used),fits_observed_launch_workload:($projected_used < $limit),
      future_unbounded_candidate_output_guaranteed:false}' > "$QUAL/capacity.json"
[[ "$projected_used" -lt "$LIMIT" ]] || { printf 'prospective5GiB cap insufficient for observed launch workload\n' >&2; exit 1; }
original_identity_sha256="$(sha256sum "$TMPFS/candidate/.evaluation-source.json" | cut -d' ' -f1)"
install -m 0400 "$TMPFS/candidate/.evaluation-source.json" "$QUAL/candidate-original-source.json"
if docker image inspect "$BASE_ALIAS" >/dev/null 2>&1; then
    [[ $(docker image inspect --format '{{.Id}}' "$BASE_ALIAS") == "$IMAGE" ]]
else
    docker image tag "$IMAGE" "$BASE_ALIAS"
fi
"$BUNDLE/build-submission.sh" --allow-local-base --base-image "$BASE_ALIAS" \
    --source-root "$TMPFS/candidate" --patch "$QUAL/candidate.patch" --policy "$QUAL/policy.json" \
    --tag urnetwork/sim-latency-submission:c2-broad-capability-1685325b-79e96f0ed-evidence5g-v2 \
    > "$QUAL/candidate-build.json" 2> "$QUAL/candidate-build.stderr"
candidate_image="$(jq -er '.image_id' "$QUAL/candidate-build.json")"
[[ "$candidate_image" =~ ^sha256:[0-9a-f]{64}$ ]]
docker run --rm --network none --read-only --cap-drop ALL --security-opt no-new-privileges:true \
    --cpuset-cpus=20,22 --memory=1g --memory-swap=1g --pids-limit=256 \
    "$candidate_image" identity > "$QUAL/candidate-runtime-identity.json"
jq -e --arg base "$BASE" --arg patch "$PATCH_SHA256" --arg original "$original_identity_sha256" \
    --argjson builder "$builder_identity" --slurpfile built "$QUAL/candidate-build.json" \
    --slurpfile source "$TMPFS/candidate/.evaluation-source.json" \
    '.schema == 1 and .image_kind == "submission" and .base_sha == $base and
     .patch_sha256 == $patch and .original_source_identity_sha256 == $original and
     .builder_sha256 == $builder.builder_sha256 and .patch_validator_sha256 == $builder.patch_validator_sha256 and
     .candidate_commits == $source[0].repositories and .candidate_commits == $built[0].candidate_commits and
     .build_sha == .candidate_commits.server and .paths == ["connect/subprotocol.go","server/connect/resident_contract_manager.go"]' \
    "$QUAL/candidate-runtime-identity.json" >/dev/null
for repository in server connect sdk proxy glog goidenticons userwireguard sn warp gvisor; do
    old="$(jq -er --arg repository "$repository" '.repositories[$repository]' "$ROOT/source-lock.json")"
    new="$(jq -er --arg repository "$repository" '.candidate_commits[$repository]' "$QUAL/candidate-build.json")"
    [[ $(git -C "$TMPFS/candidate/$repository" rev-parse HEAD) == "$new" ]]
    [[ -z $(git -C "$TMPFS/candidate/$repository" status --porcelain=v1 --untracked-files=all) ]]
    case "$repository" in
        server|connect) [[ "$new" != "$old" && $(git -C "$TMPFS/candidate/$repository" rev-list --parents -n 1 HEAD) == "$new $old" ]] ;;
        *) [[ "$new" == "$old" ]] ;;
    esac
done
[[ $(git -C "$TMPFS/baseline/server" rev-parse HEAD:connect/sim-latency) == $(git -C "$TMPFS/candidate/server" rev-parse HEAD:connect/sim-latency) ]]
install -m 0400 "$TMPFS/candidate/.evaluation-source.json" "$QUAL/candidate-final-source.json"
# Execute the actual C2 verifier as the final image's default unprivileged uid.
timeout --signal=TERM --kill-after=5s 120s docker run --rm --network none --read-only \
    --cap-drop ALL --security-opt no-new-privileges:true --cpuset-cpus=20,22 \
    --memory=1g --memory-swap=1g --pids-limit=256 \
    --entrypoint /opt/urnetwork/bin/sim-latency "$candidate_image" \
    source-check --epoch=0 --repos-root=/workspace --source-config=/opt/urnetwork/sim-latency.yml --json \
    > "$QUAL/runtime-source-check.json" 2> "$QUAL/runtime-source-check.stderr"
jq -e '.schema == 1 and .epoch == 0 and .branch == "sim-latency" and .significant_improvement_percent == 26.9' \
    "$QUAL/runtime-source-check.json" >/dev/null
docker image inspect "$candidate_image" > "$QUAL/candidate-image-inspect.json"
find "$QUAL" -maxdepth 1 -type f ! -name output-files.sha256 \
    ! -name execution.stdout ! -name execution.stderr ! -name terminal.json \
    -print0 | sort -z | xargs -0 sha256sum > "$QUAL/output-files.sha256"
printf 'PASS_broad_admission_host_and_Docker_build_full_map_and_unprivileged_C2_verifier\n'
