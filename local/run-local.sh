#!/usr/bin/env bash
#
# run-local.sh -- bring up the local test/dev backing stores for
# github.com/urnetwork/server and point the well-known hostnames at them.
#
# It:
#   1. reads the postgres/redis credentials + ports from the selected local
#      vault resources (or the checked-in throwaway fallback),
#   2. on Linux, temporarily widens the ephemeral TCP port range and listen
#      queues for full-scale local load simulations,
#   3. claims exclusive ownership of the local service names and points them at
#      a dedicated address (LOCAL_HOST_IP, default 10.213.0.1) in /etc/hosts,
#   4. adds that address to the loopback interface -- deliberately NOT
#      127.0.0.1 (see SAFETY below),
#   5. starts postgres + redis on a dedicated docker network, publishing their
#      ports on LOCAL_HOST_IP,
#   6. blocks in the foreground streaming container logs, and
#   7. on exit (Ctrl-C or otherwise) restores the port range and /etc/hosts,
#      stops the containers, and removes the loopback alias.
#
# SAFETY: each local hostname must resolve only to LOCAL_HOST_IP, never to a
# second address or 127.0.0.1. Tests create and DROP databases, and a tunnel to
# a real (prod) database commonly listens on 127.0.0.1:5432. The launcher does
# not guess which resolver result is safe or rewrite operator-owned aliases: an
# unmanaged alias, legacy managed block, or existing launcher lock stops startup
# before host/network/Docker mutation. LOCAL_HOST_IP is a distinct dedicated
# address; the worst case when the stack is down is "connection refused".
#
# Run it in its own terminal, then run the tests from another, e.g.:
#   ./test.sh -run TestFoo
# (test.sh already exports WARP_ENV=local and the BRINGYOUR_*_HOSTNAME vars that
# match the /etc/hosts aliases below.)
#
# Editing /etc/hosts and the loopback interface needs root. On Linux the script
# also runs Docker through `sudo` because access to the daemon socket is commonly
# root-only; Docker Desktop on macOS continues to run as the current user.
# You'll be prompted for your password once.
#
# Flags:
#   --fresh      wipe the postgres data volume first (forces DB re-init)
#   --keep-up    leave the containers running after this script exits
#   -h, --help   show this help and exit

set -euo pipefail

die() { printf 'run-local.sh: %s\n' "$*" >&2; exit 1; }
log() { printf '\033[1;34m[run-local]\033[0m %s\n' "$*"; }

usage() { sed -n '2,/^$/p' "$0" | sed 's/^#\{0,1\} \{0,1\}//'; }

FRESH=0
KEEP_UP=0
for arg in "$@"; do
  case "$arg" in
    --fresh)    FRESH=1 ;;
    --keep-up)  KEEP_UP=1 ;;
    -h|--help)  usage; exit 0 ;;
    *)          die "unknown argument: $arg (see --help)" ;;
  esac
done

# --- resolve paths -----------------------------------------------------------

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd)"
SERVER_DIR="$(cd -- "$SCRIPT_DIR/.." >/dev/null 2>&1 && pwd)"
URNETWORK_HOME="$(cd -- "$SERVER_DIR/.." >/dev/null 2>&1 && pwd)"
COMPOSE_FILE="$SCRIPT_DIR/docker-compose.yml"
LOCAL_STATE_FILE="$SCRIPT_DIR/run-local-state.sh"
LOCAL_SERVICES_FILE="$SCRIPT_DIR/run-local-services.sh"
OS="$(uname -s)"

[[ -f "$LOCAL_STATE_FILE" ]] || die "local state helper not found: $LOCAL_STATE_FILE"
source "$LOCAL_STATE_FILE"
[[ -f "$LOCAL_SERVICES_FILE" ]] || die "local services helper not found: $LOCAL_SERVICES_FILE"
source "$LOCAL_SERVICES_FILE"

WARP_ENV="${WARP_ENV:-local}"
[[ "$WARP_ENV" == "local" ]] || die "refusing WARP_ENV=$WARP_ENV; the local test stack requires WARP_ENV=local"

# Prefer an explicit warp override, then an existing sibling vault checkout, and
# finally the checked-in throwaway fixture. An explicit override remains
# fail-closed so a typo never silently selects different credentials.
if [[ "${WARP_TEST_ENV_USE_PORTABLE_RESOURCES:-0}" == "1" ]]; then
  VAULT_ROOT="$SCRIPT_DIR/testdata/vault"
elif [[ -n "${WARP_VAULT_HOME:-}" ]]; then
  VAULT_ROOT="$WARP_VAULT_HOME"
elif [[ -n "${WARP_HOME:-}" && -f "$WARP_HOME/vault/$WARP_ENV/pg.yml" && -f "$WARP_HOME/vault/$WARP_ENV/redis.yml" ]]; then
  VAULT_ROOT="$WARP_HOME/vault"
elif [[ -f "$URNETWORK_HOME/vault/$WARP_ENV/pg.yml" && -f "$URNETWORK_HOME/vault/$WARP_ENV/redis.yml" ]]; then
  VAULT_ROOT="$URNETWORK_HOME/vault"
else
  VAULT_ROOT="$SCRIPT_DIR/testdata/vault"
fi
VAULT_LOCAL="$VAULT_ROOT/$WARP_ENV"
PG_YML="$VAULT_LOCAL/pg.yml"
REDIS_YML="$VAULT_LOCAL/redis.yml"

[[ -f "$COMPOSE_FILE" ]] || die "compose file not found: $COMPOSE_FILE"
[[ -f "$PG_YML" ]]       || die "postgres vault config not found: $PG_YML (set WARP_VAULT_HOME?)"
[[ -f "$REDIS_YML" ]]    || die "redis vault config not found: $REDIS_YML (set WARP_VAULT_HOME?)"
log "using local test resources from $VAULT_LOCAL"

# --- dedicated addressing (never 127.0.0.1) ----------------------------------

# The loopback-alias IP the DB ports bind to and the hostnames resolve to.
LOCAL_HOST_IP="${LOCAL_HOST_IP:-10.213.0.1}"
case "$LOCAL_HOST_IP" in
  ""|127.0.0.1|0.0.0.0|localhost)
    die "LOCAL_HOST_IP must be a dedicated address, not '$LOCAL_HOST_IP' (127.0.0.1 is forbidden -- it risks hitting a prod-db tunnel)" ;;
esac

# --- parse the vault yaml (flat scalars) -------------------------------------

get_scalar() {
  # get_scalar FILE KEY -> value of a top-level `KEY: value`, with surrounding
  # double quotes and trailing "# comment" stripped. These files are flat, so a
  # line-oriented parse is sufficient (avoids a yq/python dependency).
  sed -n -E "s/^[[:space:]]*$2[[:space:]]*:[[:space:]]*(.*)\$/\1/p" "$1" \
    | head -n1 \
    | sed -E 's/[[:space:]]+#.*$//; s/^"(.*)"$/\1/; s/[[:space:]]+$//'
}

PG_USER="$(get_scalar "$PG_YML" user)"
PG_PASSWORD="$(get_scalar "$PG_YML" password)"
PG_DB="$(get_scalar "$PG_YML" db)"
PG_AUTHORITY="$(get_scalar "$PG_YML" authority)"
PG_PORT="${PG_AUTHORITY##*:}"

REDIS_AUTHORITY="$(get_scalar "$REDIS_YML" authority)"
REDIS_PORT="${REDIS_AUTHORITY##*:}"

[[ -n "$PG_USER" ]]     || die "could not read 'user' from $PG_YML"
[[ -n "$PG_PASSWORD" ]] || die "could not read 'password' from $PG_YML"
[[ -n "$PG_DB" ]]       || die "could not read 'db' from $PG_YML"
[[ "$PG_PORT"    =~ ^[0-9]+$ ]] || die "could not parse postgres port from authority '$PG_AUTHORITY'"
[[ "$REDIS_PORT" =~ ^[0-9]+$ ]] || die "could not parse redis port from authority '$REDIS_AUTHORITY'"

# Hostnames the server/tests use to reach the DBs. Match whatever the harness
# will thread into the vault authority via BRINGYOUR_*_HOSTNAME (see test.sh).
PG_HOST="${BRINGYOUR_POSTGRES_HOSTNAME:-local-pg.bringyour.com}"
REDIS_HOST="${BRINGYOUR_REDIS_HOSTNAME:-local-redis.bringyour.com}"
HOSTS_IP="$LOCAL_HOST_IP"

# Values consumed by docker-compose.yml / the postgres init script.
export APP_DB_USER="$PG_USER"
export APP_DB_PASSWORD="$PG_PASSWORD"
export APP_DB_NAME="$PG_DB"
export POSTGRES_PORT="$PG_PORT"
export REDIS_PORT="$REDIS_PORT"
export LOCAL_BIND_IP="$LOCAL_HOST_IP"
export POSTGRES_SUPERUSER_PASSWORD="${POSTGRES_SUPERUSER_PASSWORD:-postgres}"

# --- docker / compose plumbing ----------------------------------------------

command -v nc >/dev/null 2>&1 || die "nc not found on PATH (required for bounded service probes)"
command -v docker >/dev/null 2>&1 || die "docker not found on PATH"
DOCKER=(docker)
COMPOSE_ENV_VARS="APP_DB_USER,APP_DB_PASSWORD,APP_DB_NAME,POSTGRES_PORT,REDIS_PORT,LOCAL_BIND_IP,LOCAL_DOCKER_SUBNET,POSTGRES_SUPERUSER_PASSWORD"
if [[ "$OS" == Linux ]]; then
  DOCKER=(sudo docker)
fi

"${DOCKER[@]}" info >/dev/null || die "the docker daemon is not running or is not accessible"
if "${DOCKER[@]}" compose version >/dev/null 2>&1; then
  if [[ "$OS" == Linux ]]; then
    # sudo normally strips the values exported above, but Compose needs them
    # while interpolating docker-compose.yml. Preserve only that narrow set.
    DC=(sudo "--preserve-env=$COMPOSE_ENV_VARS" docker compose)
  else
    DC=(docker compose)
  fi
elif command -v docker-compose >/dev/null 2>&1; then
  if [[ "$OS" == Linux ]]; then
    DC=(sudo "--preserve-env=$COMPOSE_ENV_VARS" docker-compose)
  else
    DC=(docker-compose)
  fi
else
  die "neither 'docker compose' nor 'docker-compose' is available"
fi
compose() { "${DC[@]}" -f "$COMPOSE_FILE" "$@"; }

PG_CONTAINER=urnetwork-local-pg
REDIS_CONTAINER=urnetwork-local-redis

wait_healthy() { # wait_healthy CONTAINER TIMEOUT_SECONDS
  local c="$1" timeout="$2" i=0 status
  while :; do
    status="$("${DOCKER[@]}" inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "$c" 2>/dev/null || echo missing)"
    [[ "$status" == healthy ]] && return 0
    i=$((i + 1))
    [[ "$i" -ge "$timeout" ]] && { log "$c did not become healthy (last status: $status)"; return 1; }
    sleep 1
  done
}

port_open() { # port_open HOST PORT
  nc -z -w 2 -- "$1" "$2" </dev/null >/dev/null 2>&1
}

verify_reachable() { # confirm the dedicated address actually serves the DBs
  local i
  for i in $(seq 1 15); do
    if port_open "$LOCAL_HOST_IP" "$PG_PORT" && port_open "$LOCAL_HOST_IP" "$REDIS_PORT"; then
      return 0
    fi
    sleep 1
  done
  return 1
}

# --- Linux ephemeral ports (full-scale loopback load) -----------------------

# A connection is identified by its local/remote address+port tuple. The Linux
# default range (commonly 32768-60999) contains only 28,232 source ports, but a
# full sim-latency run can hold 30,000 Redis connections to one destination.
# Widen the range while the local environment is active, then restore exactly
# what was present when the script started.
EPHEMERAL_PORT_LOW="${LOCAL_EPHEMERAL_PORT_LOW:-10240}"
EPHEMERAL_PORT_HIGH="${LOCAL_EPHEMERAL_PORT_HIGH:-65535}"
[[ "$EPHEMERAL_PORT_LOW" =~ ^[0-9]+$ ]] ||
  die "LOCAL_EPHEMERAL_PORT_LOW must be an integer"
[[ "$EPHEMERAL_PORT_HIGH" =~ ^[0-9]+$ ]] ||
  die "LOCAL_EPHEMERAL_PORT_HIGH must be an integer"
(( EPHEMERAL_PORT_LOW >= 1024 && EPHEMERAL_PORT_LOW < EPHEMERAL_PORT_HIGH && EPHEMERAL_PORT_HIGH <= 65535 )) ||
  die "invalid ephemeral port range: $EPHEMERAL_PORT_LOW $EPHEMERAL_PORT_HIGH"

EPHEMERAL_RANGE_ORIGINAL=""
EPHEMERAL_RANGE_APPLIED=""
EPHEMERAL_RANGE_CHANGED=0

configure_ephemeral_range() {
  [[ "$OS" == Linux ]] || return 0
  local current_low current_high
  read -r current_low current_high < /proc/sys/net/ipv4/ip_local_port_range
  if (( current_low <= EPHEMERAL_PORT_LOW && EPHEMERAL_PORT_HIGH <= current_high )); then
    log "ephemeral TCP range already sufficient: $current_low $current_high"
    return 0
  fi
  EPHEMERAL_RANGE_ORIGINAL="$current_low $current_high"
  EPHEMERAL_RANGE_APPLIED="$EPHEMERAL_PORT_LOW $EPHEMERAL_PORT_HIGH"
  sudo sysctl -q -w "net.ipv4.ip_local_port_range=$EPHEMERAL_RANGE_APPLIED"
  EPHEMERAL_RANGE_CHANGED=1
  read -r current_low current_high < /proc/sys/net/ipv4/ip_local_port_range
  [[ "$current_low $current_high" == "$EPHEMERAL_RANGE_APPLIED" ]] ||
    die "failed to set ephemeral TCP range to $EPHEMERAL_RANGE_APPLIED"
  log "widened ephemeral TCP range: $EPHEMERAL_RANGE_ORIGINAL -> $EPHEMERAL_RANGE_APPLIED"
}

restore_ephemeral_range() {
  [[ "$EPHEMERAL_RANGE_CHANGED" == 1 ]] || return 0
  local current_low current_high
  read -r current_low current_high < /proc/sys/net/ipv4/ip_local_port_range
  if [[ "$current_low $current_high" != "$EPHEMERAL_RANGE_APPLIED" ]]; then
    log "warning: ephemeral TCP range changed externally; leaving $current_low $current_high in place"
    return 0
  fi
  sudo sysctl -q -w "net.ipv4.ip_local_port_range=$EPHEMERAL_RANGE_ORIGINAL"
}

# A burst of tens of thousands of simulator connections can also overflow the
# kernel's default 4,096-entry accept/SYN queues before Redis has a chance to
# accept them. Match the Redis tcp-backlog configured in docker-compose.yml.
TCP_LISTEN_BACKLOG="${LOCAL_TCP_LISTEN_BACKLOG:-65535}"
NETDEV_MAX_BACKLOG="${LOCAL_NETDEV_MAX_BACKLOG:-65535}"
NF_CONNTRACK_MAX="${LOCAL_NF_CONNTRACK_MAX:-1048576}"
[[ "$TCP_LISTEN_BACKLOG" =~ ^[0-9]+$ ]] ||
  die "LOCAL_TCP_LISTEN_BACKLOG must be an integer"
(( TCP_LISTEN_BACKLOG >= 4096 && TCP_LISTEN_BACKLOG <= 65535 )) ||
  die "invalid LOCAL_TCP_LISTEN_BACKLOG: $TCP_LISTEN_BACKLOG"
[[ "$NETDEV_MAX_BACKLOG" =~ ^[0-9]+$ ]] ||
  die "LOCAL_NETDEV_MAX_BACKLOG must be an integer"
(( NETDEV_MAX_BACKLOG >= 1000 && NETDEV_MAX_BACKLOG <= 1048576 )) ||
  die "invalid LOCAL_NETDEV_MAX_BACKLOG: $NETDEV_MAX_BACKLOG"
[[ "$NF_CONNTRACK_MAX" =~ ^[0-9]+$ ]] ||
  die "LOCAL_NF_CONNTRACK_MAX must be an integer"
(( NF_CONNTRACK_MAX >= 262144 && NF_CONNTRACK_MAX <= 8388608 )) ||
  die "invalid LOCAL_NF_CONNTRACK_MAX: $NF_CONNTRACK_MAX"

SOMAXCONN_ORIGINAL=""
SOMAXCONN_APPLIED=""
SOMAXCONN_CHANGED=0
SYN_BACKLOG_ORIGINAL=""
SYN_BACKLOG_APPLIED=""
SYN_BACKLOG_CHANGED=0
NETDEV_BACKLOG_ORIGINAL=""
NETDEV_BACKLOG_APPLIED=""
NETDEV_BACKLOG_CHANGED=0
CONNTRACK_MAX_ORIGINAL=""
CONNTRACK_MAX_APPLIED=""
CONNTRACK_MAX_CHANGED=0

configure_tcp_backlogs() {
  [[ "$OS" == Linux ]] || return 0
  local current

  current="$(sysctl -n net.core.somaxconn)"
  if (( current < TCP_LISTEN_BACKLOG )); then
    SOMAXCONN_ORIGINAL="$current"
    SOMAXCONN_APPLIED="$TCP_LISTEN_BACKLOG"
    sudo sysctl -q -w "net.core.somaxconn=$SOMAXCONN_APPLIED"
    SOMAXCONN_CHANGED=1
    [[ "$(sysctl -n net.core.somaxconn)" == "$SOMAXCONN_APPLIED" ]] ||
      die "failed to set net.core.somaxconn to $SOMAXCONN_APPLIED"
    log "raised TCP accept queue: $SOMAXCONN_ORIGINAL -> $SOMAXCONN_APPLIED"
  else
    log "TCP accept queue already sufficient: $current"
  fi

  current="$(sysctl -n net.ipv4.tcp_max_syn_backlog)"
  if (( current < TCP_LISTEN_BACKLOG )); then
    SYN_BACKLOG_ORIGINAL="$current"
    SYN_BACKLOG_APPLIED="$TCP_LISTEN_BACKLOG"
    sudo sysctl -q -w "net.ipv4.tcp_max_syn_backlog=$SYN_BACKLOG_APPLIED"
    SYN_BACKLOG_CHANGED=1
    [[ "$(sysctl -n net.ipv4.tcp_max_syn_backlog)" == "$SYN_BACKLOG_APPLIED" ]] ||
      die "failed to set net.ipv4.tcp_max_syn_backlog to $SYN_BACKLOG_APPLIED"
    log "raised TCP SYN queue: $SYN_BACKLOG_ORIGINAL -> $SYN_BACKLOG_APPLIED"
  else
    log "TCP SYN queue already sufficient: $current"
  fi

  current="$(sysctl -n net.core.netdev_max_backlog)"
  if (( current < NETDEV_MAX_BACKLOG )); then
    NETDEV_BACKLOG_ORIGINAL="$current"
    NETDEV_BACKLOG_APPLIED="$NETDEV_MAX_BACKLOG"
    sudo sysctl -q -w "net.core.netdev_max_backlog=$NETDEV_BACKLOG_APPLIED"
    NETDEV_BACKLOG_CHANGED=1
    [[ "$(sysctl -n net.core.netdev_max_backlog)" == "$NETDEV_BACKLOG_APPLIED" ]] ||
      die "failed to set net.core.netdev_max_backlog to $NETDEV_BACKLOG_APPLIED"
    log "raised network device backlog: $NETDEV_BACKLOG_ORIGINAL -> $NETDEV_BACKLOG_APPLIED"
  else
    log "network device backlog already sufficient: $current"
  fi

  current="$(sysctl -n net.netfilter.nf_conntrack_max)"
  if (( current < NF_CONNTRACK_MAX )); then
    CONNTRACK_MAX_ORIGINAL="$current"
    CONNTRACK_MAX_APPLIED="$NF_CONNTRACK_MAX"
    sudo sysctl -q -w "net.netfilter.nf_conntrack_max=$CONNTRACK_MAX_APPLIED"
    CONNTRACK_MAX_CHANGED=1
    [[ "$(sysctl -n net.netfilter.nf_conntrack_max)" == "$CONNTRACK_MAX_APPLIED" ]] ||
      die "failed to set net.netfilter.nf_conntrack_max to $CONNTRACK_MAX_APPLIED"
    log "raised conntrack capacity: $CONNTRACK_MAX_ORIGINAL -> $CONNTRACK_MAX_APPLIED"
  else
    log "conntrack capacity already sufficient: $current"
  fi
}

restore_tcp_backlogs() {
  local current count
  if [[ "$CONNTRACK_MAX_CHANGED" == 1 ]]; then
    current="$(sysctl -n net.netfilter.nf_conntrack_max)"
    if [[ "$current" == "$CONNTRACK_MAX_APPLIED" ]]; then
      count="$(sysctl -n net.netfilter.nf_conntrack_count)"
      if (( count <= CONNTRACK_MAX_ORIGINAL )); then
        sudo sysctl -q -w "net.netfilter.nf_conntrack_max=$CONNTRACK_MAX_ORIGINAL"
      else
        log "warning: conntrack still holds $count entries; leaving capacity $current in place"
      fi
    else
      log "warning: conntrack capacity changed externally; leaving $current in place"
    fi
  fi
  if [[ "$NETDEV_BACKLOG_CHANGED" == 1 ]]; then
    current="$(sysctl -n net.core.netdev_max_backlog)"
    if [[ "$current" == "$NETDEV_BACKLOG_APPLIED" ]]; then
      sudo sysctl -q -w "net.core.netdev_max_backlog=$NETDEV_BACKLOG_ORIGINAL"
    else
      log "warning: network device backlog changed externally; leaving $current in place"
    fi
  fi
  if [[ "$SYN_BACKLOG_CHANGED" == 1 ]]; then
    current="$(sysctl -n net.ipv4.tcp_max_syn_backlog)"
    if [[ "$current" == "$SYN_BACKLOG_APPLIED" ]]; then
      sudo sysctl -q -w "net.ipv4.tcp_max_syn_backlog=$SYN_BACKLOG_ORIGINAL"
    else
      log "warning: TCP SYN queue changed externally; leaving $current in place"
    fi
  fi
  if [[ "$SOMAXCONN_CHANGED" == 1 ]]; then
    current="$(sysctl -n net.core.somaxconn)"
    if [[ "$current" == "$SOMAXCONN_APPLIED" ]]; then
      sudo sysctl -q -w "net.core.somaxconn=$SOMAXCONN_ORIGINAL"
    else
      log "warning: TCP accept queue changed externally; leaving $current in place"
    fi
  fi
}

# --- loopback alias (host reachability, cross-platform) ----------------------

loopback_alias() { # loopback_alias add|del
  case "$OS" in
    Darwin)
      if [[ "$1" == add ]]; then
        sudo ifconfig lo0 alias "$LOCAL_HOST_IP" netmask 255.255.255.255 up
      else
        sudo ifconfig lo0 -alias "$LOCAL_HOST_IP" 2>/dev/null || true
      fi ;;
    Linux)
      if [[ "$1" == add ]]; then
        sudo ip addr add "$LOCAL_HOST_IP/32" dev lo 2>/dev/null || true
      else
        sudo ip addr del "$LOCAL_HOST_IP/32" dev lo 2>/dev/null || true
      fi ;;
    *)
      die "unsupported OS '$OS' for managing a loopback alias (only Darwin/Linux)" ;;
  esac
}

# --- /etc/hosts management ---------------------------------------------------

HOSTS_FILE=/etc/hosts
RUN_LOCK_DIR=/tmp/urnetwork-server-run-local.lock
MARKER_BEGIN="# >>> urnetwork local-env (server/local/run-local.sh) >>>"
MARKER_END="# <<< urnetwork local-env (server/local/run-local.sh) <<<"
HOSTS_BACKUP=""
HOSTS_APPLIED=""

# The state helper defaults to an unprivileged copy for isolated unit tests.
# The real hosts file is the one operation here that requires elevation.
local_hosts_replace_file() {
  sudo cp "$1" "$2"
}

flush_hosts_cache() {
  if [[ "$OS" == Darwin ]]; then
    sudo dscacheutil -flushcache 2>/dev/null || true
    sudo killall -HUP mDNSResponder 2>/dev/null || true
  fi
}

install_hosts() {
  if ! local_hosts_install \
      "$HOSTS_FILE" "$HOSTS_BACKUP" "$HOSTS_APPLIED" "$HOSTS_IP" \
      "$PG_HOST" "$REDIS_HOST" "$MARKER_BEGIN" "$MARKER_END"; then
    [[ "$LOCAL_HOSTS_FILE_MUTATED" == 1 ]] && HOSTS_INSTALLED=1
    return 1
  fi
  HOSTS_INSTALLED=1
  flush_hosts_cache
}

restore_hosts() {
  if ! local_hosts_restore \
      "$HOSTS_FILE" "$HOSTS_BACKUP" "$HOSTS_APPLIED" "$MARKER_BEGIN" "$MARKER_END"; then
    HOSTS_BACKUP_RETAIN=1
    return 1
  fi
  flush_hosts_cache
  if [[ "$LOCAL_HOSTS_RESTORE_EXACT" != 1 ]]; then
    HOSTS_BACKUP_RETAIN=1
    log "warning: $HOSTS_FILE changed externally; preserved that edit and removed only the managed block"
  fi
  HOSTS_INSTALLED=0
}

# --- lifecycle / cleanup -----------------------------------------------------

MAIN_PID=$$
SUDO_KEEPALIVE_PID=""
HOSTS_INSTALLED=0
HOSTS_BACKUP_RETAIN=0
ALIAS_ADDED=0
STACK_OWNED=0
CLEANED=0
RUN_LOCK_HELD=0
RUN_LOCK_OWNER="${SCRIPT_DIR}:$$:${RANDOM}:${RANDOM}"

cleanup() {
  [[ "$CLEANED" == 1 ]] && return
  CLEANED=1
  echo
  if [[ "$HOSTS_INSTALLED" == 1 ]]; then
    log "restoring $HOSTS_FILE"
    restore_hosts || log "warning: failed to restore $HOSTS_FILE (backup: $HOSTS_BACKUP)"
  fi
  if [[ "$STACK_OWNED" != 1 ]]; then
    :
  elif [[ "$KEEP_UP" == 1 ]]; then
    log "leaving containers running (--keep-up); stop them with: cd $SCRIPT_DIR && ${DC[*]} down"
  else
    log "stopping containers"
    compose down --remove-orphans || true
  fi
  if [[ "$ALIAS_ADDED" == 1 ]]; then
    log "removing loopback alias $LOCAL_HOST_IP"
    loopback_alias del || true
  fi
  if [[ "$EPHEMERAL_RANGE_CHANGED" == 1 ]]; then
    log "restoring ephemeral TCP range $EPHEMERAL_RANGE_ORIGINAL"
    restore_ephemeral_range || log "warning: failed to restore ephemeral TCP range $EPHEMERAL_RANGE_ORIGINAL"
  fi
  if [[ "$CONNTRACK_MAX_CHANGED" == 1 || "$NETDEV_BACKLOG_CHANGED" == 1 || "$SYN_BACKLOG_CHANGED" == 1 || "$SOMAXCONN_CHANGED" == 1 ]]; then
    log "restoring Linux network queue and conntrack limits"
    restore_tcp_backlogs || log "warning: failed to restore Linux network queue/conntrack limits"
  fi
  [[ -n "$SUDO_KEEPALIVE_PID" ]] && kill "$SUDO_KEEPALIVE_PID" 2>/dev/null || true
  if [[ -n "$HOSTS_BACKUP" ]]; then
    if [[ "$HOSTS_BACKUP_RETAIN" == 1 ]]; then
      log "hosts backup retained for manual recovery: $HOSTS_BACKUP"
    else
      rm -f "$HOSTS_BACKUP"
    fi
  fi
  if [[ -n "$HOSTS_APPLIED" ]]; then
    rm -f "$HOSTS_APPLIED"
  fi
  if [[ "$RUN_LOCK_HELD" == 1 ]]; then
    local_run_lock_release "$RUN_LOCK_DIR" "$RUN_LOCK_OWNER" ||
      log "warning: failed to release local launcher lock $RUN_LOCK_DIR"
    RUN_LOCK_HELD=0
  fi
}

trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

HOSTS_BACKUP="$(mktemp -t urnetwork-hosts-backup.XXXXXX)"
HOSTS_APPLIED="$(mktemp -t urnetwork-hosts-applied.XXXXXX)"
# Arm the token-checked release before acquisition. If a signal lands after the
# helper publishes our owner token but before it returns, cleanup still owns it.
RUN_LOCK_HELD=1
if ! local_run_lock_acquire "$RUN_LOCK_DIR" "$RUN_LOCK_OWNER"; then
  RUN_LOCK_HELD=0
  rm -f "$HOSTS_BACKUP" "$HOSTS_APPLIED"
  die "another local launcher owns $RUN_LOCK_DIR; inspect it before removing a stale lock"
fi

# --- run ---------------------------------------------------------------------

log "vault:    $VAULT_LOCAL"
log "postgres: $PG_HOST -> $LOCAL_HOST_IP:$PG_PORT  (user=$PG_USER db=$PG_DB)"
log "redis:    $REDIS_HOST -> $LOCAL_HOST_IP:$REDIS_PORT"

# Prime sudo up front and keep the credential warm so the edits (and the
# restore-on-exit) don't block waiting for a password.
log "requesting sudo (needed for Docker on Linux, /etc/hosts, host networking, and the loopback alias)"
if ! sudo -n true 2>/dev/null; then
  sudo -v || die "sudo is required for Docker on Linux, $HOSTS_FILE, host networking, and the loopback interface"
fi
( while kill -0 "$MAIN_PID" 2>/dev/null; do sudo -n true 2>/dev/null || exit; sleep 30; done ) &
SUDO_KEEPALIVE_PID=$!

# Claim the resolver names before every network, kernel, or Docker mutation.
# A legacy managed block or any unmanaged spelling fails inside this transaction
# before the hosts-file replacement callback is reached.
log "claiming unique local service mappings in $HOSTS_FILE"
install_hosts || die "local service mappings are already owned; inspect $HOSTS_FILE and $RUN_LOCK_DIR"

configure_ephemeral_range
configure_tcp_backlogs

# From this point cleanup owns every Docker mutation, including --fresh.
STACK_OWNED=1
if [[ "$FRESH" == 1 ]]; then
  log "wiping existing volumes (--fresh)"
  compose down -v --remove-orphans || true
fi

# Add the dedicated loopback address before starting Docker; the already-owned
# hostnames safely get connection refused until this alias and its listeners exist.
log "adding loopback alias $LOCAL_HOST_IP"
loopback_alias add || die "failed to add loopback alias $LOCAL_HOST_IP"
ALIAS_ADDED=1

log "starting postgres + redis on the urnetwork-local network"
compose up -d

log "waiting for containers to become healthy"
wait_healthy "$PG_CONTAINER" 90    || die "postgres failed to start (see: ${DC[*]} -f $COMPOSE_FILE logs postgres)"
wait_healthy "$REDIS_CONTAINER" 60 || die "redis failed to start (see: ${DC[*]} -f $COMPOSE_FILE logs redis)"

log "verifying $LOCAL_HOST_IP:$PG_PORT and $LOCAL_HOST_IP:$REDIS_PORT are reachable"
verify_reachable || die "the DBs are healthy but $LOCAL_HOST_IP is not reachable on ports $PG_PORT/$REDIS_PORT (loopback alias / port publish problem)"

log "verifying PostgreSQL access with the selected local resource"
local_postgres_require_application_access "$PG_CONTAINER" "$PG_USER" "$PG_PASSWORD" "$PG_DB" ||
  die "postgres does not satisfy the selected local test profile"

cat <<INFO

  Local environment is up.

    postgres  ${PG_HOST}:${PG_PORT}   ->  ${LOCAL_HOST_IP}:${PG_PORT}   (user=${PG_USER} db=${PG_DB})
    redis     ${REDIS_HOST}:${REDIS_PORT}  ->  ${LOCAL_HOST_IP}:${REDIS_PORT}

  Addresses resolve to ${LOCAL_HOST_IP} (a dedicated loopback alias, never 127.0.0.1).
  Linux ephemeral TCP range: $(cat /proc/sys/net/ipv4/ip_local_port_range 2>/dev/null || echo "platform default")
  Linux TCP accept/SYN queues: $(sysctl -n net.core.somaxconn 2>/dev/null || echo "platform default") / $(sysctl -n net.ipv4.tcp_max_syn_backlog 2>/dev/null || echo "platform default")
  Linux netdev backlog / conntrack max: $(sysctl -n net.core.netdev_max_backlog 2>/dev/null || echo "platform default") / $(sysctl -n net.netfilter.nf_conntrack_max 2>/dev/null || echo "platform default")

  Run tests from another terminal (server/):  ./test.sh -run TestName
  Stop everything: press Ctrl-C here (restores ${HOSTS_FILE} and removes the alias).

  Streaming container logs (Ctrl-C to stop)...
INFO

# Block in the foreground. Ctrl-C sends SIGINT to this group; `logs -f` exits,
# the INT trap fires -> exit -> cleanup restores hosts, tears down, drops alias.
compose logs -f --tail=20 || true
