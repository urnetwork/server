#!/usr/bin/env bash
# T3's file clusters (EVSB-M2-17): the compose file's three clusters on the minio fork's settle_testhooks build.
#
# usage: local/file-cluster-t3/t3.sh build    builds the fork's hooked binary and t3tool for the docker host, and the
#                                             image sidecar-test-fc-minio:local from them
#        local/file-cluster-t3/t3.sh up       starts the clusters, waits for each to serve, and makes their buckets
#        local/file-cluster-t3/t3.sh suite    runs filecluster-suite on every cluster (-cluster <id> for one)
#        local/file-cluster-t3/t3.sh down     stops and removes the containers and their drives
#        local/file-cluster-t3/t3.sh status
#
# MINIO_DIR names the fork's checkout (the sibling minio by default), GO the go to build with, and
# FILECLUSTER_T3_DRIVE_TIMEOUT the drive timeout the hosts run with (5s; 30s is production's, which the nightly and
# Gn-out runs set). Everything binds to 127.0.0.1 above port 50000 and is named sidecar-test-fc-*. Nothing is
# fetched: the image is FROM scratch with the two binaries built here.
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
server=$(cd "$here/../.." && pwd)
GO=${GO:-go}
MINIO_DIR=${MINIO_DIR:-$server/../minio}
export FILECLUSTER_T3_DRIVE_TIMEOUT=${FILECLUSTER_T3_DRIVE_TIMEOUT:-5s}
compose=(docker compose -f "$here/docker-compose.yml")
health=(52101 52201 52301)

build() {
  local arch context commit release version ldflags
  arch=$(docker version --format '{{.Server.Arch}}')
  context=$(mktemp -d)
  trap 'rm -rf "$context"' RETURN
  commit=$(git -C "$MINIO_DIR" rev-parse HEAD)
  # stamped as the release stamps a build (buildscripts/sidecar-release.sh), so admin info names the commit the suite
  # reports; gen-ldflags runs with no GOPATH or GOROOT, which it would otherwise stamp
  release=$(git -C "$MINIO_DIR" describe --tags --abbrev=0 --match 'RELEASE.*')
  version=$(sed -E 's/^RELEASE\.([0-9]{4}-[0-9]{2}-[0-9]{2})T([0-9]{2})-([0-9]{2})-([0-9]{2})Z$/\1T\2:\3:\4Z/' <<<"$release")
  (cd "$MINIO_DIR" && "$GO" build -o "$context/gen-ldflags" buildscripts/gen-ldflags.go)
  ldflags=$(cd "$MINIO_DIR" && env -u GOPATH -u GOROOT MINIO_RELEASE=RELEASE MINIO_HOTFIX="sidecar.${commit:0:12}" "$context/gen-ldflags" "$version")
  rm -f "$context/gen-ldflags"
  (cd "$MINIO_DIR" && GOOS=linux GOARCH="$arch" CGO_ENABLED=0 GOTOOLCHAIN=local "$GO" build -tags settle_testhooks -trimpath -ldflags "$ldflags" -o "$context/minio" .)
  (cd "$server" && GOOS=linux GOARCH="$arch" CGO_ENABLED=0 "$GO" build -trimpath -o "$context/t3tool" ./local/file-cluster-t3/t3tool)
  docker build --quiet -f "$here/Dockerfile" --build-arg MINIO_COMMIT="$commit" -t sidecar-test-fc-minio:local "$context"
  echo "sidecar-test-fc-minio:local: the fork at $commit, settle_testhooks"
}

serves() {
  curl -sf -o /dev/null "http://127.0.0.1:$1/minio/health/cluster"
}

up() {
  local port waited
  "${compose[@]}" up -d --quiet-pull
  for port in "${health[@]}"; do
    waited=0
    until serves "$port"; do
      waited=$((waited + 1))
      [ $waited -lt 1200 ] || { echo "the cluster at 127.0.0.1:$port did not come up" >&2; return 1; }
      sleep 0.1
    done
  done
  (cd "$server" && "$GO" run ./local/file-cluster-t3/t3tool buckets -clusters "$here/clusters.yml")
}

suite() {
  local hosts
  hosts=$(mktemp)
  trap 'rm -f "$hosts"' RETURN
  sed "s/drive_timeout: .*/drive_timeout: $FILECLUSTER_T3_DRIVE_TIMEOUT/" "$here/hosts.yml" >"$hosts"
  (cd "$server" && "$GO" run ./cli/filecluster-suite -clusters "$here/clusters.yml" -hosts "$hosts" "$@")
}

case "${1:-}" in
  build) build ;;
  up) up ;;
  suite) shift; suite "$@" ;;
  down) "${compose[@]}" down -v --remove-orphans ;;
  status) "${compose[@]}" ps ;;
  *) echo "usage: $0 build|up|suite [-cluster <id>]|down|status" >&2; exit 64 ;;
esac
