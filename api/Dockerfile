FROM ubuntu:22.04
# https://www.docker.com/blog/faster-multi-platform-builds-dockerfile-cross-compilation-guide/
ARG TARGETPLATFORM
ENV MESSAGE api
COPY bin/$TARGETPLATFORM/echo /usr/local/bin/echo
EXPOSE 80
STOPSIGNAL SIGTERM
CMD ["/usr/local/bin/echo", "--port", "80", ";"]
