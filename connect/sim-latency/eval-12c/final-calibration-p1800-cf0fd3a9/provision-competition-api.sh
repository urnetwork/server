#!/usr/bin/env bash

# Provision the local authoritative competition API configuration without ever
# placing API bearer hashes, raw tokens, or the seed-encryption key in the
# config/local and vault/local leaves mounted into submitted containers.

set -Eeuo pipefail
umask 077
export PATH=/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

readonly SERVER=/home/by/urnetwork/server
readonly CONTROL=/home/by/urnetwork/server-finalization-control-plane
readonly ROOT="$SERVER/connect/sim-latency/eval-12c/final-calibration-p1800-cf0fd3a9"
readonly SOURCE_LOCK="$ROOT/source-lock.json"
readonly SOURCE_LOCK_SHA=0cf71458833f3b1ae96a663357c583eba3a9c25a19d6c795c8549e4154141838
readonly SELECTION="$ROOT/post-frontier/final-calibration-selection.json"
readonly REFERENCE_V5="$ROOT/reference-requalification-v5"
readonly INDEPENDENT_ROOT="$REFERENCE_V5/hidden-launch-runtime"
readonly INDEPENDENT_ATTESTATION="$INDEPENDENT_ROOT/independent-campaign-attestation.json"
readonly INDEPENDENT_PROGRESS="$INDEPENDENT_ROOT/independent-references/progress.json"
readonly INDEPENDENT_COMMITMENT="$INDEPENDENT_ROOT/independent-references/campaign-commitment.json"
readonly INDEPENDENT_REVEAL="$INDEPENDENT_ROOT/independent-references/seed-reveal.json"
readonly COMPATIBILITY_DECISION="$INDEPENDENT_ROOT/calibration-decision.json"
readonly TERMINAL_DECISION="$REFERENCE_V5/hidden-launch-decision.json"
readonly MEASUREMENT_AMENDMENT="$REFERENCE_V5/hidden-launch-measurement-amendment.json"
readonly R1_CORRECTION="$ROOT/independent-reference-r1-correction.json"
readonly INDEPENDENT_PROTOCOL="$REFERENCE_V5/hidden-launch-protocol.json"
readonly STAGING_AMENDMENT="$ROOT/production-staging-reference-v5-amendment.json"
readonly RELEASE_AMENDMENT="$ROOT/production-release-self-check-contract-amendment.json"
readonly ATTESTATION_REPAIR="$REFERENCE_V5/hidden-attestation-path-repair.json"
readonly ATTESTATION_REPAIR_SCRIPT="$REFERENCE_V5/repair-hidden-attestation-path.py"
readonly PRODUCTION_STAGING_PROTOCOL="$ROOT/production-staging-protocol.json"
readonly MEASUREMENT_AMENDMENT_SHA=9d453d7b1d763a7c0975bae8275985ef3b7fc3367535f67ccd79ac1afe9e0f61
readonly R1_CORRECTION_SHA=b500ac07ac7272e8ff839d3bdf6f5ebcdc327d254b1a6d0a5d6078b64831dafa
readonly INDEPENDENT_PROTOCOL_SHA=4969535eb343049d7b790c5fff8e82b7eb7a60b6e92d2e2aa94e6466e7789fad
readonly STAGING_AMENDMENT_SHA=618393539636b69cfcdbd6fec14afef3e58fe20d43bda06fbcbf15693802b695
readonly RELEASE_AMENDMENT_SHA=99d6010edcbc659d936e97cbc7cde48129d0af9146c6404a1bc03604d750ef5d
readonly PRODUCTION_STAGING_PROTOCOL_SHA=6fc4a809779bf6e694ef3afa71522fa50d0512c56177b42da4249738a37dc7af
readonly COMPATIBILITY_DECISION_SHA=ba49014d7ceef1ff044a2d799f7911868b2eb159a6c587425a9c9f3d4fac2649
readonly TERMINAL_DECISION_SHA=3e4cc70d783b01a87328736caf82f49016138c97ff384b26dc38864f8cede835
readonly ATTESTATION_REPAIR_SHA=499efd5e6d99f4d56a55f05d3949f6107ae8fcdeb2c7dfeb5b9877207541412d
readonly ATTESTATION_REPAIR_SCRIPT_SHA=a5bfedfd7228b8e7c01a41334aa01b0d6a413ffadc4cca380073ac9ecdb668a0
readonly CONTROL_COMMIT=2ee4883f2b77cccfcbc69b3bcf1cb4ee613dad36
readonly CONTROL_SOURCE_RELEASE="$ROOT/control-plane-release/source-release.json"
readonly CONTROL_SOURCE_RELEASE_SHA=90458a61e19259bba1bf1626b63567e92a06082d3944a070a8ea071b5f8bd5e7
readonly SUPERSEDED_CONTROL_COMMIT=5070445ddb1764ad80f999102a9d71946e5a9e29
readonly SUPERSEDED_CONTROL_SOURCE_RELEASE_SHA=b942c70bae7e69bf08c811084075a094d4cbb18d74083e53a8935de110f4c940
readonly SUPERSEDED_PROVISIONER_SHA=13256d52487fd38f3c4f8d16f8b441dcbcca04e8a8de1ba3b7c31e7186d2060c
readonly ANCHOR="$ROOT/frontier-anchor-request.json"
readonly BASE_SHA=5ca3d5242f4a7d40efe4415635608023b05a0956
readonly BASE_IMAGE=sha256:cf0fd3a9e73385729ee8dcd8da7ea53eb59d5f372b9ff36789ec923056222038
readonly SIMULATOR_SHA=247a4d2998699eb439ade7987588cf886be707bde458a07ed1fb6a4fd84c102d
readonly CONFIG_REPOSITORY=/home/by/urnetwork/config
readonly CONFIG_REPOSITORY_SHA=f61e90f9a9fe4efdbd4c200d875f6a809fde2679
readonly IPINFO_BLOB_SHA1=d43ab4c54fb5ffd9cd765a3c06e5397e3636bce4
readonly ARIN_BLOB_SHA1=095cc9db14b8d61283b46c4269c8c08cc759da03
readonly IPINFO_LFS_SHA256=4ce34ae285de80f6b1c53e4ecb0a115a78335f0523aaa23d930a6a15f60dcc22
readonly ARIN_LFS_SHA256=45e6d2ae7fd00b1c17c52ac4c548af0aa914510933eaf6d0d1ec48b2195d3255
readonly IPINFO_SIZE=1531504244
readonly ARIN_SIZE=24887592
readonly IPINFO_SOURCE="$CONFIG_REPOSITORY/all/mmdb/2026.7.2/ip-ipinfo.mmdb"
readonly ARIN_SOURCE="$CONFIG_REPOSITORY/all/arindb/2026.2.18/arin.mmdb"
readonly EVALUATION_CONFIG=/home/by/urnetwork/config/local
readonly EVALUATION_VAULT=/home/by/urnetwork/vault/local
readonly HASH_LOCAL="$SERVER/competition/container/hash-local-mount.sh"
readonly API_PARENT=/etc/urnetwork
readonly API_ROOT="$API_PARENT/competition-api"
readonly API_CONFIG_ROOT="$API_ROOT/config"
readonly API_VAULT_ROOT="$API_ROOT/vault"
readonly API_CREDENTIALS="$API_ROOT/credentials.json"
readonly API_MANIFEST="$API_ROOT/deployment-manifest.json"
readonly API_SUPERSEDED_ROOT="$API_PARENT/competition-api-superseded-5070445d"
readonly API_SERVICE_USER=by
readonly API_SERVICE_GROUP=by
readonly LIBEXEC_ROOT=/usr/local/libexec/urnetwork/competition-cf0fd3a9
readonly LIBEXEC_SUPERSEDED_ROOT=/usr/local/libexec/urnetwork/competition-cf0fd3a9-superseded-5070445d
readonly INSTALLED_SIMULATOR="$LIBEXEC_ROOT/sim-latency"
readonly INSTALLED_EVALUATOR="$LIBEXEC_ROOT/container/evaluator.sh"
readonly INSTALLED_SELF_CHECK="$LIBEXEC_ROOT/competition-host-self-check"
readonly INSTALLED_CONTAINMENT_PROMOTER="$LIBEXEC_ROOT/promote-host-containment.sh"
readonly INSTALLED_REBASELINE_PROMOTER="$LIBEXEC_ROOT/promote-round-rebaseline.sh"
readonly SELF="$(realpath "$0")"
readonly SELF_SHA_FILE="$SELF.sha256"

stage=""
image_container=""
created_api_root=false
created_libexec_root=false
api_parent_changed=false
install_committed=false

log() {
    printf '[competition-api-provision-cf0fd3a9] %s %s\n' "$(date -u '+%FT%TZ')" "$*"
}

die() {
    log "ERROR: $*" >&2
    exit 1
}

sha256_file() {
    sha256sum "$1" | awk '{print $1}'
}

sudo_sha256_file() {
    sudo -n sha256sum "$1" | awk '{print $1}'
}

cleanup() {
    local rc=$?
    if [ -n "${image_container:-}" ]; then
        sudo -n docker rm -f "$image_container" >/dev/null 2>&1 || true
    fi
    if [ -n "${stage:-}" ] && [[ "$stage" == /run/user/"$(id -u)"/competition-api-stage.* ]] && [ -d "$stage" ]; then
        chmod -R u+rwX "$stage" 2>/dev/null || true
        rm -rf -- "$stage"
    fi
    if [ "$install_committed" != true ]; then
        if [ "$created_api_root" = true ]; then
            [ "$API_ROOT" = /etc/urnetwork/competition-api ] || exit 125
            sudo -n rm -rf -- "$API_ROOT" >/dev/null 2>&1 || true
        fi
        if [ "$api_parent_changed" = true ]; then
            [ "$API_PARENT" = /etc/urnetwork ] || exit 125
            sudo -n chown root:root "$API_PARENT" >/dev/null 2>&1 || true
            sudo -n chmod 0700 "$API_PARENT" >/dev/null 2>&1 || true
        fi
        if [ "$created_libexec_root" = true ]; then
            [ "$LIBEXEC_ROOT" = /usr/local/libexec/urnetwork/competition-cf0fd3a9 ] || exit 125
            sudo -n rm -rf -- "$LIBEXEC_ROOT" >/dev/null 2>&1 || true
        fi
    fi
    exit "$rc"
}
trap cleanup EXIT INT TERM

verify_file() {
    local path="$1" expected="$2"
    [ -f "$path" ] && [ ! -L "$path" ] || die "missing or unsafe file: $path"
    [ "$(sha256_file "$path")" = "$expected" ] || die "frozen file changed: $path"
}

verify_release_amendment() {
    verify_file "$RELEASE_AMENDMENT" "$RELEASE_AMENDMENT_SHA"
    jq -e \
        --arg source "$SOURCE_LOCK_SHA" \
        --arg protocol "$PRODUCTION_STAGING_PROTOCOL_SHA" \
        --arg staging_amendment "$STAGING_AMENDMENT_SHA" \
        --arg commit "$CONTROL_COMMIT" \
        --arg source_release "$CONTROL_SOURCE_RELEASE_SHA" \
        '.schema == 1 and
         .kind == "sim-latency-production-release-self-check-contract-amendment" and
         .passed == true and
         .binding.source_lock_sha256 == $source and
         .binding.production_staging_protocol_sha256 == $protocol and
         .binding.production_staging_reference_v5_amendment_sha256 == $staging_amendment and
         .replacement_release.control_plane_commit == $commit and
         .replacement_release.origin_control_plane_commit == $commit and
         .replacement_release.source_release_sha256 == $source_release and
         .root_cause.strict_unknown_field_rejection_retained == true and
         ([.retained_invariants[]] | all)' \
        "$RELEASE_AMENDMENT" >/dev/null || die "production release self-check amendment is invalid"
}

verify_resource_layer() {
    local layer="$1" entry name
    [ -d "$layer" ] && [ ! -L "$layer" ] && [ "$(realpath -e "$layer")" = "$layer" ] ||
        die "API source resource layer is missing or unsafe: $layer"
    [ -z "$(find "$layer" -mindepth 1 -maxdepth 1 ! -type f -print -quit)" ] ||
        die "API source resource layer contains a non-regular entry: $layer"
    while IFS= read -r -d '' entry; do
        name="${entry##*/}"
        case "$name" in
            *$'\n'*|*$'\r'*) die "API source resource layer contains an unsafe path: $layer" ;;
        esac
    done < <(find "$layer" -mindepth 1 -maxdepth 1 -type f -print0)
}

verify_self() {
    [ -f "$SELF_SHA_FILE" ] && [ ! -L "$SELF_SHA_FILE" ] || die "provisioner hash lock is missing"
    local expected
    expected="$(awk 'NR == 1 {print $1}' "$SELF_SHA_FILE")"
    [[ "$expected" =~ ^[0-9a-f]{64}$ ]] || die "provisioner hash lock is malformed"
    [ "$(sha256_file "$SELF")" = "$expected" ] || die "provisioner changed after arming"
}

verify_inputs() {
    local config_hash vault_hash
    verify_file "$SOURCE_LOCK" "$SOURCE_LOCK_SHA"
    [ "$(git -C "$SERVER" rev-parse HEAD)" = "$BASE_SHA" ] || die "server source changed"
    [ -z "$(git -C "$SERVER" status --porcelain --untracked-files=no)" ] || die "server tracked worktree changed"
    [ "$(git -C "$CONTROL" rev-parse HEAD)" = "$CONTROL_COMMIT" ] || die "control-plane source changed"
    [ "$(git -C "$CONTROL" rev-parse '@{upstream}')" = "$CONTROL_COMMIT" ] || die "control-plane push identity changed"
    [ -z "$(git -C "$CONTROL" status --porcelain --untracked-files=no)" ] || die "control-plane tracked worktree changed"
    verify_file "$CONTROL_SOURCE_RELEASE" "$CONTROL_SOURCE_RELEASE_SHA"
    verify_release_amendment
    [ "$(< /proc/sys/kernel/random/boot_id)" = "$(jq -er '.host.boot_id' "$SOURCE_LOCK")" ] || die "host rebooted"
    [ "$(sudo -n docker image inspect --format '{{.Id}}' "$BASE_IMAGE")" = "$BASE_IMAGE" ] || die "frozen evaluator image unavailable"
    [ "$(git -C "$CONFIG_REPOSITORY" rev-parse HEAD)" = "$CONFIG_REPOSITORY_SHA" ] || die "config source changed"
    [ -z "$(git -C "$CONFIG_REPOSITORY" status --porcelain --untracked-files=no)" ] || die "config tracked worktree changed"
    [ "$(git -C "$CONFIG_REPOSITORY" cat-file -t "$IPINFO_BLOB_SHA1")" = blob ] || die "frozen IPInfo data blob unavailable"
    [ "$(git -C "$CONFIG_REPOSITORY" cat-file -t "$ARIN_BLOB_SHA1")" = blob ] || die "frozen ARIN data blob unavailable"
    [ "$(git -C "$CONFIG_REPOSITORY" cat-file -p "$IPINFO_BLOB_SHA1" | awk '$1 == "oid" {print $2}')" = "sha256:$IPINFO_LFS_SHA256" ] || die "frozen IPInfo LFS identity changed"
    [ "$(git -C "$CONFIG_REPOSITORY" cat-file -p "$ARIN_BLOB_SHA1" | awk '$1 == "oid" {print $2}')" = "sha256:$ARIN_LFS_SHA256" ] || die "frozen ARIN LFS identity changed"
    [ "$(git -C "$CONFIG_REPOSITORY" cat-file -p "$IPINFO_BLOB_SHA1" | awk '$1 == "size" {print $2}')" = "$IPINFO_SIZE" ] || die "frozen IPInfo LFS size changed"
    [ "$(git -C "$CONFIG_REPOSITORY" cat-file -p "$ARIN_BLOB_SHA1" | awk '$1 == "size" {print $2}')" = "$ARIN_SIZE" ] || die "frozen ARIN LFS size changed"
    [ -f "$IPINFO_SOURCE" ] && [ ! -L "$IPINFO_SOURCE" ] && [ "$(stat -c %s "$IPINFO_SOURCE")" = "$IPINFO_SIZE" ] || die "checked-out IPInfo LFS payload unavailable"
    [ -f "$ARIN_SOURCE" ] && [ ! -L "$ARIN_SOURCE" ] && [ "$(stat -c %s "$ARIN_SOURCE")" = "$ARIN_SIZE" ] || die "checked-out ARIN LFS payload unavailable"
    [ -d "$EVALUATION_CONFIG" ] && [ -d "$EVALUATION_VAULT" ] || die "evaluation local leaves missing"
    [ "$(realpath -e "$EVALUATION_CONFIG")" = "$EVALUATION_CONFIG" ] || die "evaluation config/local is not canonical"
    [ "$(realpath -e "$EVALUATION_VAULT")" = "$EVALUATION_VAULT" ] || die "evaluation vault/local is not canonical"
    [ "$(id -un)" = "$API_SERVICE_USER" ] && [ "$(id -gn)" = "$API_SERVICE_GROUP" ] ||
        die "provisioner must run as the frozen API service identity"
    verify_resource_layer "$EVALUATION_CONFIG"
    verify_resource_layer "$EVALUATION_VAULT"
    [ ! -e "$EVALUATION_CONFIG/competition.yml" ] || die "competition API config leaked into evaluation config/local"
    [ ! -e "$EVALUATION_VAULT/competition.yml" ] || die "competition API secrets leaked into evaluation vault/local"
    config_hash="$(sudo -n "$HASH_LOCAL" "$EVALUATION_CONFIG")"
    vault_hash="$(sudo -n "$HASH_LOCAL" "$EVALUATION_VAULT")"
    [ "$config_hash" = "$(jq -er '.host.config_local_sha256' "$SOURCE_LOCK")" ] || die "evaluation config/local hash changed"
    [ "$vault_hash" = "$(jq -er '.host.vault_local_sha256' "$SOURCE_LOCK")" ] || die "evaluation vault/local hash changed"
    if [ -f "$SELECTION" ]; then
        jq -e --arg source "$SOURCE_LOCK_SHA" \
            '.schema == 1 and .kind == "sim-latency-post-frontier-final-calibration-selection" and
             .accepted == true and .source_lock_sha256 == $source and .same_seed_pairs == 12 and
             .independent_seed_target == 12 and .reference_required_passes == 11 and
             .post_frontier_sequence_satisfied == true and
             (.replicate_count as $r | [1,3,5,7,9] | index($r)) != null and
             (.takeover_margin | type == "number" and isfinite and . > 0 and . <= 0.5)' "$SELECTION" >/dev/null ||
            die "final calibration selection invalid"
    fi
}

verify_terminal_evidence() {
    local attestation_sha commitment_sha decision_sha progress_sha reveal_sha selection_sha
    [ -f "$SELECTION" ] && [ ! -L "$SELECTION" ] || die "final calibration selection is missing"
    [ -f "$INDEPENDENT_ATTESTATION" ] && [ ! -L "$INDEPENDENT_ATTESTATION" ] ||
        die "independent reference attestation is missing"
    [ -f "$INDEPENDENT_PROGRESS" ] && [ ! -L "$INDEPENDENT_PROGRESS" ] ||
        die "independent reference progress is missing"
    [ -f "$INDEPENDENT_COMMITMENT" ] && [ ! -L "$INDEPENDENT_COMMITMENT" ] ||
        die "independent seed commitment is missing"
    [ -f "$INDEPENDENT_REVEAL" ] && [ ! -L "$INDEPENDENT_REVEAL" ] ||
        die "independent seed reveal is missing"
    verify_file "$MEASUREMENT_AMENDMENT" "$MEASUREMENT_AMENDMENT_SHA"
    verify_file "$R1_CORRECTION" "$R1_CORRECTION_SHA"
    verify_file "$INDEPENDENT_PROTOCOL" "$INDEPENDENT_PROTOCOL_SHA"
    verify_file "$COMPATIBILITY_DECISION" "$COMPATIBILITY_DECISION_SHA"
    verify_file "$TERMINAL_DECISION" "$TERMINAL_DECISION_SHA"
    verify_file "$STAGING_AMENDMENT" "$STAGING_AMENDMENT_SHA"
    verify_file "$ATTESTATION_REPAIR" "$ATTESTATION_REPAIR_SHA"
    verify_file "$ATTESTATION_REPAIR_SCRIPT" "$ATTESTATION_REPAIR_SCRIPT_SHA"
    verify_file "$PRODUCTION_STAGING_PROTOCOL" "$PRODUCTION_STAGING_PROTOCOL_SHA"
    jq -e --arg source "$SOURCE_LOCK_SHA" \
        '.schema == 1 and .kind == "sim-latency-post-frontier-final-calibration-selection" and
         .accepted == true and .source_lock_sha256 == $source and .same_seed_pairs == 12 and
         .independent_seed_target == 12 and .reference_required_passes == 11 and
         .post_frontier_sequence_satisfied == true and
         (.replicate_count as $r | [1,3,5,7,9] | index($r)) != null and
         (.takeover_margin | type == "number" and isfinite and . > 0 and . <= 0.5)' \
        "$SELECTION" >/dev/null || die "terminal calibration selection is invalid"
    selection_sha="$(sha256_file "$SELECTION")"
    decision_sha="$(sha256_file "$COMPATIBILITY_DECISION")"
    progress_sha="$(sha256_file "$INDEPENDENT_PROGRESS")"
    commitment_sha="$(sha256_file "$INDEPENDENT_COMMITMENT")"
    reveal_sha="$(sha256_file "$INDEPENDENT_REVEAL")"
    jq -e --arg source "$SOURCE_LOCK_SHA" --arg selection "$selection_sha" \
        --arg amendment "$MEASUREMENT_AMENDMENT_SHA" \
        --argjson replicates "$(jq -er '.replicate_count' "$SELECTION")" \
        --argjson margin "$(jq -er '.takeover_margin' "$SELECTION")" \
        '.schema == 1 and .kind == "sim-latency-final-calibration-decision" and
         .decision_ready == true and .source_lock_sha256 == $source and
         .source_calibration_selection_sha256 == $selection and
         .replicates == $replicates and .takeover_margin == $margin and
         .independent_seed_target == 5 and .reference_required_passes == 4 and
         .measurement_amendment_sha256 == $amendment' \
        "$COMPATIBILITY_DECISION" >/dev/null || die "compatibility calibration decision is invalid"
    jq -e --arg decision "$decision_sha" --arg amendment "$MEASUREMENT_AMENDMENT_SHA" \
        --arg correction "$R1_CORRECTION_SHA" \
        '.schema == 1 and .kind == "sim-latency-independent-seed-campaign-commitment" and
         .target_independent_seeds == 5 and .independent_reference_replicates == 1 and
         .calibration_decision_sha256 == $decision and .measurement_amendment_sha256 == $amendment and
         .independent_reference_r1_correction_sha256 == $correction and (.seeds | length) == 5 and
         ([.seeds[].seed_index] == [1,2,3,4,5]) and
         ([.seeds[].seed_commitment | test("^[0-9a-f]{64}$")] | all)' \
        "$INDEPENDENT_COMMITMENT" >/dev/null || die "independent seed commitment is invalid"
    jq -e --arg progress "$progress_sha" --arg commitment "$commitment_sha" --arg reveal "$reveal_sha" \
        --arg protocol "$INDEPENDENT_PROTOCOL_SHA" --arg amendment "$MEASUREMENT_AMENDMENT_SHA" \
        --arg correction "$R1_CORRECTION_SHA" \
        '.schema == 1 and .kind == "sim-latency-independent-launch-compromise-attestation" and
         .accepted == true and .target_independent_seeds == 5 and
         .reference_required_passes == 4 and .independent_reference_replicates == 1 and
         .reference_ordering_passes >= 4 and .all_seeds_precommitted_before_first_result == true and
         .campaign_progress_sha256 == $progress and .campaign_commitment_sha256 == $commitment and
         .seed_reveal_sha256 == $reveal and .protocol_sha256 == $protocol and
         .measurement_amendment_sha256 == $amendment and
         .independent_reference_r1_correction_sha256 == $correction and
         .selected_competition_replicate_count_unchanged == true and
         .confidence_equivalent_to_original_protocol == false' \
        "$INDEPENDENT_ATTESTATION" >/dev/null || die "terminal independent reference attestation is invalid"
    jq -e '.schema == 1 and .kind == "sim-latency-independent-reference-progress" and
        .complete == true and .completed_independent_seeds == 5 and
        .target_independent_seeds == 5 and .designated_independent_baselines == 5 and
        .replicates_per_reference == 1 and .reference_required_passes == 4 and
        .reference_ordering_passes >= 4 and .separability_passed == true and
        .failed_ordering_seed_indices == [3]' \
        "$INDEPENDENT_PROGRESS" >/dev/null || die "terminal independent reference progress is invalid"
    attestation_sha="$(sha256_file "$INDEPENDENT_ATTESTATION")"
    jq -e --arg source "$SOURCE_LOCK_SHA" --arg protocol "$PRODUCTION_STAGING_PROTOCOL_SHA" \
        --arg attestation "$attestation_sha" --arg terminal "$TERMINAL_DECISION_SHA" \
        --arg hidden_protocol "$INDEPENDENT_PROTOCOL_SHA" --arg repair "$ATTESTATION_REPAIR_SHA" \
        --arg repair_script "$ATTESTATION_REPAIR_SCRIPT_SHA" \
        --argjson replicates "$(jq -er '.replicate_count' "$SELECTION")" \
        --argjson margin "$(jq -er '.takeover_margin' "$SELECTION")" \
        '.schema == 1 and .kind == "sim-latency-production-staging-reference-v5-amendment" and
         .draft == false and .authorized == true and .source_lock_sha256 == $source and
         .original_production_staging_protocol_sha256 == $protocol and
         .hidden_campaign_attestation_sha256 == $attestation and
         .hidden_campaign_decision_sha256 == $terminal and
         .hidden_campaign_protocol_sha256 == $hidden_protocol and
         .hidden_attestation_repair_sha256 == $repair and
         .hidden_attestation_repair_script_sha256 == $repair_script and
         .replacement_measurement_dependencies.same_seed_pairs == 12 and
         .replacement_measurement_dependencies.independent_seeds == 5 and
         .replacement_measurement_dependencies.required_reference_ordering_passes == 4 and
         .replacement_measurement_dependencies.selected_competition_replicates == $replicates and
         .replacement_measurement_dependencies.takeover_margin == $margin and
         .retained_invariants.all_original_release_gates_unchanged == true and
         .retained_invariants.all_original_security_gates_unchanged == true and
         .retained_invariants.all_original_staging_round_gates_unchanged == true and
         .retained_invariants.production_source_changed == false and
         .retained_invariants.evaluator_image_changed == false and
         .retained_invariants.frozen_scale_changed == false and
         .retained_invariants.scorer_changed == false' \
        "$STAGING_AMENDMENT" >/dev/null || die "reference-v5 production staging amendment is invalid"
}

verify_deployment() {
    local expected_control_commit="$1" expected_source_release="$2"
    local expected_provisioner="$3" expected_release_amendment="$4"
    local manifest_config manifest_vault manifest_credentials manifest_simulator manifest_evaluator manifest_self_check
    local manifest_containment_promoter manifest_rebaseline_promoter
    local manifest_api_config manifest_api_vault source_file installed_ipinfo_hash installed_arin_hash
    sudo -n test -f "$API_MANIFEST" || return 1
    sudo -n test ! -L "$API_MANIFEST" || die "API manifest is a symlink"
    verify_terminal_evidence
    sudo -n jq -e --arg source "$SOURCE_LOCK_SHA" --arg selection "$(sha256_file "$SELECTION")" \
        --arg reference "$(sha256_file "$INDEPENDENT_ATTESTATION")" --arg provisioner "$expected_provisioner" \
        --arg staging_amendment "$STAGING_AMENDMENT_SHA" \
        --arg release_amendment "$expected_release_amendment" \
        --arg control_commit "$expected_control_commit" --arg source_release "$expected_source_release" \
        --arg config_hash "$(jq -er '.host.config_local_sha256' "$SOURCE_LOCK")" \
        --arg vault_hash "$(jq -er '.host.vault_local_sha256' "$SOURCE_LOCK")" \
        --arg config_repository "$CONFIG_REPOSITORY_SHA" --arg ipinfo_blob "$IPINFO_BLOB_SHA1" --arg arin_blob "$ARIN_BLOB_SHA1" \
        --arg ipinfo_lfs "$IPINFO_LFS_SHA256" --arg arin_lfs "$ARIN_LFS_SHA256" \
        --argjson ipinfo_size "$IPINFO_SIZE" --argjson arin_size "$ARIN_SIZE" \
        '.schema == 1 and .kind == "sim-latency-competition-api-deployment" and
         .source_lock_sha256 == $source and .final_calibration_selection_sha256 == $selection and
         .independent_attestation_sha256 == $reference and .provisioner_sha256 == $provisioner and
         .production_staging_reference_v5_amendment_sha256 == $staging_amendment and
         (.production_release_self_check_contract_amendment_sha256 // "") == $release_amendment and
         .control_plane_commit == $control_commit and
         .control_plane_source_release_sha256 == $source_release and
         .api_runtime_user == "by" and .api_runtime_group == "by" and
         .api_parent == "/etc/urnetwork" and .api_parent_mode == "root:by:0710" and
         .api_config_root == "/etc/urnetwork/competition-api/config" and
         .api_vault_root == "/etc/urnetwork/competition-api/vault" and
         .raw_credentials_path == "/etc/urnetwork/competition-api/credentials.json" and
         .evaluation_config_local_sha256 == $config_hash and .evaluation_vault_local_sha256 == $vault_hash and
         .api_resource_scope == ["local","allowlisted-host-runtime-assets","generated-competition"] and
         .nonlocal_configuration_or_vault_imported == false and
         .host_runtime_assets_exposed_to_evaluation == false and
         ([.host_runtime_assets[].path] == ["arindb/arin.mmdb","mmdb/ip-ipinfo.mmdb"]) and
         ([.host_runtime_assets[] | .source_repository_sha == $config_repository] | all) and
         ([.host_runtime_assets[] | select(.path == "mmdb/ip-ipinfo.mmdb" and .source_blob_sha1 == $ipinfo_blob and .source_lfs_sha256 == $ipinfo_lfs and .size_bytes == $ipinfo_size)] | length == 1) and
         ([.host_runtime_assets[] | select(.path == "arindb/arin.mmdb" and .source_blob_sha1 == $arin_blob and .source_lfs_sha256 == $arin_lfs and .size_bytes == $arin_size)] | length == 1) and
         .evaluation_mounts_unchanged == true and .api_secrets_outside_evaluation_mounts == true and
         .raw_credentials_present_in_manifest == false' \
        "$API_MANIFEST" >/dev/null || return 1
    [ "$(sudo -n stat -c '%U:%G:%a' "$API_PARENT")" = root:"$API_SERVICE_GROUP":710 ] || die "API parent traversal permissions"
    [ "$(sudo -n stat -c '%U:%G:%a' "$API_ROOT")" = root:"$API_SERVICE_GROUP":750 ] || die "API root permissions"
    [ "$(sudo -n stat -c '%U:%G:%a' "$API_CONFIG_ROOT")" = root:"$API_SERVICE_GROUP":550 ] || die "API config root permissions"
    [ "$(sudo -n stat -c '%U:%G:%a' "$API_CONFIG_ROOT/local")" = root:"$API_SERVICE_GROUP":550 ] || die "API config/local permissions"
    [ "$(sudo -n stat -c '%U:%G:%a' "$API_VAULT_ROOT")" = root:"$API_SERVICE_GROUP":550 ] || die "API vault root permissions"
    [ "$(sudo -n stat -c '%U:%G:%a' "$API_VAULT_ROOT/local")" = root:"$API_SERVICE_GROUP":550 ] || die "API vault/local permissions"
    [ "$(sudo -n stat -c '%U:%G:%a' "$API_CONFIG_ROOT/local/competition.yml")" = root:"$API_SERVICE_GROUP":440 ] || die "API competition config permissions"
    [ "$(sudo -n stat -c '%U:%G:%a' "$API_CONFIG_ROOT/local/mmdb/ip-ipinfo.mmdb")" = root:"$API_SERVICE_GROUP":440 ] || die "API IPInfo data permissions"
    [ "$(sudo -n stat -c '%U:%G:%a' "$API_CONFIG_ROOT/local/arindb/arin.mmdb")" = root:"$API_SERVICE_GROUP":440 ] || die "API ARIN data permissions"
    [ "$(sudo -n stat -c '%U:%G:%a' "$API_VAULT_ROOT/local/competition.yml")" = root:"$API_SERVICE_GROUP":440 ] || die "API competition vault permissions"
    [ "$(sudo -n stat -c '%U:%G:%a' "$API_CREDENTIALS")" = root:root:400 ] || die "API credentials permissions"
    [ "$(sudo -n stat -c '%U:%G:%a' "$API_MANIFEST")" = root:"$API_SERVICE_GROUP":440 ] || die "API manifest permissions"
    [ "$(sudo -n stat -c '%U:%G:%a' "$LIBEXEC_ROOT")" = root:root:555 ] || die "trusted command root permissions"
    [ "$(sudo -n stat -c '%U:%G:%a' "$INSTALLED_SIMULATOR")" = root:root:555 ] || die "installed simulator permissions"
    [ "$(sudo -n stat -c '%U:%G:%a' "$INSTALLED_EVALUATOR")" = root:root:555 ] || die "installed evaluator permissions"
    [ "$(sudo -n stat -c '%U:%G:%a' "$INSTALLED_SELF_CHECK")" = root:root:555 ] || die "installed self-check permissions"
    [ "$(sudo -n stat -c '%U:%G:%a' "$INSTALLED_CONTAINMENT_PROMOTER")" = root:root:555 ] || die "installed containment promoter permissions"
    [ "$(sudo -n stat -c '%U:%G:%a' "$INSTALLED_REBASELINE_PROMOTER")" = root:root:555 ] || die "installed rebaseline promoter permissions"
    manifest_config="$(sudo -n jq -er '.competition_config_sha256' "$API_MANIFEST")"
    manifest_vault="$(sudo -n jq -er '.competition_vault_sha256' "$API_MANIFEST")"
    manifest_credentials="$(sudo -n jq -er '.raw_credentials_sha256' "$API_MANIFEST")"
    manifest_api_config="$(sudo -n jq -er '.api_config_local_sha256' "$API_MANIFEST")"
    manifest_api_vault="$(sudo -n jq -er '.api_vault_local_sha256' "$API_MANIFEST")"
    manifest_simulator="$(sudo -n jq -er '.simulator_sha256' "$API_MANIFEST")"
    manifest_evaluator="$(sudo -n jq -er '.evaluator_sha256' "$API_MANIFEST")"
    manifest_self_check="$(sudo -n jq -er '.self_check_sha256' "$API_MANIFEST")"
    manifest_containment_promoter="$(sudo -n jq -er '.containment_promoter_sha256' "$API_MANIFEST")"
    manifest_rebaseline_promoter="$(sudo -n jq -er '.rebaseline_promoter_sha256' "$API_MANIFEST")"
    [ "$(sudo_sha256_file "$API_CONFIG_ROOT/local/competition.yml")" = "$manifest_config" ] || die "installed API config hash"
    [ "$(sudo_sha256_file "$API_VAULT_ROOT/local/competition.yml")" = "$manifest_vault" ] || die "installed API vault hash"
    [ "$(sudo_sha256_file "$API_CREDENTIALS")" = "$manifest_credentials" ] || die "installed raw credentials hash"
    [ "$(sudo -n "$HASH_LOCAL" "$API_CONFIG_ROOT/local")" = "$manifest_api_config" ] || die "installed API config/local tree hash"
    [ "$(sudo -n "$HASH_LOCAL" "$API_VAULT_ROOT/local")" = "$manifest_api_vault" ] || die "installed API vault/local tree hash"
    [ "$(sudo -n "$HASH_LOCAL" "$EVALUATION_CONFIG")" = "$(sudo -n jq -er '.source_local_layer_sha256.config' "$API_MANIFEST")" ] || die "API config/local source layer changed"
    [ "$(sudo -n "$HASH_LOCAL" "$EVALUATION_VAULT")" = "$(sudo -n jq -er '.source_local_layer_sha256.vault' "$API_MANIFEST")" ] || die "API vault/local source layer changed"
    installed_ipinfo_hash="$(sudo_sha256_file "$API_CONFIG_ROOT/local/mmdb/ip-ipinfo.mmdb")"
    installed_arin_hash="$(sudo_sha256_file "$API_CONFIG_ROOT/local/arindb/arin.mmdb")"
    [ "$installed_ipinfo_hash" = "$(sudo -n jq -er '.host_runtime_assets[] | select(.path == "mmdb/ip-ipinfo.mmdb") | .sha256' "$API_MANIFEST")" ] || die "installed IPInfo data hash"
    [ "$installed_arin_hash" = "$(sudo -n jq -er '.host_runtime_assets[] | select(.path == "arindb/arin.mmdb") | .sha256' "$API_MANIFEST")" ] || die "installed ARIN data hash"
    [ "$installed_ipinfo_hash" = "$IPINFO_LFS_SHA256" ] || die "installed IPInfo data does not match frozen LFS payload"
    [ "$installed_arin_hash" = "$ARIN_LFS_SHA256" ] || die "installed ARIN data does not match frozen LFS payload"
    [ "$(find "$EVALUATION_CONFIG" -mindepth 1 -maxdepth 1 -type f -printf '%f\n' | sort)" = \
        "$(find "$API_CONFIG_ROOT/local" -mindepth 1 -maxdepth 1 -type f ! -name competition.yml -printf '%f\n' | sort)" ] ||
        die "API config/local imported a non-local configuration file"
    [ "$(find "$EVALUATION_VAULT" -mindepth 1 -maxdepth 1 -type f -printf '%f\n' | sort)" = \
        "$(find "$API_VAULT_ROOT/local" -mindepth 1 -maxdepth 1 -type f ! -name competition.yml -printf '%f\n' | sort)" ] ||
        die "API vault/local imported a non-local vault file"
    [ "$(find "$API_CONFIG_ROOT/local" -mindepth 1 -maxdepth 1 -type d -printf '%f\n' | sort | paste -sd, -)" = arindb,mmdb ] ||
        die "API host runtime asset directory allowlist changed"
    [ -z "$(find "$API_VAULT_ROOT/local" -mindepth 1 -type d -print -quit)" ] || die "API vault contains a nested resource"
    while IFS= read -r -d '' source_file; do
        cmp -s "$source_file" "$API_CONFIG_ROOT/local/${source_file##*/}" || die "API config/local copy changed: ${source_file##*/}"
    done < <(find "$EVALUATION_CONFIG" -mindepth 1 -maxdepth 1 -type f -print0)
    while IFS= read -r -d '' source_file; do
        cmp -s "$source_file" "$API_VAULT_ROOT/local/${source_file##*/}" || die "API vault/local copy changed: ${source_file##*/}"
    done < <(find "$EVALUATION_VAULT" -mindepth 1 -maxdepth 1 -type f -print0)
    [ "$(sudo_sha256_file "$INSTALLED_SIMULATOR")" = "$manifest_simulator" ] || die "installed simulator hash"
    [ "$(sudo_sha256_file "$INSTALLED_EVALUATOR")" = "$manifest_evaluator" ] || die "installed evaluator hash"
    [ "$(sudo_sha256_file "$INSTALLED_SELF_CHECK")" = "$manifest_self_check" ] || die "installed self-check hash"
    [ "$(sudo_sha256_file "$INSTALLED_CONTAINMENT_PROMOTER")" = "$manifest_containment_promoter" ] || die "installed containment promoter hash"
    [ "$(sudo_sha256_file "$INSTALLED_REBASELINE_PROMOTER")" = "$manifest_rebaseline_promoter" ] || die "installed rebaseline promoter hash"
    [ "$manifest_simulator" = "$SIMULATOR_SHA" ] || die "installed simulator does not match frozen image"
    sudo -n jq -e '.schema == 1 and .kind == "sim-latency-competition-api-raw-credentials" and
        (.tokens | keys | sort) == ["apex-submit-a", "apex-submit-b", "competition-operator"] and
        ([.tokens[] | type == "string" and test("^[0-9a-f]{64}$")] | all)' "$API_CREDENTIALS" >/dev/null ||
        die "raw credentials file structure"
    [ ! -e "$EVALUATION_CONFIG/competition.yml" ] && [ ! -e "$EVALUATION_VAULT/competition.yml" ] ||
        die "competition API material crossed into evaluation leaves"
    return 0
}

verify_existing() {
    verify_deployment \
        "$CONTROL_COMMIT" \
        "$CONTROL_SOURCE_RELEASE_SHA" \
        "$(sha256_file "$SELF")" \
        "$RELEASE_AMENDMENT_SHA"
}

verify_superseded_existing() {
    verify_deployment \
        "$SUPERSEDED_CONTROL_COMMIT" \
        "$SUPERSEDED_CONTROL_SOURCE_RELEASE_SHA" \
        "$SUPERSEDED_PROVISIONER_SHA" \
        ""
}

archive_superseded_deployment() {
    sudo -n test ! -e "$API_SUPERSEDED_ROOT" || die "superseded API archive already exists"
    sudo -n test ! -e "$LIBEXEC_SUPERSEDED_ROOT" || die "superseded command archive already exists"
    [ "$API_ROOT" = /etc/urnetwork/competition-api ] || die "unsafe API upgrade source"
    [ "$API_SUPERSEDED_ROOT" = /etc/urnetwork/competition-api-superseded-5070445d ] ||
        die "unsafe API upgrade archive"
    [ "$LIBEXEC_ROOT" = /usr/local/libexec/urnetwork/competition-cf0fd3a9 ] ||
        die "unsafe command upgrade source"
    [ "$LIBEXEC_SUPERSEDED_ROOT" = /usr/local/libexec/urnetwork/competition-cf0fd3a9-superseded-5070445d ] ||
        die "unsafe command upgrade archive"
    sudo -n mv -- "$API_ROOT" "$API_SUPERSEDED_ROOT"
    sudo -n chown root:root "$API_SUPERSEDED_ROOT"
    sudo -n chmod 0500 "$API_SUPERSEDED_ROOT"
    sudo -n mv -- "$LIBEXEC_ROOT" "$LIBEXEC_SUPERSEDED_ROOT"
    sudo -n chown root:root "$LIBEXEC_SUPERSEDED_ROOT"
    sudo -n chmod 0555 "$LIBEXEC_SUPERSEDED_ROOT"
    sudo -n chown root:root "$API_PARENT"
    sudo -n chmod 0700 "$API_PARENT"
    log "authenticated 5070445d deployment retained under commit-qualified archive paths"
}

extract_simulator() {
    image_container="$(sudo -n docker create --network none --entrypoint /bin/true "$BASE_IMAGE")"
    [ -n "$image_container" ] || die "could not create extraction container"
    sudo -n docker cp "$image_container:/opt/urnetwork/bin/sim-latency" "$stage/libexec/sim-latency"
    sudo -n chown "$(id -u):$(id -g)" "$stage/libexec/sim-latency"
    sudo -n docker rm "$image_container" >/dev/null
    image_container=""
    chmod 0555 "$stage/libexec/sim-latency"
    [ "$(sha256_file "$stage/libexec/sim-latency")" = "$SIMULATOR_SHA" ] || die "extracted simulator hash mismatch"
}

write_resources() {
    local selection_path="${1:-$SELECTION}" reference_path="${2:-$INDEPENDENT_ATTESTATION}" command_root="${3:-$LIBEXEC_ROOT}"
    local runtime_asset_validation="${4:-frozen}"
    local replicate_count takeover_margin score_minimum score_timeout
    local submit_a_token_file submit_b_token_file operator_token_file seed_key_file
    local submit_a_hash submit_b_hash operator_hash
    local evaluator_sha self_check_sha containment_promoter_sha rebaseline_promoter_sha config_hash vault_hash
    local api_config_hash api_vault_hash config_local_hash vault_local_hash ipinfo_hash arin_hash
    replicate_count="$(jq -er '.replicate_count' "$selection_path")"
    takeover_margin="$(jq -er '.takeover_margin' "$selection_path")"
    score_minimum="$($SERVER/competition/container/timeout-budget.sh score "$replicate_count" 60000 60000 1200000 180000 120000)"
    score_timeout=$(((score_minimum * 12 + 9) / 10))
    evaluator_sha="$(sha256_file "$stage/libexec/container/evaluator.sh")"
    self_check_sha="$(sha256_file "$stage/libexec/competition-host-self-check")"
    containment_promoter_sha="$(sha256_file "$stage/libexec/promote-host-containment.sh")"
    rebaseline_promoter_sha="$(sha256_file "$stage/libexec/promote-round-rebaseline.sh")"
    config_hash="$(sudo -n "$HASH_LOCAL" "$EVALUATION_CONFIG")"
    vault_hash="$(sudo -n "$HASH_LOCAL" "$EVALUATION_VAULT")"

    mkdir -m 0700 "$stage/secrets"
    submit_a_token_file="$stage/secrets/submit-a-token"
    submit_b_token_file="$stage/secrets/submit-b-token"
    operator_token_file="$stage/secrets/operator-token"
    seed_key_file="$stage/secrets/seed-key-base64"
    openssl rand -hex 32 > "$submit_a_token_file"
    openssl rand -hex 32 > "$submit_b_token_file"
    openssl rand -hex 32 > "$operator_token_file"
    openssl rand 32 | base64 -w0 > "$seed_key_file"
    chmod 0400 "$submit_a_token_file" "$submit_b_token_file" "$operator_token_file" "$seed_key_file"
    grep -Eq '^[0-9a-f]{64}$' "$submit_a_token_file" || die "submitter A token generation failed"
    grep -Eq '^[0-9a-f]{64}$' "$submit_b_token_file" || die "submitter B token generation failed"
    grep -Eq '^[0-9a-f]{64}$' "$operator_token_file" || die "operator token generation failed"
    [ "$(wc -c < "$seed_key_file")" -eq 44 ] || die "seed key generation failed"
    submit_a_hash="$(tr -d '\n' < "$submit_a_token_file" | sha256sum | awk '{print $1}')"
    submit_b_hash="$(tr -d '\n' < "$submit_b_token_file" | sha256sum | awk '{print $1}')"
    operator_hash="$(tr -d '\n' < "$operator_token_file" | sha256sum | awk '{print $1}')"

    jq -n \
        --arg base "$BASE_SHA" --arg image "$BASE_IMAGE" \
        --arg evaluator_sha "$evaluator_sha" --arg self_check_sha "$self_check_sha" \
        --arg config_hash "$config_hash" --arg vault_hash "$vault_hash" --arg simulator_sha "$SIMULATOR_SHA" \
        --arg hardware "$(jq -er '.host.hardware_id' "$SOURCE_LOCK")" \
        --arg qualification "$(jq -er '.host.qualification_sha256' "$SOURCE_LOCK")" \
        --argjson providers "$(jq -er '.provider_count' "$selection_path")" \
        --argjson clients "$(jq -er '.client_pool_size' "$selection_path")" \
        --argjson rate "$(jq -er '.arrivals_per_minute' "$selection_path")" \
        --argjson replicates "$replicate_count" --argjson margin "$takeover_margin" \
        --argjson score_timeout "$score_timeout" --arg command_root "$command_root" \
        --arg season_end "${COMPETITION_SEASON_ENDS_AT:-2027-01-01T00:00:00Z}" \
        --arg retain_until "${COMPETITION_RETAIN_UNTIL:-2027-02-01T00:00:00Z}" \
        '{enabled:true,competition_id:"sim-latency-season-1",base_sha:$base,
          evaluator_image_digest:$image,artifact_root:"/var/lib/urnetwork/competition",
          config_local_directory:"/home/by/urnetwork/config/local",
          vault_local_directory:"/home/by/urnetwork/vault/local",
          season_ends_at:$season_end,retain_until:$retain_until,
          worker_lease_seconds:90,worker_heartbeat_seconds:20,host_heartbeat_max_age_seconds:60,
          max_infrastructure_attempts:3,
          simulator_command:($command_root + "/sim-latency"),
          evaluator_command:($command_root + "/container/evaluator.sh"),
          evaluator_command_sha256:$evaluator_sha,
          self_check_command:($command_root + "/competition-host-self-check"),
          self_check_command_sha256:$self_check_sha,
          patch_policy:{max_patch_bytes:262144,
            allowed_paths:["connect/resident_contract_manager.go"],
            forbidden_paths:["connect/sim-latency/**","stats/**","db_migrations.go","db_migrations_*.go","go.mod","go.sum","vendor/**",".github/**"]},
          evaluation_policy:{hardware_id:$hardware,host_qualification_sha256:$qualification,
            config_local_sha256:$config_hash,vault_local_sha256:$vault_hash,
            simulator_sha256:$simulator_sha,scorer_sha256:$simulator_sha,
            provider_count:$providers,client_pool_size:$clients,arrivals_per_minute:$rate,
            quality_window_size:2,exchange_hosts:4,fleet_shards:4,
            site_listen:"127.0.0.1:0",api_port:7640,ramp_ms:60000,prewarm_ms:46800000,
            settle_ms:60000,client_warmup_timeout_ms:1200000,duration_ms:180000,
            request_timeout_ms:120000,pipeline_interval_ms:10000,test_timeout_ms:3000,
            announce_timeout_ms:2000,impairment_enabled:true,replicates:$replicates,
            takeover_margin:$margin,queue_limit:1,score_timeout_seconds:$score_timeout}}' \
        > "$stage/api/config/local/competition.yml"
    chmod 0444 "$stage/api/config/local/competition.yml"

    jq -n --rawfile seed "$seed_key_file" --arg submit_a "$submit_a_hash" --arg submit_b "$submit_b_hash" --arg operator "$operator_hash" \
        '{seed_key_base64:$seed,tokens:[
          {name:"apex-submit-a",role:"submitter",sha256:$submit_a},
          {name:"apex-submit-b",role:"submitter",sha256:$submit_b},
          {name:"competition-operator",role:"operator",sha256:$operator}]}' \
        > "$stage/api/vault/local/competition.yml"
    chmod 0400 "$stage/api/vault/local/competition.yml"
    jq -n --arg created "$(date -u '+%FT%TZ')" \
        --rawfile submit_a "$submit_a_token_file" --rawfile submit_b "$submit_b_token_file" \
        --rawfile operator "$operator_token_file" \
        '{schema:1,kind:"sim-latency-competition-api-raw-credentials",created_at:$created,
          handling:"root-only; hand off out of band; never mount into evaluation containers",
          tokens:{"apex-submit-a":($submit_a | rtrimstr("\n")),
                  "apex-submit-b":($submit_b | rtrimstr("\n")),
                  "competition-operator":($operator | rtrimstr("\n"))}}' \
        > "$stage/api/credentials.json"
    chmod 0400 "$stage/api/credentials.json"
    chmod 0600 "$submit_a_token_file" "$submit_b_token_file" "$operator_token_file" "$seed_key_file"
    rm -f -- "$submit_a_token_file" "$submit_b_token_file" "$operator_token_file" "$seed_key_file"
    rmdir "$stage/secrets"

    api_config_hash="$("$HASH_LOCAL" "$stage/api/config/local")"
    api_vault_hash="$("$HASH_LOCAL" "$stage/api/vault/local")"
    config_local_hash="$(sudo -n "$HASH_LOCAL" "$EVALUATION_CONFIG")"
    vault_local_hash="$(sudo -n "$HASH_LOCAL" "$EVALUATION_VAULT")"
    ipinfo_hash="$(sha256_file "$stage/api/config/local/mmdb/ip-ipinfo.mmdb")"
    arin_hash="$(sha256_file "$stage/api/config/local/arindb/arin.mmdb")"
    if [ "$runtime_asset_validation" = frozen ]; then
        [ "$ipinfo_hash" = "$IPINFO_LFS_SHA256" ] && [ "$(stat -c %s "$stage/api/config/local/mmdb/ip-ipinfo.mmdb")" = "$IPINFO_SIZE" ] || die "staged IPInfo data changed"
        [ "$arin_hash" = "$ARIN_LFS_SHA256" ] && [ "$(stat -c %s "$stage/api/config/local/arindb/arin.mmdb")" = "$ARIN_SIZE" ] || die "staged ARIN data changed"
    elif [ "$runtime_asset_validation" != synthetic ]; then
        die "invalid runtime asset validation mode"
    fi

    jq -n --arg created "$(date -u '+%FT%TZ')" --arg source "$SOURCE_LOCK_SHA" \
        --arg selection "$(sha256_file "$selection_path")" --arg reference "$(sha256_file "$reference_path")" \
        --arg provisioner "$(sha256_file "$SELF")" --arg config_hash "$config_hash" --arg vault_hash "$vault_hash" \
        --arg competition_config "$(sha256_file "$stage/api/config/local/competition.yml")" \
        --arg competition_vault "$(sha256_file "$stage/api/vault/local/competition.yml")" \
        --arg raw_credentials "$(sha256_file "$stage/api/credentials.json")" \
        --arg api_config_hash "$api_config_hash" --arg api_vault_hash "$api_vault_hash" \
        --arg config_local_hash "$config_local_hash" --arg vault_local_hash "$vault_local_hash" \
        --arg config_repository_sha "$CONFIG_REPOSITORY_SHA" \
        --arg ipinfo_blob "$IPINFO_BLOB_SHA1" --arg ipinfo_lfs "$IPINFO_LFS_SHA256" --arg ipinfo_hash "$ipinfo_hash" \
        --arg arin_blob "$ARIN_BLOB_SHA1" --arg arin_lfs "$ARIN_LFS_SHA256" --arg arin_hash "$arin_hash" \
        --argjson ipinfo_size "$IPINFO_SIZE" --argjson arin_size "$ARIN_SIZE" \
        --arg simulator "$SIMULATOR_SHA" --arg evaluator "$evaluator_sha" --arg self_check "$self_check_sha" \
        --arg containment_promoter "$containment_promoter_sha" --arg rebaseline_promoter "$rebaseline_promoter_sha" \
        --arg control_commit "$CONTROL_COMMIT" --arg control_source_release "$CONTROL_SOURCE_RELEASE_SHA" \
        --arg staging_amendment "$STAGING_AMENDMENT_SHA" --arg release_amendment "$RELEASE_AMENDMENT_SHA" \
        '{schema:1,kind:"sim-latency-competition-api-deployment",created_at:$created,
          source_lock_sha256:$source,final_calibration_selection_sha256:$selection,
          independent_attestation_sha256:$reference,provisioner_sha256:$provisioner,
          production_staging_reference_v5_amendment_sha256:$staging_amendment,
          production_release_self_check_contract_amendment_sha256:$release_amendment,
          api_runtime_user:"by",api_runtime_group:"by",
          api_parent:"/etc/urnetwork",api_parent_mode:"root:by:0710",
          api_config_root:"/etc/urnetwork/competition-api/config",
          api_vault_root:"/etc/urnetwork/competition-api/vault",
          raw_credentials_path:"/etc/urnetwork/competition-api/credentials.json",
          evaluation_config_local:"/home/by/urnetwork/config/local",
          evaluation_vault_local:"/home/by/urnetwork/vault/local",
          evaluation_config_local_sha256:$config_hash,evaluation_vault_local_sha256:$vault_hash,
          competition_config_sha256:$competition_config,competition_vault_sha256:$competition_vault,
          api_config_local_sha256:$api_config_hash,api_vault_local_sha256:$api_vault_hash,
          source_local_layer_sha256:{config:$config_local_hash,vault:$vault_local_hash},
          api_resource_scope:["local","allowlisted-host-runtime-assets","generated-competition"],
          nonlocal_configuration_or_vault_imported:false,
          host_runtime_assets:[
            {path:"arindb/arin.mmdb",source_repository_sha:$config_repository_sha,source_blob_sha1:$arin_blob,source_lfs_sha256:$arin_lfs,size_bytes:$arin_size,sha256:$arin_hash},
            {path:"mmdb/ip-ipinfo.mmdb",source_repository_sha:$config_repository_sha,source_blob_sha1:$ipinfo_blob,source_lfs_sha256:$ipinfo_lfs,size_bytes:$ipinfo_size,sha256:$ipinfo_hash}],
          host_runtime_assets_exposed_to_evaluation:false,
          raw_credentials_sha256:$raw_credentials,
          simulator_sha256:$simulator,evaluator_sha256:$evaluator,self_check_sha256:$self_check,
          containment_promoter_sha256:$containment_promoter,
          rebaseline_promoter_sha256:$rebaseline_promoter,
          control_plane_commit:$control_commit,
          control_plane_source_release_sha256:$control_source_release,
          evaluation_mounts_unchanged:true,api_secrets_outside_evaluation_mounts:true,
          raw_credentials_present_in_manifest:false}' > "$stage/api/deployment-manifest.json"
    chmod 0444 "$stage/api/deployment-manifest.json"
}

install_bundle() {
    [ "$(sudo -n stat -c '%U:%G:%a' "$API_PARENT")" = root:root:700 ] ||
        die "API parent has unexpected pre-install ownership or mode"
    ! sudo -n test -e "$API_ROOT" || die "partial API root exists without an authenticated manifest: $API_ROOT"
    ! sudo -n test -e "$LIBEXEC_ROOT" || die "partial installed competition command root already exists: $LIBEXEC_ROOT"
    sudo -n install -d -o root -g root -m 0555 "$LIBEXEC_ROOT"
    created_libexec_root=true
    sudo -n cp -a "$stage/libexec/container" "$LIBEXEC_ROOT/container"
    sudo -n install -o root -g root -m 0555 "$stage/libexec/sim-latency" "$INSTALLED_SIMULATOR"
    sudo -n install -o root -g root -m 0555 "$stage/libexec/competition-host-self-check" "$INSTALLED_SELF_CHECK"
    sudo -n install -o root -g root -m 0555 "$stage/libexec/authoritative-host-irqs.sh" "$LIBEXEC_ROOT/authoritative-host-irqs.sh"
    sudo -n install -o root -g root -m 0555 "$stage/libexec/promote-host-containment.sh" "$INSTALLED_CONTAINMENT_PROMOTER"
    sudo -n install -o root -g root -m 0555 "$stage/libexec/promote-round-rebaseline.sh" "$INSTALLED_REBASELINE_PROMOTER"
    sudo -n chown -R root:root "$LIBEXEC_ROOT/container"
    sudo -n find "$LIBEXEC_ROOT/container" -type d -exec chmod 0555 {} +
    sudo -n find "$LIBEXEC_ROOT/container" -type f -exec chmod 0444 {} +
    sudo -n find "$LIBEXEC_ROOT/container" -type f -name '*.sh' -exec chmod 0555 {} +
    api_parent_changed=true
    sudo -n chown root:"$API_SERVICE_GROUP" "$API_PARENT"
    sudo -n chmod 0710 "$API_PARENT"
    sudo -n install -d -o root -g "$API_SERVICE_GROUP" -m 0750 "$API_ROOT"
    created_api_root=true
    sudo -n cp -a "$stage/api/config" "$API_CONFIG_ROOT"
    sudo -n cp -a "$stage/api/vault" "$API_VAULT_ROOT"
    sudo -n install -o root -g root -m 0400 "$stage/api/credentials.json" "$API_CREDENTIALS"
    sudo -n install -o root -g "$API_SERVICE_GROUP" -m 0440 "$stage/api/deployment-manifest.json" "$API_MANIFEST"
    sudo -n chown -R root:"$API_SERVICE_GROUP" "$API_CONFIG_ROOT" "$API_VAULT_ROOT"
    sudo -n find "$API_CONFIG_ROOT" -type d -exec chmod 0550 {} +
    sudo -n find "$API_CONFIG_ROOT" -type f -exec chmod 0440 {} +
    sudo -n find "$API_VAULT_ROOT" -type d -exec chmod 0550 {} +
    sudo -n find "$API_VAULT_ROOT" -type f -exec chmod 0440 {} +
}

prepare_stage() {
    local include_runtime_assets="${1:-true}"
    [ "$include_runtime_assets" = true ] || [ "$include_runtime_assets" = false ] ||
        die "invalid runtime asset staging mode"
    mkdir -m 0700 -p "$stage/api/config/local" "$stage/api/vault/local" "$stage/libexec"
    # The evaluator-host API receives a private copy of the exact local leaves.
    # Never fold config/all, config/main, or vault/main into this deployment.
    cp -a "$EVALUATION_CONFIG/." "$stage/api/config/local/"
    cp -a "$EVALUATION_VAULT/." "$stage/api/vault/local/"
    if [ "$include_runtime_assets" = true ]; then
        mkdir -m 0700 -p "$stage/api/config/local/mmdb" "$stage/api/config/local/arindb"
        cp -- "$IPINFO_SOURCE" "$stage/api/config/local/mmdb/ip-ipinfo.mmdb"
        cp -- "$ARIN_SOURCE" "$stage/api/config/local/arindb/arin.mmdb"
        [ "$(stat -c %s "$stage/api/config/local/mmdb/ip-ipinfo.mmdb")" = "$IPINFO_SIZE" ] || die "staged IPInfo data size"
        [ "$(stat -c %s "$stage/api/config/local/arindb/arin.mmdb")" = "$ARIN_SIZE" ] || die "staged ARIN data size"
        chmod 0444 "$stage/api/config/local/mmdb/ip-ipinfo.mmdb" "$stage/api/config/local/arindb/arin.mmdb"
    fi
    cp -a "$SERVER/competition/container" "$stage/libexec/container"
    cp "$SERVER/competition/host-self-check.sh" "$stage/libexec/competition-host-self-check"
    cp "$SERVER/competition/authoritative-host-irqs.sh" "$stage/libexec/authoritative-host-irqs.sh"
    cp "$CONTROL/competition/promote-host-containment.sh" "$stage/libexec/promote-host-containment.sh"
    cp "$CONTROL/competition/promote-round-rebaseline.sh" "$stage/libexec/promote-round-rebaseline.sh"
    chmod 0555 "$stage/libexec/competition-host-self-check" "$stage/libexec/authoritative-host-irqs.sh" \
        "$stage/libexec/promote-host-containment.sh" "$stage/libexec/promote-round-rebaseline.sh"
    extract_simulator
}

verify_settings() {
    local config_root="$1" vault_root="$2"
    local probe="$stage/settings-probe.go" probe_binary="$stage/settings-probe" output
    cat > "$probe" <<'EOF'
package main

import (
    "fmt"
    "os"

    "github.com/urnetwork/server/competition"
)

func main() {
    settings, err := competition.LoadSettings()
    if err != nil {
        fmt.Fprintln(os.Stderr, err)
        os.Exit(1)
    }
    fmt.Printf("competition=%s providers=%d clients=%d replicates=%d margin=%.6f\n",
        settings.CompetitionId, settings.EvaluationPolicy.ProviderCount,
        settings.EvaluationPolicy.ClientPoolSize, settings.EvaluationPolicy.Replicates,
        settings.EvaluationPolicy.TakeoverMargin)
}
EOF
    (cd "$SERVER" && GOTOOLCHAIN=local GOPROXY=off go build -o "$probe_binary" "$probe")
    chmod 0555 "$probe_binary"
    output="$(env WARP_CONFIG_HOME="$config_root" WARP_VAULT_HOME="$vault_root" \
        WARP_ENV=local WARP_SERVICE=api WARP_DOMAIN=bringyour.com WARP_BLOCK=competition WARP_VERSION=0.0.0 \
        "$probe_binary")"
    [[ "$output" == competition=sim-latency-season-1\ providers=* ]] || die "installed settings probe failed"
    log "competition settings validated (${output#competition=sim-latency-season-1 })"
}

verify_installed_settings() {
    verify_settings "$API_CONFIG_ROOT" "$API_VAULT_ROOT"
}

self_test() {
    local synthetic_selection synthetic_reference seed_key_bytes
    stage="$(mktemp -d "/run/user/$(id -u)/competition-api-stage.XXXXXXXX")"
    prepare_stage false
    cmp -s "$EVALUATION_CONFIG/settings.yml" "$stage/api/config/local/settings.yml" ||
        die "self-test did not copy config/local"
    cmp -s "$EVALUATION_VAULT/auth.yml" "$stage/api/vault/local/auth.yml" ||
        die "self-test did not copy vault/local"
    [ ! -e "$stage/api/config/local/iso-country-list.yml" ] ||
        die "self-test imported config/all"
    [ ! -e "$stage/api/config/local/aws.yml" ] ||
        die "self-test imported config/main"
    [ ! -e "$stage/api/vault/local/google.yml" ] ||
        die "self-test imported vault/main"
    mkdir -m 0700 -p "$stage/api/config/local/mmdb" "$stage/api/config/local/arindb"
    printf '%s\n' synthetic-ipinfo > "$stage/api/config/local/mmdb/ip-ipinfo.mmdb"
    printf '%s\n' synthetic-arin > "$stage/api/config/local/arindb/arin.mmdb"
    chmod 0400 "$stage/api/config/local/mmdb/ip-ipinfo.mmdb" "$stage/api/config/local/arindb/arin.mmdb"
    synthetic_selection="$stage/synthetic-selection.json"
    synthetic_reference="$stage/synthetic-reference.json"
    jq -n \
        --argjson providers "$(jq -er '.evaluation_policy.provider_count' "$ANCHOR")" \
        --argjson clients "$(jq -er '.evaluation_policy.client_pool_size' "$ANCHOR")" \
        --argjson rate "$(jq -er '.evaluation_policy.arrivals_per_minute' "$ANCHOR")" \
        '{provider_count:$providers,client_pool_size:$clients,arrivals_per_minute:$rate,
          replicate_count:9,takeover_margin:0.13}' > "$synthetic_selection"
    jq -n '{schema:1,kind:"synthetic-reference-decision"}' > "$synthetic_reference"
    chmod 0400 "$synthetic_selection" "$synthetic_reference"
    write_resources "$synthetic_selection" "$synthetic_reference" "$stage/libexec" synthetic
    verify_settings "$stage/api/config" "$stage/api/vault"
    jq -e '.schema == 1 and .kind == "sim-latency-competition-api-raw-credentials" and
        (.tokens | keys | sort) == ["apex-submit-a", "apex-submit-b", "competition-operator"] and
        ([.tokens[] | type == "string" and test("^[0-9a-f]{64}$")] | all)' \
        "$stage/api/credentials.json" >/dev/null || die "self-test raw credential structure"
    seed_key_bytes="$(jq -er '.seed_key_base64' "$stage/api/vault/local/competition.yml" | base64 -d | wc -c)" ||
        die "self-test seed key decoding"
    [ "$seed_key_bytes" -eq 32 ] || die "self-test seed key length"
    [ ! -e "$EVALUATION_CONFIG/competition.yml" ] && [ ! -e "$EVALUATION_VAULT/competition.yml" ] ||
        die "self-test crossed evaluation mount boundary"
    jq -e --arg release_amendment "$RELEASE_AMENDMENT_SHA" \
        '.api_resource_scope == ["local","allowlisted-host-runtime-assets","generated-competition"] and
        .nonlocal_configuration_or_vault_imported == false and
        .host_runtime_assets_exposed_to_evaluation == false and
        .production_release_self_check_contract_amendment_sha256 == $release_amendment and
        (.containment_promoter_sha256 | test("^[0-9a-f]{64}$")) and
        (.rebaseline_promoter_sha256 | test("^[0-9a-f]{64}$")) and
        ([.host_runtime_assets[].path] == ["arindb/arin.mmdb","mmdb/ip-ipinfo.mmdb"])' \
        "$stage/api/deployment-manifest.json" >/dev/null ||
        die "self-test API resource scope"
    log "self-test passed: local-only API resources, allowlisted host data, secret generation, frozen settings, and separate-root boundary"
}

preflight() {
    local command
    for command in awk base64 chmod cmp cp docker find git go grep id install jq mktemp mv openssl paste realpath rm rmdir sed sha256sum stat sudo tr wc; do
        command -v "$command" >/dev/null 2>&1 || die "missing command: $command"
    done
    verify_self
    verify_inputs
    verify_terminal_evidence
    log "preflight passed; v5 staging amendment and separate evaluation leaves are authenticated"
}

main() {
    local mode="${1:-}"
    [ "$mode" = --preflight-only ] || [ "$mode" = --self-test ] || [ "$mode" = --install ] ||
        die "usage: $0 {--preflight-only|--self-test|--install}"
    preflight
    [ "$mode" = --preflight-only ] && return 0
    if [ "$mode" = --self-test ]; then
        mkdir -p "/run/user/$(id -u)"
        self_test
        return 0
    fi
    if sudo -n test -e "$API_MANIFEST"; then
        if verify_existing; then
            log "existing competition API deployment authenticated"
            return 0
        fi
        if verify_superseded_existing; then
            archive_superseded_deployment
        else
            die "existing API deployment does not match the current or superseded authenticated release"
        fi
    fi
    mkdir -p "/run/user/$(id -u)"
    stage="$(mktemp -d "/run/user/$(id -u)/competition-api-stage.XXXXXXXX")"
    prepare_stage
    write_resources
    install_bundle
    verify_installed_settings
    verify_existing || die "new API deployment failed authentication"
    [ "$(sudo -n "$HASH_LOCAL" "$EVALUATION_CONFIG")" = "$(jq -er '.host.config_local_sha256' "$SOURCE_LOCK")" ] || die "evaluation config changed during provisioning"
    [ "$(sudo -n "$HASH_LOCAL" "$EVALUATION_VAULT")" = "$(jq -er '.host.vault_local_sha256' "$SOURCE_LOCK")" ] || die "evaluation vault changed during provisioning"
    install_committed=true
    log "competition API config, API-readable vault, root-only raw credentials, and trusted commands provisioned outside evaluation mounts"
}

main "$@"
