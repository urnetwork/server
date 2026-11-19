#!/usr/bin/env bash
# The sidecar's local stack (OPS-M9-01, TLS-M5-26): the compose file's SMTP sink and pebble/'s ACME server.
#
# usage: local/sidecar-stack/stack.sh build          builds smtpsink and pebble for the docker host and the images
#                                                    sidecar-test-smtp-sink:local and sidecar-test-pebble:local
#        local/sidecar-stack/stack.sh up             starts the sink and waits for its control port
#        local/sidecar-stack/stack.sh check          sends the sink one message, reads it back and forgets it
#        local/sidecar-stack/stack.sh down           stops and removes the sink's container and its network, and
#                                                    nothing else: never with --remove-orphans
#        local/sidecar-stack/stack.sh status
#        local/sidecar-stack/stack.sh pebble-up      starts Pebble and waits for its management port
#        local/sidecar-stack/stack.sh pebble-check   reads Pebble's root and its ACME directory through its own
#                                                    certificate
#        local/sidecar-stack/stack.sh pebble-down    stops and removes Pebble's container and its network
#
# GO names the go to build with. Everything binds to 127.0.0.1 above port 50000 and is named sidecar-test-*.
# Nothing is fetched: the image is FROM scratch with the one binary built here.
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
server=$(cd "$here/../.." && pwd)
GO=${GO:-go}
compose=(docker compose -f "$here/docker-compose.yml")
pebble_compose=(docker compose -f "$here/pebble/docker-compose.yml")
smtp_port=52525
control_port=52526
pebble_acme_port=51400
pebble_management_port=51500

build() {
  local arch context
  arch=$(docker version --format '{{.Server.Arch}}')
  context=$(mktemp -d)
  trap 'rm -rf "$context"' RETURN
  (cd "$server" && GOOS=linux GOARCH="$arch" CGO_ENABLED=0 "$GO" build -trimpath -o "$context/smtpsink" ./local/sidecar-stack/smtpsink)
  docker build --quiet -f "$here/Dockerfile" -t sidecar-test-smtp-sink:local "$context"
  echo "sidecar-test-smtp-sink:local: smtpsink for linux/$arch"
  (cd "$server" && GOOS=linux GOARCH="$arch" CGO_ENABLED=0 "$GO" build -trimpath -o "$context/pebble" ./local/sidecar-stack/pebble)
  docker build --quiet -f "$here/pebble/Dockerfile" -t sidecar-test-pebble:local "$context"
  echo "sidecar-test-pebble:local: pebble for linux/$arch"
}

pebble_serves() {
  curl -sf -o /dev/null "http://127.0.0.1:$pebble_management_port/status"
}

pebble_up() {
  local waited=0
  "${pebble_compose[@]}" up -d
  until pebble_serves; do
    waited=$((waited + 1))
    [ $waited -lt 300 ] || { echo "Pebble at 127.0.0.1:$pebble_management_port did not come up" >&2; return 1; }
    sleep 0.1
  done
  echo "Pebble's directory is https://127.0.0.1:$pebble_acme_port/dir, its root and certificate on 127.0.0.1:$pebble_management_port"
}

# Pebble's root parses, and its directory answers through the certificate its management port hands out.
pebble_check() {
  local trust directory
  trust=$(mktemp)
  trap 'rm -f "$trust"' RETURN
  curl -sf "http://127.0.0.1:$pebble_management_port/roots/0" | grep -q 'BEGIN CERTIFICATE' || { echo "Pebble hands out no root" >&2; return 1; }
  curl -sf -o "$trust" "http://127.0.0.1:$pebble_management_port/tls-cert"
  directory=$(curl -sf --cacert "$trust" --resolve "localhost:$pebble_acme_port:127.0.0.1" "https://localhost:$pebble_acme_port/dir")
  case "$directory" in
  *'"newOrder"'*'"renewalInfo"'*|*'"renewalInfo"'*'"newOrder"'*) ;;
  *) echo "Pebble's directory is not one: $directory" >&2; return 1 ;;
  esac
  echo "Pebble hands out its root and answers its directory"
}

serves() {
  curl -sf -o /dev/null "http://127.0.0.1:$control_port/status"
}

up() {
  local waited=0
  "${compose[@]}" up -d
  until serves; do
    waited=$((waited + 1))
    [ $waited -lt 300 ] || { echo "the sink at 127.0.0.1:$control_port did not come up" >&2; return 1; }
    sleep 0.1
  done
  echo "the sink takes SMTP on 127.0.0.1:$smtp_port and lists it on 127.0.0.1:$control_port"
}

# One message through the sink over plain SMTP, read back from the control port and then forgotten.
check() {
  local marker listed
  marker="sidecar-stack-check-$$-$RANDOM"
  curl -sf -o /dev/null --url "smtp://127.0.0.1:$smtp_port" \
    --mail-from "check@ur.local.example" --mail-rcpt "check@sink.mail.example" \
    --upload-file - <<MESSAGE
From: check@ur.local.example
To: check@sink.mail.example
Subject: $marker

$marker
MESSAGE
  listed=$(curl -sf "http://127.0.0.1:$control_port/messages")
  case "$listed" in
  *'"from":"check@ur.local.example"'*'"recipients":["check@sink.mail.example"]'*) ;;
  *) echo "the sink does not list the message it was sent: $listed" >&2; return 1 ;;
  esac
  curl -sf -o /dev/null -X DELETE "http://127.0.0.1:$control_port/messages"
  echo "the sink took a message and listed it"
}

case "${1:-}" in
build) build ;;
up) up ;;
check) check ;;
down) "${compose[@]}" down ;;
status) "${compose[@]}" ps; "${pebble_compose[@]}" ps ;;
pebble-up) pebble_up ;;
pebble-check) pebble_check ;;
pebble-down) "${pebble_compose[@]}" down ;;
*)
  sed -n '2,18p' "${BASH_SOURCE[0]}" >&2
  exit 2
  ;;
esac
