# syntax=docker/dockerfile:1.7
ARG C1_BASE_IMAGE=urnetwork/sim-latency-evaluator-base:canceled-stop-c1f9dda6-go1273
FROM ${C1_BASE_IMAGE} AS c2-source
ARG C2_SHA=a9e8e1a710b24c749e6499a0606938252c83e136
ARG SOURCE_LOCK_SHA256=239cea7fe6f77f989b75ba6efa677e8731640ba2fdd3690a35e6fe72103f3d11
ARG SOURCE_CONFIG_SHA256=589ab624be8301c22e01571ac33b3e1b44e419ffb68009a577e6d520c0bb8995
USER 0:0
COPY source/ /workspace/
COPY --chmod=0444 source-lock.json /opt/urnetwork/source-lock.json
COPY --chmod=0444 sim-latency.yml /opt/urnetwork/sim-latency.yml
COPY --chmod=0444 canonical.patch /opt/urnetwork/submission/canonical.patch
RUN --network=none set -eu; \
    go version; \
    go env GOEXPERIMENT CGO_ENABLED GOMODCACHE GOPATH GOTOOLCHAIN; \
    test "$(go env GOVERSION)" = go1.27.3; \
    test "$(sha256sum /opt/urnetwork/source-lock.json | awk '{print $1}')" = "${SOURCE_LOCK_SHA256}"; \
    test "$(sha256sum /opt/urnetwork/sim-latency.yml | awk '{print $1}')" = "${SOURCE_CONFIG_SHA256}"; \
    jq -e --arg server "${C2_SHA}" '.schema == 1 and .development_snapshot == false and .candidate_path_base == "mono_root_v1" and .repositories.server == $server and (.repositories|length) == 10' /opt/urnetwork/source-lock.json >/dev/null; \
    for repository in server connect sdk proxy glog goidenticons userwireguard sn warp gvisor; do \
      expected="$(jq -er --arg repository "$repository" '.repositories[$repository]' /opt/urnetwork/source-lock.json)"; \
      test "$(git -C "/workspace/$repository" rev-parse HEAD)" = "$expected"; \
      status="$(git -C "/workspace/$repository" status --porcelain=v1 --untracked-files=all)"; \
      test -z "$status"; \
    done; \
    chmod -R go-w,a+rX /workspace

# Compilation executes no package initialization. No test command is in this
# definition: independent low-reasoning qualification owns all test execution.
FROM c2-source AS c2-binary
ARG C2_SHA
USER 0:0
RUN install -d -o 65532 -g 65532 -m 0700 /opt/urnetwork/c2-build/cache /opt/urnetwork/c2-build/tmp /opt/urnetwork/c2-build/output \
    && cp -R /opt/urnetwork/go/build-cache/. /opt/urnetwork/c2-build/cache/ \
    && chown -R 65532:65532 /opt/urnetwork/c2-build \
    && chown 65532:65532 /opt/urnetwork/go/pkg/mod/cache/download/github.com/urnetwork/server/@v
USER 65532:65532
WORKDIR /workspace/server
RUN --network=none \
    GOMAXPROCS=2 GOCACHE=/opt/urnetwork/c2-build/cache GOTMPDIR=/opt/urnetwork/c2-build/tmp TMPDIR=/opt/urnetwork/c2-build/tmp \
    GOFLAGS='-mod=readonly -p=2' GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local \
    go build -trimpath -o /opt/urnetwork/c2-build/output/sim-latency ./connect/sim-latency \
    && test "$(go version -m /opt/urnetwork/c2-build/output/sim-latency | sed -n 's/.*vcs.revision=//p')" = "${C2_SHA}" \
    && test "$(go version -m /opt/urnetwork/c2-build/output/sim-latency | sed -n 's/.*vcs.modified=//p')" = false

FROM c2-source AS final
ARG C2_SHA
ARG SOURCE_LOCK_SHA256
ARG SOURCE_CONFIG_SHA256
USER 0:0
COPY --from=c2-binary --chmod=0555 /opt/urnetwork/c2-build/output/sim-latency /opt/urnetwork/bin/sim-latency
RUN --network=none jq -n \
    --arg base_sha "${C2_SHA}" --arg source_config_sha256 "${SOURCE_CONFIG_SHA256}" \
    --arg source_lock_sha256 "${SOURCE_LOCK_SHA256}" \
    --arg simulator_sha256 "$(sha256sum /opt/urnetwork/bin/sim-latency | awk '{print $1}')" \
    '{schema:1,image_kind:"evaluator-base",base_sha:$base_sha,build_sha:$base_sha,source_epoch:0,source_config_sha256:$source_config_sha256,patch_sha256:"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",policy_sha256:null,source_lock_sha256:$source_lock_sha256,simulator_sha256:$simulator_sha256,paths:[]}' \
    > /opt/urnetwork/image-identity.json \
    && chmod 0444 /opt/urnetwork/image-identity.json
LABEL org.opencontainers.image.revision="${C2_SHA}" \
      com.urnetwork.competition.image-kind="evaluator-base" \
      com.urnetwork.competition.base-sha="${C2_SHA}" \
      com.urnetwork.competition.source-epoch="0" \
      com.urnetwork.competition.source-lock-sha256="${SOURCE_LOCK_SHA256}" \
      com.urnetwork.competition.source-config-sha256="${SOURCE_CONFIG_SHA256}" \
      com.urnetwork.competition.candidate-path-base="mono_root_v1" \
      com.urnetwork.competition.development-snapshot="false" \
      com.urnetwork.competition.staging="false"
USER 65532:65532
WORKDIR /workspace/server
CMD ["identity"]
