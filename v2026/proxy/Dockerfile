FROM ubuntu:24.04
# https://www.docker.com/blog/faster-multi-platform-builds-dockerfile-cross-compilation-guide/

ARG TARGETPLATFORM
ARG warp_env
ENV WARP_ENV=$warp_env

# see https://docs.docker.com/develop/develop-images/dockerfile_best-practices/
RUN apt-get update && apt-get install -y \
    ca-certificates \
    curl \
 && rm -rf /var/lib/apt/lists/*
RUN update-ca-certificates

COPY build/$TARGETPLATFORM/proxy /usr/local/sbin/bringyour-proxy

RUN ln -s /srv/warp/vault/.aws /root/.aws

STOPSIGNAL SIGTERM
CMD ["/usr/local/sbin/bringyour-proxy"]
