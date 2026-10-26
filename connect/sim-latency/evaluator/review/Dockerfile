# Build context contains only the trusted static manager and public CA bundle.
# Never use a repository root, config, vault or candidate checkout as context.
FROM scratch
COPY --chmod=0555 sim-latency /review-broker
COPY --chmod=0444 ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
USER 65532:65532
ENTRYPOINT ["/review-broker", "review-broker", "--model-key-file=/run/secrets/model-key"]
