# this docker is based on debian because
# - it needs to use headless chrome for scraping (puppeteer) 
# - ubuntu will not install chromium or firefox via apt anymore
# - snap which is now required to install chromium or firefox does not work without systemd
# - systemd is difficult to run inside a container
FROM debian:12.4
# https://www.docker.com/blog/faster-multi-platform-builds-dockerfile-cross-compilation-guide/

ARG TARGETPLATFORM
ARG warp_env
ENV WARP_ENV=$warp_env

# see https://docs.docker.com/develop/develop-images/dockerfile_best-practices/
RUN apt-get update && apt-get install -y \
    ca-certificates \
    curl \
    chromium \
 && rm -rf /var/lib/apt/lists/*
RUN update-ca-certificates

# install node
ENV NODE_VERSION=v20.10.0
RUN curl -o- https://raw.githubusercontent.com/nvm-sh/nvm/v0.39.7/install.sh | bash
ENV NVM_DIR=/root/.nvm
RUN . "$NVM_DIR/nvm.sh" && nvm install ${NODE_VERSION}
RUN . "$NVM_DIR/nvm.sh" && nvm use ${NODE_VERSION}
RUN . "$NVM_DIR/nvm.sh" && nvm alias default ${NODE_VERSION}
ENV PATH="/root/.nvm/versions/node/${NODE_VERSION}/bin/:${PATH}"
RUN node --version
RUN npm --version

# install pupeteer
ENV PUPPETEER_EXECUTABLE_PATH=/usr/bin/chromium
RUN cd /usr/local/sbin && PUPPETEER_SKIP_CHROMIUM_DOWNLOAD=true npm i puppeteer puppeteer-extra puppeteer-extra-plugin-stealth

COPY build/$TARGETPLATFORM/gpt /usr/local/sbin/bringyour-gpt
ADD innerhtml /usr/local/sbin/innerhtml

RUN ln -s /srv/warp/vault/.aws /root/.aws

EXPOSE 80
STOPSIGNAL SIGTERM
CMD ["/usr/local/sbin/bringyour-gpt", "-p", "80"]
