FROM ubuntu:22.04
# https://www.docker.com/blog/faster-multi-platform-builds-dockerfile-cross-compilation-guide/
ARG TARGETPLATFORM

# see https://docs.docker.com/develop/develop-images/dockerfile_best-practices/
RUN apt-get update && apt-get install -y \
	ca-certificates \
 && rm -rf /var/lib/apt/lists/*
RUN update-ca-certificates

COPY bin/$TARGETPLATFORM/api /usr/local/bin/bringyour-api
# HOME is /root on our base container
ENV BRINGYOUR_HOME /root/bringyour
RUN mkdir -p $BRINGYOUR_HOME
# note the .env and .settings.yml are typically from the mounted keys home
# RUN echo -n "canary" > $BRINGYOUR_HOME/.env
RUN echo -n "2023.01.1-local" > $BRINGYOUR_HOME/.version

# fixme copy .env and .version into the BRINGYOUR_HOME dir
EXPOSE 80
STOPSIGNAL SIGTERM
CMD ["/usr/local/bin/bringyour-api", "-p", "80"]
