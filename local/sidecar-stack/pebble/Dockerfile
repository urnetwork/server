# The local tier's ACME server (TLS-M5-26): pebble's binary on nothing else. stack.sh builds the binary for the docker
# host's architecture into the build context first, from the Pebble module server's go.sum pins, so nothing is fetched.
FROM scratch
COPY pebble /pebble
USER 65532:65532
STOPSIGNAL SIGTERM
ENTRYPOINT ["/pebble"]
