FROM scratch
ARG TARGETPLATFORM
COPY build/$TARGETPLATFORM/competitionworker /competitionworker
USER 65532:65532
ENTRYPOINT ["/competitionworker"]
