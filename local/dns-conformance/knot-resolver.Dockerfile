# sidecar-test-dns-knot-resolver: Knot Resolver for §22.9's DNS row, validating the test edge's answers
# against the root's KSK. Built by build.sh, which fetches the package from Alpine's mirrors, so only a
# person runs it. The test mounts kresd.conf.
FROM alpine:3.22
RUN apk add --no-cache knot-resolver && kresd --version
EXPOSE 53/udp 53/tcp
