FROM golang:1.23 AS builder
WORKDIR /build
ADD . /build
RUN --mount=type=cache,target=/root/.cache/go-build --mount=type=cache,target=/go/pkg/mod/ CGO_ENABLED=0 go build -o /out/service .

FROM alpine
RUN apk add --no-cache
WORKDIR /app
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /out/service /app
ENTRYPOINT ["/app/service"]
