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
    #unzip \
    #libnss3 \
    #libatk1.0-0 \
    #libatk-bridge2.0-0 \
    #libcups2 \
    #libxcomposite1 \
    #libxdamage1 \
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

# RUN ln -s /root/.nvm/versions/node/${NODE_VERSION}/bin/node /usr/local/sbin/node

# install innerhtml dependencies
#RUN echo https://playwright.azureedge.net/builds/chromium/1088/chromium-$(echo $TARGETPLATFORM | tr / -).zip
#RUN curl https://playwright.azureedge.net/builds/chromium/1088/chromium-$(echo $TARGETPLATFORM | tr / -).zip -o /usr/local/sbin/chromium.zip
# this creates /usr/local/sbin/chrome-linux
#RUN unzip /usr/local/sbin/chromium.zip
#RUN rm /usr/local/sbin/chromium.zip

# use the debian apt sources to install chromium-browser via apt instead of snap
# see https://www.nom.one/blog/debian-repo-on-ubuntu
#COPY debian/debian-stable.list /etc/apt/sources.list.d/debian-stable.list 
#COPY debian/debian-chromium /etc/apt/preferences.d/debian-chromium
#RUN apt-get update && apt-get install -y \
#    chromium \
# && rm -rf /var/lib/apt/lists/*

ENV PUPPETEER_EXECUTABLE_PATH /usr/bin/chromium
RUN cd /usr/local/sbin && PUPPETEER_SKIP_CHROMIUM_DOWNLOAD=true npm i puppeteer

COPY build/$TARGETPLATFORM/gpt /usr/local/sbin/bringyour-gpt
ADD innerhtml /usr/local/sbin/innerhtml

RUN ln -s /srv/warp/vault/.aws /root/.aws

EXPOSE 80
STOPSIGNAL SIGTERM
CMD ["/usr/local/sbin/bringyour-gpt", "-p", "80"]
