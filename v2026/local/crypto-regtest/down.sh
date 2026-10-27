#!/usr/bin/env bash
# Removes everything the harness created: its containers, its network, its
# volumes and .state/ (the generated secrets and test wallets). The verified
# downloads in .cache/ are kept unless --purge-cache is given. Touches nothing
# that is not named crypto-regtest-*.

source "$(dirname -- "${BASH_SOURCE[0]}")/lib.sh"

purge_cache=0
for arg in "$@"; do
    case "$arg" in
        --purge-cache) purge_cache=1 ;;
        *) die "unknown argument: $arg (usage: ./down.sh [--purge-cache])" ;;
    esac
done

log "stopping and removing the compose project"
compose --profile bootstrap --profile tool --profile db down --volumes --remove-orphans --timeout 60 >/dev/null 2>&1 || true

# Anything left over by an interrupted up.sh (a one-shot `compose run`
# container, a volume compose no longer tracks), matched by exact prefix.
leftover_containers="$(docker ps --all --format '{{.Names}}' | grep '^crypto-regtest-' || true)"
if [[ -n "$leftover_containers" ]]; then
    log "removing leftover containers: $(printf '%s' "$leftover_containers" | tr '\n' ' ')"
    printf '%s\n' "$leftover_containers" | xargs docker rm --force --volumes >/dev/null
fi
leftover_volumes="$(docker volume ls --format '{{.Name}}' | grep '^crypto-regtest-' || true)"
if [[ -n "$leftover_volumes" ]]; then
    log "removing leftover volumes: $(printf '%s' "$leftover_volumes" | tr '\n' ' ')"
    printf '%s\n' "$leftover_volumes" | xargs docker volume rm --force >/dev/null
fi
if docker network ls --format '{{.Name}}' | grep -qx 'crypto-regtest-net'; then
    docker network rm crypto-regtest-net >/dev/null
fi

rm -rf "$STATE_DIR"
if [[ $purge_cache -eq 1 ]]; then
    rm -rf "$CACHE_DIR"
    log "removed .cache/"
fi
log "down"
