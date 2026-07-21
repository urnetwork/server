#!/usr/bin/env bash
#
# run-local.sh -- bring up the local test/dev backing stores for
# github.com/urnetwork/server and point the well-known hostnames at them.
#
# It:
#   1. reads the postgres/redis credentials + ports from vault/local/{pg,redis}.yml
#      (the single source of truth),
#   2. adds a dedicated loopback-alias IP (LOCAL_HOST_IP, default 10.213.0.1) to
#      the loopback interface -- deliberately NOT 127.0.0.1 (see SAFETY below),
#   3. points  local-pg.bringyour.com / local-redis.bringyour.com  at that IP in
#      /etc/hosts for as long as this script runs,
#   4. starts postgres + redis on a dedicated docker network, publishing their
#      ports on LOCAL_HOST_IP,
#   5. blocks in the foreground streaming container logs, and
#   6. on exit (Ctrl-C or otherwise) restores /etc/hosts, stops the containers,
#      and removes the loopback alias.
#
# SAFETY: the local hostnames must never resolve to 127.0.0.1. Tests create and
# DROP databases, and a tunnel to a real (prod) database commonly listens on
# 127.0.0.1:5432 -- so a stray 127.0.0.1 mapping could let a test wipe prod.
# LOCAL_HOST_IP is a distinct dedicated address; the worst case when the stack
# is down is "connection refused", never a real database.
#
# Run it in its own terminal, then run the tests from another, e.g.:
#   ./test.sh -run TestFoo
# (test.sh already exports WARP_ENV=local and the BRINGYOUR_*_HOSTNAME vars that
# match the /etc/hosts aliases below.)
#
# Editing /etc/hosts and the loopback interface needs root, so the script uses
# `sudo` for just those operations (docker itself runs as you); you'll be
# prompted for your password once.
#
# Flags:
#   --fresh      wipe the postgres data volume first (forces DB re-init)
#   --keep-up    leave the containers running after this script exits
#   -h, --help   show this help and exit

set -euo pipefail

die() { printf 'run-local.sh: %s\n' "$*" >&2; exit 1; }
log() { printf '\033[1;34m[run-local]\033[0m %s\n' "$*"; }

usage() { sed -n '2,46p' "$0" | sed 's/^#\{0,1\} \{0,1\}//'; }

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

WARP_ENV="${WARP_ENV:-local}"
# Prefer explicit warp overrides, else $WARP_HOME/vault, else the repo layout.
if [[ -n "${WARP_VAULT_HOME:-}" ]]; then
  VAULT_ROOT="$WARP_VAULT_HOME"
elif [[ -n "${WARP_HOME:-}" ]]; then
  VAULT_ROOT="$WARP_HOME/vault"
else
  VAULT_ROOT="$URNETWORK_HOME/vault"
fi
VAULT_LOCAL="$VAULT_ROOT/$WARP_ENV"
PG_YML="$VAULT_LOCAL/pg.yml"
REDIS_YML="$VAULT_LOCAL/redis.yml"

[[ -f "$COMPOSE_FILE" ]] || die "compose file not found: $COMPOSE_FILE"
[[ -f "$PG_YML" ]]       || die "postgres vault config not found: $PG_YML (set WARP_VAULT_HOME?)"
[[ -f "$REDIS_YML" ]]    || die "redis vault config not found: $REDIS_YML (set WARP_VAULT_HOME?)"

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

command -v docker >/dev/null 2>&1 || die "docker not found on PATH"
docker info >/dev/null 2>&1 || die "the docker daemon is not running"
if docker compose version >/dev/null 2>&1; then
  DC=(docker compose)
elif command -v docker-compose >/dev/null 2>&1; then
  DC=(docker-compose)
else
  die "neither 'docker compose' nor 'docker-compose' is available"
fi
compose() { "${DC[@]}" -f "$COMPOSE_FILE" "$@"; }

PG_CONTAINER=urnetwork-local-pg
REDIS_CONTAINER=urnetwork-local-redis

wait_healthy() { # wait_healthy CONTAINER TIMEOUT_SECONDS
  local c="$1" timeout="$2" i=0 status
  while :; do
    status="$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "$c" 2>/dev/null || echo missing)"
    [[ "$status" == healthy ]] && return 0
    i=$((i + 1))
    [[ "$i" -ge "$timeout" ]] && { log "$c did not become healthy (last status: $status)"; return 1; }
    sleep 1
  done
}

port_open() { # port_open HOST PORT
  if command -v nc >/dev/null 2>&1; then
    nc -z -w2 "$1" "$2" >/dev/null 2>&1
  else
    (exec 3<>"/dev/tcp/$1/$2") 2>/dev/null && { exec 3>&-; return 0; } || return 1
  fi
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

# --- loopback alias (host reachability, cross-platform) ----------------------

OS="$(uname -s)"
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
MARKER_BEGIN="# >>> urnetwork local-env (server/local/run-local.sh) >>>"
MARKER_END="# <<< urnetwork local-env (server/local/run-local.sh) <<<"
HOSTS_BACKUP="$(mktemp -t urnetwork-hosts.XXXXXX)"
cp "$HOSTS_FILE" "$HOSTS_BACKUP"   # full safety copy of the original

# print /etc/hosts with any managed block (from a prior/crashed run) removed
strip_block() {
  awk -v b="$MARKER_BEGIN" -v e="$MARKER_END" '
    $0 == b { inblk = 1 }
    inblk != 1 { print }
    $0 == e { inblk = 0 }
  ' "$HOSTS_FILE"
}

install_hosts() {
  local tmp; tmp="$(mktemp -t urnetwork-hosts.XXXXXX)"
  {
    strip_block
    printf '%s\n' "$MARKER_BEGIN"
    printf '%s\t%s\n' "$HOSTS_IP" "$PG_HOST"
    printf '%s\t%s\n' "$HOSTS_IP" "$REDIS_HOST"
    printf '%s\n' "$MARKER_END"
  } > "$tmp"
  sudo cp "$tmp" "$HOSTS_FILE"
  rm -f "$tmp"
  if [[ "$OS" == Darwin ]]; then
    sudo dscacheutil -flushcache 2>/dev/null || true
    sudo killall -HUP mDNSResponder 2>/dev/null || true
  fi
}

restore_hosts() {
  # Re-strip our block from the *current* file, so concurrent edits survive.
  local tmp; tmp="$(mktemp -t urnetwork-hosts.XXXXXX)"
  if strip_block > "$tmp" 2>/dev/null && [[ -s "$tmp" ]]; then
    sudo cp "$tmp" "$HOSTS_FILE"
  else
    sudo cp "$HOSTS_BACKUP" "$HOSTS_FILE"   # fallback to the original
  fi
  rm -f "$tmp"
}

# --- lifecycle / cleanup -----------------------------------------------------

MAIN_PID=$$
SUDO_KEEPALIVE_PID=""
HOSTS_INSTALLED=0
ALIAS_ADDED=0
CLEANED=0

cleanup() {
  [[ "$CLEANED" == 1 ]] && return
  CLEANED=1
  echo
  if [[ "$HOSTS_INSTALLED" == 1 ]]; then
    log "restoring $HOSTS_FILE"
    restore_hosts || log "warning: failed to restore $HOSTS_FILE (backup: $HOSTS_BACKUP)"
  fi
  if [[ "$KEEP_UP" == 1 ]]; then
    log "leaving containers running (--keep-up); stop them with: cd $SCRIPT_DIR && ${DC[*]} down"
  else
    log "stopping containers"
    compose down --remove-orphans || true
  fi
  if [[ "$ALIAS_ADDED" == 1 ]]; then
    log "removing loopback alias $LOCAL_HOST_IP"
    loopback_alias del || true
  fi
  [[ -n "$SUDO_KEEPALIVE_PID" ]] && kill "$SUDO_KEEPALIVE_PID" 2>/dev/null || true
  rm -f "$HOSTS_BACKUP"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# --- run ---------------------------------------------------------------------

log "vault:    $VAULT_LOCAL"
log "postgres: $PG_HOST -> $LOCAL_HOST_IP:$PG_PORT  (user=$PG_USER db=$PG_DB)"
log "redis:    $REDIS_HOST -> $LOCAL_HOST_IP:$REDIS_PORT"

# Prime sudo up front and keep the credential warm so the edits (and the
# restore-on-exit) don't block waiting for a password.
log "requesting sudo (needed only for /etc/hosts and the loopback alias)"
sudo -v || die "sudo is required to edit $HOSTS_FILE and the loopback interface"
( while kill -0 "$MAIN_PID" 2>/dev/null; do sudo -n true 2>/dev/null || exit; sleep 30; done ) &
SUDO_KEEPALIVE_PID=$!

if [[ "$FRESH" == 1 ]]; then
  log "wiping existing volumes (--fresh)"
  compose down -v --remove-orphans || true
fi

# Add the dedicated loopback address BEFORE starting docker (the published ports
# bind to it) and patch /etc/hosts BEFORE the DBs are up, so the hostnames only
# ever resolve to this dedicated IP -- if something connects early it gets
# "connection refused", never a real database.
log "adding loopback alias $LOCAL_HOST_IP"
loopback_alias add || die "failed to add loopback alias $LOCAL_HOST_IP"
ALIAS_ADDED=1

log "updating $HOSTS_FILE"
install_hosts
HOSTS_INSTALLED=1

log "starting postgres + redis on the urnetwork-local network"
compose up -d

log "waiting for containers to become healthy"
wait_healthy "$PG_CONTAINER" 90    || die "postgres failed to start (see: ${DC[*]} -f $COMPOSE_FILE logs postgres)"
wait_healthy "$REDIS_CONTAINER" 60 || die "redis failed to start (see: ${DC[*]} -f $COMPOSE_FILE logs redis)"

log "verifying $LOCAL_HOST_IP:$PG_PORT and $LOCAL_HOST_IP:$REDIS_PORT are reachable"
verify_reachable || die "the DBs are healthy but $LOCAL_HOST_IP is not reachable on ports $PG_PORT/$REDIS_PORT (loopback alias / port publish problem)"

cat <<INFO

  Local environment is up.

    postgres  ${PG_HOST}:${PG_PORT}   ->  ${LOCAL_HOST_IP}:${PG_PORT}   (user=${PG_USER} db=${PG_DB})
    redis     ${REDIS_HOST}:${REDIS_PORT}  ->  ${LOCAL_HOST_IP}:${REDIS_PORT}

  Addresses resolve to ${LOCAL_HOST_IP} (a dedicated loopback alias, never 127.0.0.1).

  Run tests from another terminal (server/):  ./test.sh -run TestName
  Stop everything: press Ctrl-C here (restores ${HOSTS_FILE} and removes the alias).

  Streaming container logs (Ctrl-C to stop)...
INFO

# Block in the foreground. Ctrl-C sends SIGINT to this group; `logs -f` exits,
# the INT trap fires -> exit -> cleanup restores hosts, tears down, drops alias.
compose logs -f --tail=20 || true
