# Docker Official Image index for ubuntu:24.04, resolved 2026-08-16. Pin the
# multi-platform index so amd64 and arm64 builds cannot silently move to a new
# base while preserving the human-readable release tag.
FROM ubuntu:24.04@sha256:561618e2c15bf2397621dd04f96926663a3b5616c189cf7e38db7e82f5c538ea AS runtime-packages
# Reviewed Ubuntu package bytes; see cli/RUNTIME-PACKAGES.md.
ADD --checksum=sha256:6bac2a01979e210d9eac1d4d56747ec709ea60654744d66705dc3c36e7629e50 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/c/ca-certificates/ca-certificates_20260601~24.04.1_all.deb /runtime-packages/all/ca-certificates.deb
ADD --checksum=sha256:3d0955bc049bbcca0f4c3e78a3a8b994593d96db7d84f4320217224433844534 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/o/openssl/libssl3t64_3.0.13-0ubuntu3.15_amd64.deb /runtime-packages/amd64/libssl3t64.deb
ADD --checksum=sha256:bf0b51fb18187c4e1b03900e1049dc219047080ff511aa3bf691dbe24e14d24a https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/o/openssl/libssl3t64_3.0.13-0ubuntu3.15_arm64.deb /runtime-packages/arm64/libssl3t64.deb
ADD --checksum=sha256:1f2401db59a0beffde6cc47bea3664965ab8fa206b7a5ac8f1cc649cfeeb056b https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/o/openssl/openssl_3.0.13-0ubuntu3.15_amd64.deb /runtime-packages/amd64/openssl.deb
ADD --checksum=sha256:818b4ab7092eb3365358ce41896ad08206b30cdb67445d7ec0503262893a0d95 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/o/openssl/openssl_3.0.13-0ubuntu3.15_arm64.deb /runtime-packages/arm64/openssl.deb

FROM ubuntu:24.04@sha256:561618e2c15bf2397621dd04f96926663a3b5616c189cf7e38db7e82f5c538ea
# https://www.docker.com/blog/faster-multi-platform-builds-dockerfile-cross-compilation-guide/

ARG TARGETPLATFORM
ARG warp_env
ENV WARP_ENV=$warp_env

# Install offline, then discard build-only inode cache and wall-clock log.
# Keep the runtime linker cache, package database and generated certificates.
RUN --network=none --mount=from=runtime-packages,source=/runtime-packages,target=/runtime-packages,ro \
    dpkg -i /runtime-packages/"$(dpkg --print-architecture)"/*.deb /runtime-packages/all/*.deb \
    && update-ca-certificates \
    && rm -f /var/cache/ldconfig/aux-cache /var/log/dpkg.log

COPY build/$TARGETPLATFORM/api /usr/local/sbin/bringyour-api

RUN ln -s /srv/warp/vault/.aws /root/.aws

STOPSIGNAL SIGTERM
CMD ["/usr/local/sbin/bringyour-api", "-p", "80"]
