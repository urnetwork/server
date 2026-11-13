# syntax=docker/dockerfile:1
FROM golang:1.27.4@sha256:08bcb99a38f85a265f9a93d1d3323e2440eec333380f3ea26ee0dec9264bb41c AS builder
ADD . /build/
RUN mkdir /out
WORKDIR /build/
RUN --mount=type=cache,target=/root/.cache/go-build --mount=type=cache,target=/go/pkg/mod/ CGO_ENABLED=0 go build -o /out/ ./...
RUN rm /out/measure-throughput

FROM alpine
RUN apk add --no-cache
WORKDIR /app
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /out/* /app/
ENTRYPOINT []
CMD ["/bin/sh", "-c", "echo 'available binaries:' /app/*"]
