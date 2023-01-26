FROM ubuntu:22.04
COPY bin/echo /usr/local/bin/echo
EXPOSE 80
STOPSIGNAL SIGTERM
CMD ["/usr/local/bin/echo", "--port", "80", "--message", "edge-2023.01.1-beta", ";"]
