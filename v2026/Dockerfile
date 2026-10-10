# syntax=docker/dockerfile:1
FROM golang:1.23 AS builder
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
