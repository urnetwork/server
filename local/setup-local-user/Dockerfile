# syntax=docker/dockerfile:1
FROM golang:1.23 AS builder
RUN apt update && apt install -y protobuf-compiler
RUN go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
ADD . /build/
WORKDIR /build/connect/protocol
RUN make
RUN mkdir /out
WORKDIR /build/bringyour/local/setup-local-user
# RUN ls -la ../../../connect && exit 1
RUN --mount=type=cache,target=/root/.cache/go-build --mount=type=cache,target=/go/pkg/mod/ CGO_ENABLED=0 go build -o /out/service .

FROM alpine
RUN apk add --no-cache
WORKDIR /app
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /out/service /app
ENTRYPOINT ["/app/service"]