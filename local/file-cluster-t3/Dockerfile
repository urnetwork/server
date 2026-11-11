# T3's file-cluster image (EVSB-M2-17): the minio fork's settle_testhooks build of a commit, and t3tool's fake chrony,
# on nothing else. t3.sh builds both binaries for the docker host's architecture into the build context first; the
# hooked build never ships (D-OPS-20: test hooks sit under a build tag), and FS-114's evidence is this build's run of
# the same commit a cluster runs.
FROM scratch
ARG MINIO_COMMIT
LABEL org.opencontainers.image.revision=$MINIO_COMMIT
COPY minio /minio
COPY t3tool /t3tool
ENTRYPOINT ["/minio"]
