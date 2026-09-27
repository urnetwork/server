FROM ubuntu:24.04@sha256:561618e2c15bf2397621dd04f96926663a3b5616c189cf7e38db7e82f5c538ea
# https://www.docker.com/blog/faster-multi-platform-builds-dockerfile-cross-compilation-guide/

ARG TARGETPLATFORM
ARG warp_env
ENV WARP_ENV=$warp_env

# see https://docs.docker.com/develop/develop-images/dockerfile_best-practices/
RUN apt-get update && apt-get install -y \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*
RUN update-ca-certificates

COPY build/$TARGETPLATFORM/alt /usr/local/sbin/bringyour-alt

RUN ln -s /srv/warp/vault/.aws /root/.aws

STOPSIGNAL SIGTERM
CMD ["/usr/local/sbin/bringyour-alt", "-p", "80"]
