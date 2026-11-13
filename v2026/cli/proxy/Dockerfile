FROM ubuntu:24.04@sha256:561618e2c15bf2397621dd04f96926663a3b5616c189cf7e38db7e82f5c538ea AS runtime-packages
# Reviewed Ubuntu package bytes; see cli/RUNTIME-PACKAGES.md.
ADD --checksum=sha256:6bac2a01979e210d9eac1d4d56747ec709ea60654744d66705dc3c36e7629e50 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/c/ca-certificates/ca-certificates_20260601~24.04.1_all.deb /runtime-packages/all/ca-certificates.deb
ADD --checksum=sha256:4358c023e3c1a74bb1e77935807ea2088b137a898167fca278a1767c353ac1c5 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/c/curl/curl_8.5.0-2ubuntu10.15_amd64.deb /runtime-packages/amd64/curl.deb
ADD --checksum=sha256:2decbd1b17f21a8914a1198d90cef891fdb71c7e41e7dab15248807e316bac72 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/c/curl/curl_8.5.0-2ubuntu10.15_arm64.deb /runtime-packages/arm64/curl.deb
ADD --checksum=sha256:ed5e7e254c244ff686c4c9722ff613c6748ce6317c30ff2ce3c3e3066023f6c8 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/k/krb5/krb5-locales_1.20.1-6ubuntu2.10_all.deb /runtime-packages/all/krb5-locales.deb
ADD --checksum=sha256:74492419b8fda803774b8c9acef6afc5d2f9ff31782635aae212906adae7b277 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/b/brotli/libbrotli1_1.1.0-2build2_amd64.deb /runtime-packages/amd64/libbrotli1.deb
ADD --checksum=sha256:cabf3462d908e72f2e594f19ae87c581c4614f099947e38fbd865bf8eae27014 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/b/brotli/libbrotli1_1.1.0-2build2_arm64.deb /runtime-packages/arm64/libbrotli1.deb
ADD --checksum=sha256:02f8f39727a43d5a7057cba35cda866be00d929b91ea67ed02dd9d6402fa551c https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/c/curl/libcurl4t64_8.5.0-2ubuntu10.15_amd64.deb /runtime-packages/amd64/libcurl4t64.deb
ADD --checksum=sha256:aef804d64b33df366c5126585400280b19a6cda4b54e8ed3fee727c17930ee0c https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/c/curl/libcurl4t64_8.5.0-2ubuntu10.15_arm64.deb /runtime-packages/arm64/libcurl4t64.deb
ADD --checksum=sha256:0d4a0187bcdbbe3e6a10bfa574100087d12052d0d0ef28614fd6210021c587b2 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/k/krb5/libgssapi-krb5-2_1.20.1-6ubuntu2.10_amd64.deb /runtime-packages/amd64/libgssapi-krb5-2.deb
ADD --checksum=sha256:7a13651d117e3b8f06139f4aac2184d4efaa8b5d375954b5229f0a332b3d6aeb https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/k/krb5/libgssapi-krb5-2_1.20.1-6ubuntu2.10_arm64.deb /runtime-packages/arm64/libgssapi-krb5-2.deb
ADD --checksum=sha256:273f7cc95a68d927d7f71c3e78b7717a16a8d86e646a206bae7bf797150ae9db https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/k/krb5/libk5crypto3_1.20.1-6ubuntu2.10_amd64.deb /runtime-packages/amd64/libk5crypto3.deb
ADD --checksum=sha256:f05865643982afcec53b08c20e5e69be364f632dd63342009eec99f2ceb180c9 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/k/krb5/libk5crypto3_1.20.1-6ubuntu2.10_arm64.deb /runtime-packages/arm64/libk5crypto3.deb
ADD --checksum=sha256:0679f198b0128179e46cdf956fb2022c23c758664c00bc8efa0382d509683a8a https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/k/keyutils/libkeyutils1_1.6.3-3build1_amd64.deb /runtime-packages/amd64/libkeyutils1.deb
ADD --checksum=sha256:d141e94ed3b32ea6540f2e927a83b3d42cd4378256a1aad60cda583ffdbd78af https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/k/keyutils/libkeyutils1_1.6.3-3build1_arm64.deb /runtime-packages/arm64/libkeyutils1.deb
ADD --checksum=sha256:60b48c5a3233f1d8caba30d6573edc8e131911ab6d63adcd664f7cbe62708362 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/k/krb5/libkrb5-3_1.20.1-6ubuntu2.10_amd64.deb /runtime-packages/amd64/libkrb5-3.deb
ADD --checksum=sha256:0ba099c96bfe18c168e0c12f92408c7a389566692db959be4afc88bd23466963 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/k/krb5/libkrb5-3_1.20.1-6ubuntu2.10_arm64.deb /runtime-packages/arm64/libkrb5-3.deb
ADD --checksum=sha256:9224ede3246a82a845ea97652ecfae57f88e8d8f520d8a186f9157167574fb34 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/k/krb5/libkrb5support0_1.20.1-6ubuntu2.10_amd64.deb /runtime-packages/amd64/libkrb5support0.deb
ADD --checksum=sha256:62d276c37b371c0d48de636d14f55d12f93aa18164ffe16d6fa66a81caeacf74 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/k/krb5/libkrb5support0_1.20.1-6ubuntu2.10_arm64.deb /runtime-packages/arm64/libkrb5support0.deb
ADD --checksum=sha256:d509c254b8fea34f1e8186f6fccef1a499798931c471108c63529bcca9e40516 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/o/openldap/libldap-common_2.6.10+dfsg-0ubuntu0.24.04.1_all.deb /runtime-packages/all/libldap-common.deb
ADD --checksum=sha256:7f3f8e565401256f21d5aa562c9f92dbb63537b73aaea6283e4db5264f1598f4 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/o/openldap/libldap2_2.6.10+dfsg-0ubuntu0.24.04.1_amd64.deb /runtime-packages/amd64/libldap2.deb
ADD --checksum=sha256:60893ca2782c2e52dc3b7b192be0169228beb91f7c87e93abaeca5871c5f16ee https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/o/openldap/libldap2_2.6.10+dfsg-0ubuntu0.24.04.1_arm64.deb /runtime-packages/arm64/libldap2.deb
ADD --checksum=sha256:73adcbb9df32cd7c9d46b1c7598bbbbcf249be705d62d966694cfc9976a15dfe https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/n/nghttp2/libnghttp2-14_1.59.0-1ubuntu0.4_amd64.deb /runtime-packages/amd64/libnghttp2-14.deb
ADD --checksum=sha256:ec812e0b82dfb315b140020f4c8434191a89cf42c49dbb82605d0892b6783b59 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/n/nghttp2/libnghttp2-14_1.59.0-1ubuntu0.4_arm64.deb /runtime-packages/arm64/libnghttp2-14.deb
ADD --checksum=sha256:a6c85d1303ae90b6a3209d73c4f047f82c27cdc963c48adfd95dd7abca64f039 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/libp/libpsl/libpsl5t64_0.21.2-1.1build1_amd64.deb /runtime-packages/amd64/libpsl5t64.deb
ADD --checksum=sha256:886d485c74567b855bcd19b6f353a769a08b58c111b2d8275a908a469c15d4bf https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/libp/libpsl/libpsl5t64_0.21.2-1.1build1_arm64.deb /runtime-packages/arm64/libpsl5t64.deb
ADD --checksum=sha256:967a39dbc14236d1580ede01d80fd78444668572e716734e1ac66c175052594e https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/r/rtmpdump/librtmp1_2.4+20151223.gitfa8646d.1-2build7_amd64.deb /runtime-packages/amd64/librtmp1.deb
ADD --checksum=sha256:fb771b2743120d9d44ee9989fd485205680976f7bf89f2f1ac5df50238ea1db7 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/r/rtmpdump/librtmp1_2.4+20151223.gitfa8646d.1-2build7_arm64.deb /runtime-packages/arm64/librtmp1.deb
ADD --checksum=sha256:eda097f98dcb3a08b9ce157d6191d140e4885c1cba47b683c94b8ca45e88f458 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/c/cyrus-sasl2/libsasl2-2_2.1.28+dfsg1-5ubuntu3.1_amd64.deb /runtime-packages/amd64/libsasl2-2.deb
ADD --checksum=sha256:05da24adf2b95285ce7eaf9a2ea902fbf4f756a796c8adbfd221d746035df44b https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/c/cyrus-sasl2/libsasl2-2_2.1.28+dfsg1-5ubuntu3.1_arm64.deb /runtime-packages/arm64/libsasl2-2.deb
ADD --checksum=sha256:1f13548b1774cd9c70c50b8c3267204a101334a4d2f979338896ba5a4c6f81b8 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/c/cyrus-sasl2/libsasl2-modules-db_2.1.28+dfsg1-5ubuntu3.1_amd64.deb /runtime-packages/amd64/libsasl2-modules-db.deb
ADD --checksum=sha256:ee8237dfd0d26df4d5fbfd3a9b132ae9f34f05b4508176b8bd1839f12739d94c https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/c/cyrus-sasl2/libsasl2-modules-db_2.1.28+dfsg1-5ubuntu3.1_arm64.deb /runtime-packages/arm64/libsasl2-modules-db.deb
ADD --checksum=sha256:a05457e0cc509235bcca25ab857ceb29465bfa23c559036f15b3ce557c8a5153 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/c/cyrus-sasl2/libsasl2-modules_2.1.28+dfsg1-5ubuntu3.1_amd64.deb /runtime-packages/amd64/libsasl2-modules.deb
ADD --checksum=sha256:66af881dd52290f4c54dff352cfffd6ebe3aecdafe611f927fd98f77c729ea81 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/c/cyrus-sasl2/libsasl2-modules_2.1.28+dfsg1-5ubuntu3.1_arm64.deb /runtime-packages/arm64/libsasl2-modules.deb
ADD --checksum=sha256:2448ce5f99e49721d12f8b227f2472dd08ed7a78184dd819fd03a42693024a16 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/libs/libssh/libssh-4_0.10.6-2ubuntu0.5_amd64.deb /runtime-packages/amd64/libssh-4.deb
ADD --checksum=sha256:f1b8da917a3d65ca77918027206b55b69d62795df2676c39828d4439bc97e68f https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/libs/libssh/libssh-4_0.10.6-2ubuntu0.5_arm64.deb /runtime-packages/arm64/libssh-4.deb
ADD --checksum=sha256:3d0955bc049bbcca0f4c3e78a3a8b994593d96db7d84f4320217224433844534 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/o/openssl/libssl3t64_3.0.13-0ubuntu3.15_amd64.deb /runtime-packages/amd64/libssl3t64.deb
ADD --checksum=sha256:bf0b51fb18187c4e1b03900e1049dc219047080ff511aa3bf691dbe24e14d24a https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/o/openssl/libssl3t64_3.0.13-0ubuntu3.15_arm64.deb /runtime-packages/arm64/libssl3t64.deb
ADD --checksum=sha256:1f2401db59a0beffde6cc47bea3664965ab8fa206b7a5ac8f1cc649cfeeb056b https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/o/openssl/openssl_3.0.13-0ubuntu3.15_amd64.deb /runtime-packages/amd64/openssl.deb
ADD --checksum=sha256:818b4ab7092eb3365358ce41896ad08206b30cdb67445d7ec0503262893a0d95 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/o/openssl/openssl_3.0.13-0ubuntu3.15_arm64.deb /runtime-packages/arm64/openssl.deb
ADD --checksum=sha256:46dab71c8842758734d7ff38b46fbf5d8756158e6a374832b3004a53946f637c https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/p/publicsuffix/publicsuffix_20231001.0357-0.1_all.deb /runtime-packages/all/publicsuffix.deb

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

COPY build/$TARGETPLATFORM/proxy /usr/local/sbin/bringyour-proxy

RUN ln -s /srv/warp/vault/.aws /root/.aws

STOPSIGNAL SIGTERM
CMD ["/usr/local/sbin/bringyour-proxy"]
