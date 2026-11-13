#!/usr/bin/env bash
# FS-41's storage losses on T3's file clusters (EVSB-M2-20; §19.10's site-loss runbook, D-OPS-14), on the containers
# docker-compose.yml starts:
#
# usage: wipe.sh drives <cluster> <host>...   loses those hosts' drives for good: each host's container and its drive
#                                              volumes removed, then started again empty, as new disks in a replaced
#                                              host are; past a set's parity (3 of a cluster's 4 hosts) what the
#                                              lost drives held is gone, and readers alarm on heads naming missing
#                                              segments
#        wipe.sh site                          the site lost: every cluster's containers and drives removed, then
#                                              started again with new empty drives and their buckets made again (§19.10's
#                                              hardware step), on which t3.sh suite passes before any re-seed
#
# A host is named by its number, 1 to 4. The authority and Postgres steps of FS-41 are their
# owners'; sidecar.yml's reseed: true, which opens /reseed, is the run's to set. GO names the go that runs t3tool.
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
server=$(cd "$here/../.." && pwd)
GO=${GO:-go}
compose=(docker compose -f "$here/docker-compose.yml")

# a cluster's host service, as the compose file names it
service_of() {
  local cluster=$1 host=$2
  case "$cluster" in
    meta-1|meta-2|seg-1) echo "fc-$cluster-host$host" ;;
    *) echo "no cluster $cluster" >&2; return 64 ;;
  esac
}

drive_count_of() {
  case "$1" in
    meta-1|meta-2) echo 2 ;;
    seg-1) echo 4 ;;
  esac
}

lose_drives() {
  local cluster=$1 host service container drive
  shift
  [ $# -gt 0 ] || { echo "usage: $0 drives <cluster> <host>..." >&2; return 64; }
  local services=()
  for host in "$@"; do
    service=$(service_of "$cluster" "$host")
    container="sidecar-test-$service"
    docker rm -f "$container" >/dev/null
    for drive in $(seq 1 "$(drive_count_of "$cluster")"); do
      docker volume rm "$container-d$drive" >/dev/null
    done
    services+=("$service")
  done
  "${compose[@]}" up -d --quiet-pull "${services[@]}"
  echo "$cluster: lost the drives of hosts $* (${services[*]}), started again empty"
}

site() {
  "${compose[@]}" down -v --remove-orphans
  "$here/t3.sh" up
  echo "the site: every cluster started again on empty drives, its bucket new"
}

case "${1:-}" in
  drives) shift; lose_drives "$@" ;;
  site) site ;;
  *) echo "usage: $0 drives <cluster> <host>... | site" >&2; exit 64 ;;
esac
