# The local tier's SMTP sink (OPS-M9-01): smtpsink's binary on nothing else. stack.sh builds the binary for the
# docker host's architecture into the build context first, so nothing is fetched.
FROM scratch
COPY smtpsink /smtpsink
USER 65532:65532
STOPSIGNAL SIGTERM
ENTRYPOINT ["/smtpsink"]
