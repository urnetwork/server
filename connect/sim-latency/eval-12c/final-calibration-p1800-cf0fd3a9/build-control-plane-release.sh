#!/usr/bin/env bash

# Build the final API/worker/rebaseline release only after latency measurement
# is idle. A failed build is retained under .release-build.* for diagnosis; it
# is never promoted as final evidence.

set -Eeuo pipefail
umask 077
export LANG=C LC_ALL=C

readonly ROOT=/home/by/urnetwork/server/connect/sim-latency/eval-12c/final-calibration-p1800-cf0fd3a9
readonly CONTROL_WORKTREE=/home/by/urnetwork/server-finalization-control-plane
readonly CONTROL_REPOSITORY=/home/by/urnetwork/server
readonly CONTROL=/home/by/urnetwork/server-finalization-release-source-2ee4883f
readonly RELEASE_ROOT="$ROOT/control-plane-release"
readonly FINAL="$RELEASE_ROOT/final"
readonly READINESS_ROOT="$ROOT/production-readiness"
readonly RELEASE_CHECK="$READINESS_ROOT/release-artifacts.json"
readonly CONTROL_COMMIT=2ee4883f2b77cccfcbc69b3bcf1cb4ee613dad36
readonly SOURCE_RELEASE_SHA=90458a61e19259bba1bf1626b63567e92a06082d3944a070a8ea071b5f8bd5e7
readonly SOURCE_LOCK_SHA=0cf71458833f3b1ae96a663357c583eba3a9c25a19d6c795c8549e4154141838
readonly PROTOCOL_SHA=6fc4a809779bf6e694ef3afa71522fa50d0512c56177b42da4249738a37dc7af
readonly EVALUATOR_IMAGE=sha256:cf0fd3a9e73385729ee8dcd8da7ea53eb59d5f372b9ff36789ec923056222038
readonly OPENAPI_SHA=3fadfe3ecc23fcc262776d4e6321001e013a53c501574d32d615eb0f0353c289
readonly BOOT_ID=34760d1b-a0b6-46a0-b8c1-264abd1affba
readonly BUILD_CPU_LIST=0,2,4,6,8,10,12,14,16,18,20,22
readonly BUILDER_NAME=urnetwork-final-release-cf0fd3a9-2ee4883f
readonly BUILDKIT_IMAGE='moby/buildkit:v0.32.2@sha256:28a898719c18a33f4e8000685287fa36fd0dd9560c6440227d3a732d79bb41d8'
readonly SBOM_GENERATOR_IMAGE='docker/buildkit-syft-scanner:stable-1@sha256:ae4f3b554449e7e25548e7d8ccc029d17357348e30c6e3df01b92bc93654d6a9'
readonly API_TAG=urnetwork/sim-latency-competition-api:2ee4883f
readonly WORKER_TAG=urnetwork/sim-latency-competition-worker:2ee4883f
readonly SCRIPT_PATH="$(readlink -f "${BASH_SOURCE[0]}")"
readonly VERIFY_RELEASE_OCI="$ROOT/verify-release-oci.py"
readonly VERIFY_RELEASE_OCI_SHA=b4a0316f591f1963110e5a328adee56a9a6d091a6c1deef8b0e6015d5f9cff2b

log() { printf '[control-plane-release] %s\n' "$*" >&2; }
die() { log "ERROR: $*"; exit 1; }
sha256_file() { sha256sum "$1" | awk '{print $1}'; }
require_command() { command -v "$1" >/dev/null 2>&1 || die "missing command: $1"; }

verify_control_source() {
    [ -d "$CONTROL/.git" ] && [ ! -L "$CONTROL/.git" ] ||
        die "release source is not a standalone Git checkout"
    [ "$(git -C "$CONTROL" rev-parse HEAD)" = "$CONTROL_COMMIT" ] ||
        die "standalone release source commit changed"
    [ -z "$(git -C "$CONTROL" status --porcelain --untracked-files=no)" ] ||
        die "standalone release source tracked worktree is dirty"
    [ -z "$(git -C "$CONTROL" remote)" ] ||
        die "standalone release source must not retain a network-capable remote"
}

prepare_control_source() {
    local pending
    if [ -e "$CONTROL" ] || [ -L "$CONTROL" ]; then
        verify_control_source
        return
    fi
    pending="$(mktemp -d /home/by/urnetwork/.server-finalization-release-source.XXXXXXXX)"
    log "creating offline standalone release source: $pending"
    git clone --local --no-hardlinks --no-checkout "$CONTROL_REPOSITORY" "$pending" >/dev/null
    git -C "$pending" checkout --detach "$CONTROL_COMMIT" >/dev/null
    git -C "$pending" remote remove origin
    [ -d "$pending/.git" ] && [ ! -L "$pending/.git" ] ||
        die "temporary release source has no Git directory"
    [ "$(git -C "$pending" rev-parse HEAD)" = "$CONTROL_COMMIT" ] ||
        die "temporary release source commit mismatch"
    [ -z "$(git -C "$pending" status --porcelain --untracked-files=no)" ] ||
        die "temporary release source is dirty"
    mv "$pending" "$CONTROL"
    verify_control_source
}

self_test() {
    local build_section buildx_version driver_help
    buildx_version="$(sudo -n docker buildx version)"
    driver_help="$(sudo -n docker buildx create --help)"
    build_section="$(awk '/^build_image\(\)/{emit=1} emit {print}' "$SCRIPT_PATH")"
    rg -q 'github\.com/docker/buildx v0\.(1[3-9]|[2-9][0-9])\.' <<<"$buildx_version" ||
        die "Buildx 0.13 or newer is required for multiple exporters: $buildx_version"
    rg -q 'docker-container' <<<"$driver_help" || die "docker-container builder driver is unavailable"
    [ "$(rg -c -- '--output "type=oci,name=' <<<"$build_section")" -eq 1 ] ||
        die "release builder must have one reusable attested OCI exporter"
    [ "$(rg -c -- '--output "type=docker,name=' <<<"$build_section")" -eq 1 ] ||
        die "release builder must have one reusable Docker exporter"
    [ "$(rg -c -- '--provenance=mode=max,version=v1' <<<"$build_section")" -eq 1 ] ||
        die "release builder must request SLSA v1 max provenance"
    [ "$(rg -c -- '--sbom=generator=' <<<"$build_section")" -eq 1 ] ||
        die "release builder must request a digest-pinned SBOM"
    [ "$(rg -c -- '--provenance=false' <<<"$build_section")" -eq 1 ] ||
        die "runtime archive solves must explicitly disable duplicate provenance"
    [ "$(rg -c -- '--sbom=false' <<<"$build_section")" -eq 1 ] ||
        die "runtime archive solves must explicitly disable duplicate SBOMs"
    [ "$(rg -c '^build_image (api|worker) ' <<<"$build_section")" -eq 2 ] ||
        die "release builder must invoke the verified image path for API and worker"
    rg -q 'sudo -n install -d -o root -g root -m 0700 "\$READINESS_ROOT"' "$SCRIPT_PATH" ||
        die "release readiness root must remain privileged"
    rg -q 'sudo -n install -o root -g root -m 0400 "\$release_check_pending" "\$RELEASE_CHECK"' "$SCRIPT_PATH" ||
        die "release readiness record must cross the privileged boundary by exact install"
    ! rg -q '\}\}.*>"\$RELEASE_CHECK"' "$SCRIPT_PATH" ||
        die "release builder writes directly through the privileged readiness boundary"
    [[ "$BUILDKIT_IMAGE" =~ ^moby/buildkit:v0\.32\.2@sha256:[0-9a-f]{64}$ ]] ||
        die "BuildKit image is not digest-pinned"
    [[ "$SBOM_GENERATOR_IMAGE" =~ ^docker/buildkit-syft-scanner:stable-1@sha256:[0-9a-f]{64}$ ]] ||
        die "SBOM generator image is not digest-pinned"
    [ -f "$CONTROL_WORKTREE/.git" ] && [ ! -d "$CONTROL_WORKTREE/.git" ] ||
        die "linked-worktree VCS-stamping regression precondition changed"
    [ "$(sha256_file "$VERIFY_RELEASE_OCI")" = "$VERIFY_RELEASE_OCI_SHA" ] ||
        die "release OCI verifier changed"
    taskset -c 20,22 python3 "$VERIFY_RELEASE_OCI" --self-test >/dev/null ||
        die "release OCI verifier self-test failed"
    verify_control_source
    log "self-test passed: standalone VCS stamping and digest-equivalent OCI/Docker release outputs are pinned"
}

for command in awk cat chmod date docker env file find git go id install jq mktemp mv python3 readlink rg sha256sum stat sudo systemctl tar taskset wc; do
    require_command "$command"
done

case "${1:-}" in
    --self-test)
        [ "$#" -eq 1 ] || die "--self-test takes no arguments"
        sudo -n docker info >/dev/null || die "Docker is unavailable"
        prepare_control_source
        self_test
        exit 0
        ;;
    '') [ "$#" -eq 0 ] || die "unexpected arguments" ;;
    *) die "usage: $0 [--self-test]" ;;
esac

[ "$(cat /proc/sys/kernel/random/boot_id)" = "$BOOT_ID" ] || die "boot identity changed"
[ "$(sha256_file "$ROOT/source-lock.json")" = "$SOURCE_LOCK_SHA" ] || die "source lock changed"
[ "$(sha256_file "$ROOT/production-staging-protocol.json")" = "$PROTOCOL_SHA" ] || die "staging protocol changed"
[ "$(sha256_file "$RELEASE_ROOT/source-release.json")" = "$SOURCE_RELEASE_SHA" ] || die "control-plane source release changed"
[ "$(sha256_file "$VERIFY_RELEASE_OCI")" = "$VERIFY_RELEASE_OCI_SHA" ] || die "release OCI verifier changed"
[ "$(git -C "$CONTROL_WORKTREE" rev-parse HEAD)" = "$CONTROL_COMMIT" ] || die "control-plane commit changed"
[ "$(git -C "$CONTROL_WORKTREE" rev-parse '@{upstream}')" = "$CONTROL_COMMIT" ] || die "pushed control-plane commit changed"
[ -z "$(git -C "$CONTROL_WORKTREE" status --porcelain --untracked-files=no)" ] || die "control-plane tracked worktree is dirty"
prepare_control_source
[ "$(sha256_file /home/by/urnetwork/sn/api/competition.yml)" = "$OPENAPI_SHA" ] || die "competition OpenAPI changed"
sudo -n docker info >/dev/null || die "Docker is unavailable"
[ ! -e "$FINAL" ] || die "final release already exists: $FINAL"
sudo -n install -d -o root -g root -m 0700 "$READINESS_ROOT"
sudo -n test ! -e "$RELEASE_CHECK" || die "release readiness check already exists: $RELEASE_CHECK"

for service in urnetwork-final-calibration-recovery-8c7cfc98.service urnetwork-final-independent-r1-da4ee86a.service; do
    state="$(systemctl is-active "$service" 2>/dev/null || true)"
    case "$state" in
        inactive|failed|unknown) ;;
        *) die "measurement service is not idle: $service ($state)" ;;
    esac
done
[ -z "$(sudo -n docker ps -q --filter label=com.urnetwork.competition.job-id)" ] ||
    die "competition containers remain active"

install -d -m 0700 "$RELEASE_ROOT"
work="$(mktemp -d "$RELEASE_ROOT/.release-build.XXXXXXXX")"
log "retained build workspace: $work"
install -d -m 0700 "$work/binaries" "$work/images/api/build/linux/amd64" "$work/images/worker/build/linux/amd64"
builder_created=false
cleanup_builder() {
    if "$builder_created"; then
        sudo -n docker buildx rm "$BUILDER_NAME" >/dev/null 2>&1 ||
            log "WARNING: could not remove failed release builder $BUILDER_NAME"
    fi
}
trap cleanup_builder EXIT

build_binary() {
    local package="$1" output="$2" log_path="$3"
    log "building $package"
    (
        cd "$CONTROL"
        env -u GOFLAGS GOEXPERIMENT=greenteagc CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
            GOMAXPROCS=12 GOPROXY=off GOTOOLCHAIN=local \
            taskset -c "$BUILD_CPU_LIST" go build -mod=readonly -trimpath -buildvcs=true \
                -o "$output" "$package"
    ) >"$log_path" 2>&1
    chmod 0500 "$output"
    file "$output" | rg -q 'statically linked' || die "$package is not statically linked"
    go version -m "$output" >"$output.go-version.txt"
    rg -q 'CGO_ENABLED=0' "$output.go-version.txt" || die "$package was built with CGO"
    rg -q "vcs.revision=$CONTROL_COMMIT" "$output.go-version.txt" || die "$package revision is unbound"
    rg -q 'vcs.modified=false' "$output.go-version.txt" || die "$package came from a modified tree"
    chmod 0400 "$output.go-version.txt" "$log_path"
}

build_binary ./cli/api "$work/binaries/api" "$work/binaries/api.build.log"
build_binary ./cli/competitionworker "$work/binaries/competitionworker" "$work/binaries/competitionworker.build.log"
build_binary ./cli/competitionrebaseline "$work/binaries/competitionrebaseline" "$work/binaries/competitionrebaseline.build.log"
build_binary ./cli/competitiondbinit "$work/binaries/competitiondbinit" "$work/binaries/competitiondbinit.build.log"

install -m 0400 "$CONTROL/cli/api/Dockerfile" "$work/images/api/Dockerfile"
install -m 0500 "$work/binaries/api" "$work/images/api/build/linux/amd64/api"
install -m 0400 "$CONTROL/cli/competitionworker/Dockerfile" "$work/images/worker/Dockerfile"
install -m 0500 "$work/binaries/competitionworker" "$work/images/worker/build/linux/amd64/competitionworker"

[ "$(find "$work/images/api" -type f | wc -l)" -eq 2 ] || die "API image context widened"
[ "$(find "$work/images/worker" -type f | wc -l)" -eq 2 ] || die "worker image context widened"

if sudo -n docker buildx inspect "$BUILDER_NAME" >/dev/null 2>&1; then
    die "dedicated release builder already exists: $BUILDER_NAME"
fi
log "creating digest-pinned BuildKit builder"
sudo -n docker buildx create \
    --name "$BUILDER_NAME" \
    --driver docker-container \
    --driver-opt "image=$BUILDKIT_IMAGE" >"$work/images/builder.create.log" 2>&1
builder_created=true
sudo -n docker buildx inspect --bootstrap "$BUILDER_NAME" >"$work/images/builder.inspect.log" 2>&1
mapfile -t builder_containers < <(
    sudo -n docker ps -aq --filter "name=buildx_buildkit_${BUILDER_NAME}0"
)
[ "${#builder_containers[@]}" -eq 1 ] || die "dedicated BuildKit container is not unique"
buildkit_image_id="$(sudo -n docker inspect --format '{{.Image}}' "${builder_containers[0]}")"
[[ "$buildkit_image_id" =~ ^sha256:[0-9a-f]{64}$ ]] || die "BuildKit image ID is not immutable"

build_image() {
    local component="$1" context="$2" tag="$3" build_argument="$4"
    local -a build_arguments=()
    if [ -n "$build_argument" ]; then
        build_arguments+=(--build-arg "$build_argument")
    fi

    log "building $component attested OCI archive"
    sudo -n docker buildx build \
        --builder "$BUILDER_NAME" \
        --progress=plain \
        --platform linux/amd64 \
        "${build_arguments[@]}" \
        --metadata-file "$work/images/$component.attested-metadata.json" \
        --provenance=mode=max,version=v1 \
        --sbom=generator="$SBOM_GENERATOR_IMAGE" \
        --no-cache \
        --tag "$tag" \
        --output "type=oci,name=$tag,dest=$work/images/$component.oci.tar" \
        "$context" >"$work/images/$component.attested-build.log" 2>&1

    # Reuse the exact cached result from the attested solve. The Docker exporter
    # cannot carry the attestation manifest list, so a verifier below proves its
    # platform manifest digest is byte-identical to the attested OCI platform.
    log "exporting $component loadable Docker archive from the attested result"
    sudo -n docker buildx build \
        --builder "$BUILDER_NAME" \
        --progress=plain \
        --platform linux/amd64 \
        "${build_arguments[@]}" \
        --metadata-file "$work/images/$component.metadata.json" \
        --provenance=false \
        --sbom=false \
        --tag "$tag" \
        --output "type=docker,name=$tag,dest=$work/images/$component.docker.tar" \
        "$context" >"$work/images/$component.runtime-build.log" 2>&1

    taskset -c 20,22 python3 "$VERIFY_RELEASE_OCI" \
        --attested-archive "$work/images/$component.oci.tar" \
        --attested-metadata "$work/images/$component.attested-metadata.json" \
        --runtime-archive "$work/images/$component.docker.tar" \
        --runtime-metadata "$work/images/$component.metadata.json" \
        --output-dir "$work/images/$component.attestations" \
        --component "$component" \
        --expected-tag "$tag" \
        >"$work/images/$component.verifier.log"
}

build_image api "$work/images/api" "$API_TAG" warp_env=local
build_image worker "$work/images/worker" "$WORKER_TAG" ''
tar -tf "$work/images/api.docker.tar" | rg -qx 'manifest.json' || die "API Docker archive is invalid"
tar -tf "$work/images/worker.docker.tar" | rg -qx 'manifest.json' || die "worker Docker archive is invalid"

sudo -n docker load --input "$work/images/api.docker.tar" >"$work/images/api.load.log"
sudo -n docker load --input "$work/images/worker.docker.tar" >"$work/images/worker.load.log"
api_image_id="$(sudo -n docker image inspect --format '{{.Id}}' "$API_TAG")"
worker_image_id="$(sudo -n docker image inspect --format '{{.Id}}' "$WORKER_TAG")"
[[ "$api_image_id" =~ ^sha256:[0-9a-f]{64}$ ]] || die "API image ID is not immutable"
[[ "$worker_image_id" =~ ^sha256:[0-9a-f]{64}$ ]] || die "worker image ID is not immutable"
[ "$api_image_id" = "$(jq -er '.image_config_digest' "$work/images/api.attestations/verification.json")" ] ||
    die "loaded API image config is not the attested image config"
[ "$worker_image_id" = "$(jq -er '.image_config_digest' "$work/images/worker.attestations/verification.json")" ] ||
    die "loaded worker image config is not the attested image config"

sudo -n docker image inspect "$API_TAG" >"$work/images/api.inspect.json"
sudo -n docker image inspect "$WORKER_TAG" >"$work/images/worker.inspect.json"
if rg -l -i '(seed_key|bearer|password|private[_. -]?key|vault/local|config/all|vault/all)' \
    "$work/images/api.inspect.json" "$work/images/worker.inspect.json" \
    "$work/images/api.metadata.json" "$work/images/worker.metadata.json" \
    "$work/images/api.attested-metadata.json" "$work/images/worker.attested-metadata.json" \
    "$work/images/api.attestations/provenance.json" \
    "$work/images/worker.attestations/provenance.json" \
    "$work/images/api.attestations/verification.json" \
    "$work/images/worker.attestations/verification.json"; then
    die "image metadata contains a secret or forbidden mount marker"
fi
if rg -l -i '(seed_key|private[_. -]?key|vault/local|config/all|vault/all)' \
    "$work/images/api.attestations/sbom.spdx.json" \
    "$work/images/worker.attestations/sbom.spdx.json"; then
    die "image SBOM contains a secret or forbidden mount marker"
fi

sudo -n docker buildx rm "$BUILDER_NAME" >"$work/images/builder.remove.log" 2>&1 ||
    die "could not remove dedicated release builder"
builder_created=false
[ -z "$(sudo -n docker ps -aq --filter "name=buildx_buildkit_${BUILDER_NAME}0")" ] ||
    die "dedicated BuildKit container survived cleanup"

api_binary_sha="$(sha256_file "$work/binaries/api")"
worker_binary_sha="$(sha256_file "$work/binaries/competitionworker")"
rebaseline_binary_sha="$(sha256_file "$work/binaries/competitionrebaseline")"
dbinit_binary_sha="$(sha256_file "$work/binaries/competitiondbinit")"
api_archive_sha="$(sha256_file "$work/images/api.docker.tar")"
worker_archive_sha="$(sha256_file "$work/images/worker.docker.tar")"
api_oci_archive_sha="$(sha256_file "$work/images/api.oci.tar")"
worker_oci_archive_sha="$(sha256_file "$work/images/worker.oci.tar")"
api_attested_metadata_sha="$(sha256_file "$work/images/api.attested-metadata.json")"
worker_attested_metadata_sha="$(sha256_file "$work/images/worker.attested-metadata.json")"
api_provenance_sha="$(sha256_file "$work/images/api.attestations/provenance.json")"
worker_provenance_sha="$(sha256_file "$work/images/worker.attestations/provenance.json")"
api_sbom_sha="$(sha256_file "$work/images/api.attestations/sbom.spdx.json")"
worker_sbom_sha="$(sha256_file "$work/images/worker.attestations/sbom.spdx.json")"
api_verification_sha="$(sha256_file "$work/images/api.attestations/verification.json")"
worker_verification_sha="$(sha256_file "$work/images/worker.attestations/verification.json")"

jq -n \
    --arg generated_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    --arg source_lock_sha256 "$SOURCE_LOCK_SHA" \
    --arg protocol_sha256 "$PROTOCOL_SHA" \
    --arg control_commit "$CONTROL_COMMIT" \
    --arg source_vcs_revision "$(git -C "$CONTROL" rev-parse HEAD)" \
    --arg source_release_sha256 "$SOURCE_RELEASE_SHA" \
    --arg evaluator_image_digest "$EVALUATOR_IMAGE" \
    --arg go_version "$(go version)" \
    --arg api_binary_sha256 "$api_binary_sha" \
    --arg worker_binary_sha256 "$worker_binary_sha" \
    --arg rebaseline_binary_sha256 "$rebaseline_binary_sha" \
    --arg dbinit_binary_sha256 "$dbinit_binary_sha" \
    --arg api_image_id "$api_image_id" \
    --arg worker_image_id "$worker_image_id" \
    --arg api_archive_sha256 "$api_archive_sha" \
    --arg worker_archive_sha256 "$worker_archive_sha" \
    --arg api_oci_archive_sha256 "$api_oci_archive_sha" \
    --arg worker_oci_archive_sha256 "$worker_oci_archive_sha" \
    --arg api_attested_metadata_sha256 "$api_attested_metadata_sha" \
    --arg worker_attested_metadata_sha256 "$worker_attested_metadata_sha" \
    --arg api_metadata_sha256 "$(sha256_file "$work/images/api.metadata.json")" \
    --arg worker_metadata_sha256 "$(sha256_file "$work/images/worker.metadata.json")" \
    --arg api_provenance_sha256 "$api_provenance_sha" \
    --arg worker_provenance_sha256 "$worker_provenance_sha" \
    --arg api_sbom_sha256 "$api_sbom_sha" \
    --arg worker_sbom_sha256 "$worker_sbom_sha" \
    --arg api_verification_sha256 "$api_verification_sha" \
    --arg worker_verification_sha256 "$worker_verification_sha" \
    --arg api_attested_index_digest "$(jq -er '.attested_index_digest' "$work/images/api.attestations/verification.json")" \
    --arg worker_attested_index_digest "$(jq -er '.attested_index_digest' "$work/images/worker.attestations/verification.json")" \
    --arg api_platform_manifest_digest "$(jq -er '.platform_manifest_digest' "$work/images/api.attestations/verification.json")" \
    --arg worker_platform_manifest_digest "$(jq -er '.platform_manifest_digest' "$work/images/worker.attestations/verification.json")" \
    --arg api_dockerfile_sha256 "$(sha256_file "$work/images/api/Dockerfile")" \
    --arg worker_dockerfile_sha256 "$(sha256_file "$work/images/worker/Dockerfile")" \
    --arg buildx_version "$(sudo -n docker buildx version)" \
    --arg buildkit_image_ref "$BUILDKIT_IMAGE" \
    --arg buildkit_image_id "$buildkit_image_id" \
    --arg buildkit_inspect_sha256 "$(sha256_file "$work/images/builder.inspect.log")" \
    --arg sbom_generator_image_ref "$SBOM_GENERATOR_IMAGE" \
    --arg release_oci_verifier_sha256 "$VERIFY_RELEASE_OCI_SHA" \
    --arg openapi_sha256 "$OPENAPI_SHA" \
    '{
      schema:1,
      kind:"sim-latency-control-plane-binary-image-release",
      generated_at:$generated_at,
      source_lock_sha256:$source_lock_sha256,
      production_staging_protocol_sha256:$protocol_sha256,
      control_plane_commit:$control_commit,
      source_checkout:{kind:"standalone-offline-local-clone",git_directory:true,
        network_remotes:0,vcs_revision:$source_vcs_revision,
        linked_worktree_avoided_for_vcs_stamping:true},
      control_plane_source_release_sha256:$source_release_sha256,
      evaluator_image_digest:$evaluator_image_digest,
      go_version:$go_version,
      binaries:{
        api:{path:"binaries/api",sha256:$api_binary_sha256,cgo_enabled:false},
        worker:{path:"binaries/competitionworker",sha256:$worker_binary_sha256,cgo_enabled:false},
        rebaseline:{path:"binaries/competitionrebaseline",sha256:$rebaseline_binary_sha256,cgo_enabled:false},
        dbinit:{path:"binaries/competitiondbinit",sha256:$dbinit_binary_sha256,cgo_enabled:false}
      },
      images:{
        api:{image_id:$api_image_id,
          docker_archive_path:"images/api.docker.tar",docker_archive_sha256:$api_archive_sha256,
          attested_oci_archive_path:"images/api.oci.tar",attested_oci_archive_sha256:$api_oci_archive_sha256,
          metadata_path:"images/api.metadata.json",metadata_sha256:$api_metadata_sha256,
          attested_metadata_path:"images/api.attested-metadata.json",
          attested_metadata_sha256:$api_attested_metadata_sha256,
          attested_index_digest:$api_attested_index_digest,
          platform_manifest_digest:$api_platform_manifest_digest,
          provenance_path:"images/api.attestations/provenance.json",provenance_sha256:$api_provenance_sha256,
          sbom_path:"images/api.attestations/sbom.spdx.json",sbom_sha256:$api_sbom_sha256,
          equivalence_verification_path:"images/api.attestations/verification.json",
          equivalence_verification_sha256:$api_verification_sha256,
          dockerfile_sha256:$api_dockerfile_sha256},
        worker:{image_id:$worker_image_id,
          docker_archive_path:"images/worker.docker.tar",docker_archive_sha256:$worker_archive_sha256,
          attested_oci_archive_path:"images/worker.oci.tar",attested_oci_archive_sha256:$worker_oci_archive_sha256,
          metadata_path:"images/worker.metadata.json",metadata_sha256:$worker_metadata_sha256,
          attested_metadata_path:"images/worker.attested-metadata.json",
          attested_metadata_sha256:$worker_attested_metadata_sha256,
          attested_index_digest:$worker_attested_index_digest,
          platform_manifest_digest:$worker_platform_manifest_digest,
          provenance_path:"images/worker.attestations/provenance.json",provenance_sha256:$worker_provenance_sha256,
          sbom_path:"images/worker.attestations/sbom.spdx.json",sbom_sha256:$worker_sbom_sha256,
          equivalence_verification_path:"images/worker.attestations/verification.json",
          equivalence_verification_sha256:$worker_verification_sha256,
          dockerfile_sha256:$worker_dockerfile_sha256}
      },
      competition_openapi_sha256:$openapi_sha256,
      builder:{driver:"docker-container",buildx_version:$buildx_version,
        image_ref:$buildkit_image_ref,image_id:$buildkit_image_id,
        inspect_sha256:$buildkit_inspect_sha256},
      attestations:{provenance_mode:"max",provenance_version:"v1",
        sbom_format:"SPDX",sbom_generator_image_ref:$sbom_generator_image_ref,
        verifier_sha256:$release_oci_verifier_sha256,
        provenance_verified:true,sbom_verified:true,
        runtime_manifest_digest_equivalent:true},
      image_contexts_contain_config_or_vault:false,
      candidate_base_unchanged:true
    }' >"$work/release-build.json"

jq -e \
    --arg api "$api_image_id" \
    --arg worker "$worker_image_id" \
    '.schema==1 and .images.api.image_id==$api and .images.worker.image_id==$worker and
      .source_checkout.kind=="standalone-offline-local-clone" and
      .source_checkout.vcs_revision==.control_plane_commit and
      .source_checkout.network_remotes==0 and
      .source_checkout.linked_worktree_avoided_for_vcs_stamping==true and
      .builder.driver=="docker-container" and
      .attestations.provenance_verified==true and .attestations.sbom_verified==true and
      .attestations.runtime_manifest_digest_equivalent==true and
      .images.api.image_id==(.images.api | .image_id) and
      .images.api.platform_manifest_digest != .images.api.attested_index_digest and
      .images.worker.platform_manifest_digest != .images.worker.attested_index_digest and
      .image_contexts_contain_config_or_vault==false' \
    "$work/release-build.json" >/dev/null

# The docker-container exporter writes archives and metadata as root. Transfer
# only these exact retained outputs back to the invoking account before sealing
# them; no recursive or unresolved path is accepted here.
release_owner="$(id -u):$(id -g)"
sudo -n chown "$release_owner" "$work/images/"*.json "$work/images/"*.tar
chmod 0400 "$work/release-build.json" "$work/images/"*.json "$work/images/"*.log "$work/images/"*.tar
find "$work/images/api" "$work/images/worker" -type f -exec chmod 0400 {} +
find "$work" -type d -exec chmod 0500 {} +
mv "$work" "$FINAL"
release_manifest_sha="$(sha256_file "$FINAL/release-build.json")"
release_check_pending="$(mktemp "$RELEASE_ROOT/.release-artifacts.XXXXXXXX")"
jq -n \
    --arg generated_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    --arg source_lock_sha256 "$SOURCE_LOCK_SHA" \
    --arg protocol_sha256 "$PROTOCOL_SHA" \
    --arg control_commit "$CONTROL_COMMIT" \
    --arg release_manifest_sha256 "$release_manifest_sha" \
    --arg api_binary_sha256 "$(jq -er '.binaries.api.sha256' "$FINAL/release-build.json")" \
    --arg worker_binary_sha256 "$(jq -er '.binaries.worker.sha256' "$FINAL/release-build.json")" \
    --arg rebaseline_binary_sha256 "$(jq -er '.binaries.rebaseline.sha256' "$FINAL/release-build.json")" \
    --arg dbinit_binary_sha256 "$(jq -er '.binaries.dbinit.sha256' "$FINAL/release-build.json")" \
    --arg api_image_id "$(jq -er '.images.api.image_id' "$FINAL/release-build.json")" \
    --arg worker_image_id "$(jq -er '.images.worker.image_id' "$FINAL/release-build.json")" \
    --arg api_archive_sha256 "$(jq -er '.images.api.docker_archive_sha256' "$FINAL/release-build.json")" \
    --arg worker_archive_sha256 "$(jq -er '.images.worker.docker_archive_sha256' "$FINAL/release-build.json")" \
    --arg api_oci_archive_sha256 "$(jq -er '.images.api.attested_oci_archive_sha256' "$FINAL/release-build.json")" \
    --arg worker_oci_archive_sha256 "$(jq -er '.images.worker.attested_oci_archive_sha256' "$FINAL/release-build.json")" \
    --arg api_metadata_sha256 "$(jq -er '.images.api.metadata_sha256' "$FINAL/release-build.json")" \
    --arg worker_metadata_sha256 "$(jq -er '.images.worker.metadata_sha256' "$FINAL/release-build.json")" \
    --arg api_attested_metadata_sha256 "$(jq -er '.images.api.attested_metadata_sha256' "$FINAL/release-build.json")" \
    --arg worker_attested_metadata_sha256 "$(jq -er '.images.worker.attested_metadata_sha256' "$FINAL/release-build.json")" \
    --arg api_provenance_sha256 "$(jq -er '.images.api.provenance_sha256' "$FINAL/release-build.json")" \
    --arg worker_provenance_sha256 "$(jq -er '.images.worker.provenance_sha256' "$FINAL/release-build.json")" \
    --arg api_sbom_sha256 "$(jq -er '.images.api.sbom_sha256' "$FINAL/release-build.json")" \
    --arg worker_sbom_sha256 "$(jq -er '.images.worker.sbom_sha256' "$FINAL/release-build.json")" \
    --arg api_verification_sha256 "$(jq -er '.images.api.equivalence_verification_sha256' "$FINAL/release-build.json")" \
    --arg worker_verification_sha256 "$(jq -er '.images.worker.equivalence_verification_sha256' "$FINAL/release-build.json")" \
    --arg api_platform_manifest_digest "$(jq -er '.images.api.platform_manifest_digest' "$FINAL/release-build.json")" \
    --arg worker_platform_manifest_digest "$(jq -er '.images.worker.platform_manifest_digest' "$FINAL/release-build.json")" \
    --arg buildkit_image_id "$(jq -er '.builder.image_id' "$FINAL/release-build.json")" \
    --arg buildkit_inspect_sha256 "$(jq -er '.builder.inspect_sha256' "$FINAL/release-build.json")" \
    --arg openapi_sha256 "$OPENAPI_SHA" \
    '{schema:1,kind:"sim-latency-production-readiness-check",check_id:"release_artifacts",
      passed:true,generated_at:$generated_at,source_lock_sha256:$source_lock_sha256,
      production_staging_protocol_sha256:$protocol_sha256,control_plane_commit:$control_commit,
      evidence_sha256:{release_manifest:$release_manifest_sha256,
        binaries:{api:$api_binary_sha256,worker:$worker_binary_sha256,
          rebaseline:$rebaseline_binary_sha256,dbinit:$dbinit_binary_sha256},
        images:{api:{image_id:$api_image_id,docker_archive_sha256:$api_archive_sha256,
            attested_oci_archive_sha256:$api_oci_archive_sha256,
            metadata_sha256:$api_metadata_sha256,
            attested_metadata_sha256:$api_attested_metadata_sha256,
            platform_manifest_digest:$api_platform_manifest_digest,
            provenance_sha256:$api_provenance_sha256,sbom_sha256:$api_sbom_sha256,
            equivalence_verification_sha256:$api_verification_sha256},
          worker:{image_id:$worker_image_id,docker_archive_sha256:$worker_archive_sha256,
            attested_oci_archive_sha256:$worker_oci_archive_sha256,
            metadata_sha256:$worker_metadata_sha256,
            attested_metadata_sha256:$worker_attested_metadata_sha256,
            platform_manifest_digest:$worker_platform_manifest_digest,
            provenance_sha256:$worker_provenance_sha256,sbom_sha256:$worker_sbom_sha256,
            equivalence_verification_sha256:$worker_verification_sha256}},
        builder:{image_id:$buildkit_image_id,inspect_sha256:$buildkit_inspect_sha256},
        openapi:$openapi_sha256},
      assertions:{api_binary_cgo_disabled:true,worker_binary_cgo_disabled:true,
        rebaseline_binary_cgo_disabled:true,dbinit_binary_cgo_disabled:true,
        api_image_sha256_pinned:true,worker_image_sha256_pinned:true,
        openapi_hash_verified:true,source_commit_verified:true,
        digest_pinned_buildkit_verified:true,digest_pinned_sbom_generator_verified:true,
        api_slsa_v1_provenance_verified:true,worker_slsa_v1_provenance_verified:true,
        api_spdx_sbom_verified:true,worker_spdx_sbom_verified:true,
        no_config_or_vault_in_images:true}}' >"$release_check_pending"
jq -e '.schema == 1 and .passed == true and (.assertions | length) == 15 and
    ([.assertions[]] | all)' "$release_check_pending" >/dev/null || die "release readiness record is invalid"
chmod 0400 "$release_check_pending"
release_check_sha="$(sha256_file "$release_check_pending")"
sudo -n install -o root -g root -m 0400 "$release_check_pending" "$RELEASE_CHECK"
[ "$(sudo -n sha256sum "$RELEASE_CHECK" | awk '{print $1}')" = "$release_check_sha" ] ||
    die "installed release readiness record changed"
rm -f -- "$release_check_pending"
log "final control-plane release: $FINAL"
printf '%s %s\n' "$release_manifest_sha" "$release_check_sha"
