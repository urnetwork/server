# sidecar-test-dns-unbound: Unbound for §22.9's DNS row, validating the test edge's answers against the
# root's KSK as its one trust anchor (the anchored monitor's validator, §12.6). Built by build.sh, which
# fetches the package from Alpine's mirrors, so only a person runs it. The test mounts unbound.conf.
FROM alpine:3.22
RUN apk add --no-cache unbound && unbound -V
EXPOSE 53/udp 53/tcp
