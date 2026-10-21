# syntax=docker/dockerfile:1
FROM golang:1.27.3@sha256:6928d94d1142af58816fed5a62338d605a266ccd53a0deaab6b1096f19059261 AS builder
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
