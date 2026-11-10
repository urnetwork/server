#!/usr/bin/env bash

# Sourced only by the protected prospective builder after source validation.
# No candidate code runs in BuildKit: checks and compilation get two separate
# tracked containers, isolated caches and the pinned inherited core profile.

tracked_container_ids=()

# The independent owner retains uncertain ids even if this shell is killed
# before create acknowledgment. This best-effort path cannot release custody.
tracked_submission_cleanup() {
    local container
    for container in "${tracked_container_ids[@]}"; do
        evaluation_timed_docker rm --force "$container" >/dev/null 2>&1 || true
    done
}

# A definite nonzero candidate exit differs from missing transport ownership.
# The original single build allowance is shared across both execution stages.
tracked_submission_run_stage() {
    local stage="$1" script="$2" cache="$3" output="$4"
    local container remaining status
    local -a output_args=()
    [[ "$stage" = check || "$stage" = compile ]] || return 2
    if [[ "$stage" = compile ]]; then
        output_args=(--mount "type=bind,src=$output,dst=/candidate-output")
    fi
    remaining=$((tracked_build_deadline - SECONDS))
    [[ "$remaining" -gt 0 ]] || infrastructure_failure "tracked compiler admission exceeded its original build allowance"
    container="$(evaluation_docker create \
        --network none --read-only --cap-drop ALL --security-opt no-new-privileges:true \
        --security-opt "seccomp=$URNETWORK_SUBEVALUATION_CORE_PROFILE" --ulimit core=1:1 \
        --label "com.urnetwork.competition.job-id=$URNETWORK_SUBEVALUATION_JOB_ID" \
        --label "com.urnetwork.competition.round-id=$URNETWORK_SUBEVALUATION_ROUND_ID" \
        --label "com.urnetwork.competition.run-id=$subevaluation_run_id" \
        --label "com.urnetwork.competition.builder-stage=$stage" \
        --cgroup-parent "$build_cgroup_parent" --cpuset-cpus "$evaluation_cpuset" \
        --memory "$build_memory_limit" --memory-swap "$build_memory_limit" --pids-limit "$tracked_build_pids" \
        --user 65532:65532 --workdir /workspace/server \
        --mount "type=bind,src=$source_root,dst=/workspace,readonly" \
        --mount "type=bind,src=$cache,dst=/opt/urnetwork/go/pkg/mod/cache/download/github.com/urnetwork/server/@v" \
        "${output_args[@]}" \
        --tmpfs "/work:rw,nosuid,nodev,uid=65532,gid=65532,mode=0700,size=$tracked_build_work" \
        --env GOCACHE=/work/cache --env GOTMPDIR=/work/tmp --env TMPDIR=/work/tmp --env HOME=/work/home \
        --env GOFLAGS=-mod=readonly --env GOPROXY=off --env GOSUMDB=off \
        --entrypoint /bin/bash "$base_image_id" -euc "$script")" || infrastructure_failure "tracked compiler creation is uncertain"
    [[ "$container" =~ ^[0-9a-f]{64}$ ]] || infrastructure_failure "tracked compiler returned no full container identity"
    tracked_container_ids+=("$container")
    evaluation_docker container start "$container" >/dev/null || infrastructure_failure "tracked compiler start is uncertain"
    remaining=$((tracked_build_deadline - SECONDS))
    [[ "$remaining" -gt 0 ]] || infrastructure_failure "tracked compiler consumed its original build allowance"
    if status="$(timeout --signal=TERM --kill-after=30s "${remaining}s" \
        "$URNETWORK_SUBEVALUATION_CLIENT" --docker-client "$subevaluation_run_id" -- wait "$container")"; then
        :
    else
        infrastructure_failure "tracked compiler completion remains uncertain or exceeded its original allowance"
    fi
    [[ "$status" =~ ^[0-9]{1,3}$ && "$status" -le 255 ]] || infrastructure_failure "tracked compiler returned an invalid exit status"
    evaluation_docker logs "$container" >&2 || infrastructure_failure "tracked compiler log readback failed"
    evaluation_docker rm "$container" >/dev/null || infrastructure_failure "tracked compiler did not acknowledge removal"
    if [[ "$status" != 0 ]]; then
        printf 'candidate %s rejected with exit %s\n' "$stage" "$status" >&2
        return 1
    fi
}

# The exact base supplies the only formerly-writable module metadata leaf.
# Separate copies preserve the old discarded-check-stage poisoning boundary.
tracked_submission_build() {
    local root="$build_parent/tracked-build" source_container check_cache compile_cache output assembly
    local check_receipt compile_receipt binary_receipt simulator_sha256 candidate_source_identity_sha256 remaining repository actual
    tracked_build_deadline=$((SECONDS + BUILD_TIMEOUT_SECONDS))
    tracked_build_pids="$(jq -er '.build_pids_limit | select(type == "number" and . > 0 and floor == .)' <<<"$resource_boundary_json")"
    tracked_build_work="$(jq -er '.build_work_limit | select(type == "string" and test("^[1-9][0-9]*[gGmMkK]?$"))' <<<"$resource_boundary_json")"
    [[ ! -e "$root" && ! -L "$root" ]] || infrastructure_failure "tracked compiler directories already exist"
    check_cache="$root/check-cache"
    compile_cache="$root/compile-cache"
    output="$root/output"
    assembly="$root/assembly"
    install -d -m 0700 "$root" "$check_cache" "$compile_cache" "$output" "$assembly"
    source_container="$(evaluation_docker create "${subevaluation_probe_args[@]}" "${subevaluation_docker_args[@]}" "$base_image_id")" ||
        infrastructure_failure "trusted module metadata source creation is uncertain"
    [[ "$source_container" =~ ^[0-9a-f]{64}$ ]] || infrastructure_failure "trusted module metadata has no full owned container id"
    tracked_container_ids+=("$source_container")
    evaluation_docker_copy_tree "$source_container:/opt/urnetwork/go/pkg/mod/cache/download/github.com/urnetwork/server/@v/." "$check_cache/"
    evaluation_docker_copy_tree "$source_container:/opt/urnetwork/go/pkg/mod/cache/download/github.com/urnetwork/server/@v/." "$compile_cache/"
    evaluation_docker rm "$source_container" >/dev/null || infrastructure_failure "trusted module metadata source did not join"
    check_receipt="$(evaluation_host prepare-build-check-cache)" || infrastructure_failure "check cache handoff failed"
    compile_receipt="$(evaluation_host prepare-build-compile-cache)" || infrastructure_failure "compile cache handoff failed"
    jq -en --argjson check "$check_receipt" --argjson compile "$compile_receipt" '
        $check.stage == "check" and $compile.stage == "compile" and
        ($check.sha256 | test("^[0-9a-f]{64}$")) and
        $check.sha256 == $compile.sha256 and $check.bytes == $compile.bytes and $check.files == $compile.files' >/dev/null ||
        infrastructure_failure "separate compiler stages did not begin with identical trusted metadata"
    evaluation_host prepare-build-output
    tracked_submission_run_stage check \
        'install -d -m 0700 /work/cache /work/tmp /work/home; go vet ./connect/...; go test -run "^$" ./connect/...' \
        "$check_cache" "$output"
    tracked_submission_run_stage compile \
        'install -d -m 0700 /work/cache /work/tmp /work/home; go build -trimpath -o /candidate-output/sim-latency ./connect/sim-latency' \
        "$compile_cache" "$output"
    binary_receipt="$(evaluation_host reclaim-build-output)" || infrastructure_failure "joined compiler output reclaim failed"
    jq -e --arg revision "$candidate_sha" '
        .go_version == "go1.27.3" and .go_package == "github.com/urnetwork/server/connect/sim-latency" and
        .vcs_revision == $revision and .vcs_modified == "false" and
        (.sha256 | test("^[0-9a-f]{64}$")) and (.bytes | type == "number" and . > 0 and floor == .)' \
        <<<"$binary_receipt" >/dev/null || infrastructure_failure "joined binary metadata differs from the frozen compiler/source tuple"
    simulator_sha256="$(jq -er '.sha256' <<<"$binary_receipt")"
    [[ "$(sha256sum "$assembly/sim-latency" | awk '{print $1}')" = "$simulator_sha256" ]] ||
        infrastructure_failure "sealed assembly binary differs from its joined receipt"
    for repository in "${REPOSITORIES[@]}"; do
        actual="$(evaluation_git -C "$source_root/$repository" rev-parse HEAD)" || infrastructure_failure "candidate source commit readback failed"
        [[ "$actual" = "$(jq -er --arg repository "$repository" '.[$repository]' "$build_context/candidate-commits.json")" ]] ||
            infrastructure_failure "candidate source changed during tracked compilation"
        [[ -z "$(evaluation_git -C "$source_root/$repository" status --porcelain=v1 --untracked-files=all)" ]] ||
            infrastructure_failure "candidate source worktree changed during tracked compilation"
    done
    candidate_source_identity_sha256="$(sha256sum "$source_identity" | awk '{print $1}')"
    install -m 0400 "$assembly/sim-latency" "$build_context/sim-latency"
    install -m 0400 "$build_context/source-application.json" "$build_context/validated-identity.json"
    jq -nS --argjson check "$check_receipt" --argjson compile "$compile_receipt" --argjson binary "$binary_receipt" \
        '{schema:1,kind:"sim-latency-tracked-compiler",check_cache:$check,compile_cache:$compile,binary:$binary}' \
        > "$build_context/tracked-compiler.json"
    jq -S --arg build_sha "$candidate_sha" --arg builder "$builder_sha256" --arg image_key "$image_key" \
        --arg validator "$patch_validator_sha256" --arg commits "$candidate_commits_sha256" \
        --arg original "$original_source_identity_sha256" --arg source "$candidate_source_identity_sha256" \
        --arg binary "$simulator_sha256" \
        '. + {image_kind:"submission",build_sha:$build_sha,builder_sha256:$builder,image_key:$image_key,
          patch_validator_sha256:$validator,candidate_commits_sha256:$commits,original_source_identity_sha256:$original,
          simulator_sha256:$binary,candidate_source_identity_sha256:$source,
          source_materialization:"required-readonly-run-source/v1",builtin_source_kind:"pristine-evaluator-base"}' \
        "$build_context/validated-identity.json" > "$build_context/image-identity.json"
    chmod 0400 "$build_context/image-identity.json" "$build_context/validated-identity.json" "$build_context/tracked-compiler.json"
    remaining=$((tracked_build_deadline - SECONDS))
    [[ "$remaining" -gt 0 ]] || infrastructure_failure "tracked compiler completed after its original build allowance"
    if DOCKER_BUILDKIT=1 timeout --signal=TERM --kill-after=30s "${remaining}s" \
        "${subevaluation_docker_command[@]}" build --platform linux/amd64 --provenance=false --no-cache --pull=false --network none \
        --cgroup-parent "$build_cgroup_parent" --resource "cpuset-cpus=$evaluation_cpuset" \
        --resource "memory=$build_memory_limit" --resource "memory-swap=$build_memory_limit" \
        --file "$SCRIPT_DIR/Dockerfile.submission.tracked" \
        --build-arg "EVALUATOR_BASE_IMAGE=$base_image" --build-arg "BASE_SHA=$base_sha" --build-arg "CANDIDATE_SHA=$candidate_sha" \
        --build-arg "PATCH_SHA256=$patch_sha256" --build-arg "POLICY_SHA256=$policy_sha256" --build-arg "BUILDER_SHA256=$builder_sha256" \
        --build-arg "PATCH_VALIDATOR_SHA256=$patch_validator_sha256" --build-arg "CANDIDATE_COMMITS_SHA256=$candidate_commits_sha256" \
        --build-arg "ORIGINAL_SOURCE_IDENTITY_SHA256=$original_source_identity_sha256" \
        --build-arg "CANDIDATE_SOURCE_IDENTITY_SHA256=$candidate_source_identity_sha256" \
        --build-arg "SUBMISSION_IDENTITY=$image_key" --tag "$image_tag" "$build_context" >&2; then
        :
    else
        # Candidate compilation already returned a definite successful result.
        # A fixed COPY-only assembly error is always evaluator infrastructure.
        infrastructure_failure "trusted image assembly failed or its outcome remains uncertain"
    fi
}
