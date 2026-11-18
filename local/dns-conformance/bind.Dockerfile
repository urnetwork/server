# sidecar-test-dns-bind: BIND's named for §22.9's DNS row, forwarding the test root to the test edge and
# validating against the root's KSK. Built by build.sh, which fetches the package from Alpine's mirrors, so
# only a person runs it. The test mounts named.conf.
FROM alpine:3.22
RUN apk add --no-cache bind && mkdir -p /var/bind && named -v
EXPOSE 53/udp 53/tcp
