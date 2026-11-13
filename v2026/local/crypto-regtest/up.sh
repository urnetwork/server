#!/usr/bin/env bash
# Brings up the crypto regtest harness: verified release binaries, monerod with
# a funded spending wallet and a view-only wallet under test, zebrad with a
# funded spending zecd and a watch-only zecd under test. Writes
# .state/harness.json (0600) for controller/crypto_pay_regtest_test.go.
#
# Every key, mnemonic and password is generated here, for these regtest chains
# only, into .state/secrets (0600). Nothing secret is printed.
#
# Usage: ./up.sh [--db]    (./down.sh removes everything again)
#   --db  also start the end-to-end test's own PostgreSQL and Redis, for a
#         machine whose shared local stack (local/run-local.sh) is not up, and
#         write .state/test-env-portable.sh (README.md, "Use"). On a harness
#         that is already up, adds just those.

source "$(dirname -- "${BASH_SOURCE[0]}")/lib.sh"

with_db=0
for arg in "$@"; do
    case "$arg" in
        --db) with_db=1 ;;
        *) die "unknown argument: $arg (usage: ./up.sh [--db])" ;;
    esac
done

started_at=$(date +%s)
SERVER_DIR="$(cd -- "$HARNESS_DIR/../.." && pwd -P)"
UP_LOG="$STATE_DIR/up.log"

# The end-to-end test's database: PostgreSQL and Redis of the harness's own
# (docker-compose.yml, profile db), and a private portable root for the
# server's test-env.sh: the server's synthetic vault with these two services'
# authorities, and the config repository beside the server (the monoroot
# layout) without its large data sets.
start_db() {
    local portable="$STATE_DIR/portable"
    local config_source="$SERVER_DIR/../config"
    if [[ ! -f "$SECRETS_DIR/postgres.app-password" ]]; then
        random_secret | write_private "$SECRETS_DIR/postgres.app-password"
        printf 'POSTGRES_PASSWORD=%s\nAPP_DB_PASSWORD=%s\n' "$(random_secret)" \
            "$(read_secret "$SECRETS_DIR/postgres.app-password")" | write_private "$SECRETS_DIR/postgres.env"
    fi
    log "db: starting crypto-regtest-postgres and crypto-regtest-redis"
    compose --profile db up --detach postgres redis >>"$UP_LOG" 2>&1
    wait_until 180 "postgres" docker exec crypto-regtest-postgres pg_isready --quiet --host 127.0.0.1 --username bringyour --dbname bringyour
    wait_until 60 "redis" docker exec crypto-regtest-redis redis-cli ping

    rm -rf "$portable"
    (umask 077 && mkdir -p "$portable/vault/local" "$portable/config/all" "$portable/config/local" "$portable/site")
    chmod 700 "$portable"
    cp "$SERVER_DIR/local/testdata/vault/local/"* "$portable/vault/local/"
    printf '# crypto-regtest-postgres (up.sh --db), for these tests only\nauthority: "127.0.0.1:%s"\nuser: "bringyour"\npassword: "%s"\ndb: "bringyour"\n' \
        "$PORT_POSTGRES" "$(read_secret "$SECRETS_DIR/postgres.app-password")" | write_private "$portable/vault/local/pg.yml"
    cp -p "$portable/vault/local/pg.yml" "$portable/vault/local/pg_maintenance.yml"
    printf '# crypto-regtest-redis (up.sh --db), for these tests only\nauthority: "127.0.0.1:%s"\npassword: ""\ndb: 0\ncluster: false\n' \
        "$PORT_REDIS" | write_private "$portable/vault/local/redis.yml"
    if [[ -f "$config_source/all/pro.yml" && -f "$config_source/local/db.yml" ]]; then
        cp "$config_source/all/"*.yml "$config_source/all/"*.pem "$portable/config/all/"
        cp "$config_source/local/"*.yml "$portable/config/local/"
        if [[ -d "$config_source/local/mmdb" ]]; then
            cp -R "$config_source/local/mmdb" "$portable/config/local/"
        fi
    else
        log "db: no config repository beside the server; using its synthetic config (tests that need pro.yml skip)"
        cp -R "$SERVER_DIR/local/testdata/config/." "$portable/config/"
    fi

    local physical
    physical="$(cd -- "$portable" && pwd -P)"
    {
        printf '# Source before the server'"'"'s test-env.sh to run the DB tests against the harness'"'"'s\n'
        printf '# own PostgreSQL and Redis (test-env.sh'"'"'s private portable mode). Written by up.sh --db.\n'
        printf 'unset WARP_HOME WARP_VAULT_HOME WARP_CONFIG_HOME URNETWORK_TEST_REAL_VAULT\n'
        printf 'unset WARP_TEST_ENV_SUITE_PROXY_STATE_DIR WARP_TEST_ENV_TCP_PROBE WARP_TEST_ENV_TEST_HOSTS_FILE WARP_TEST_ENV_TEST_RUN_LOCAL_LOCK_DIR\n'
        printf 'export WARP_TEST_ENV_USE_PORTABLE_RESOURCES=1\nexport WARP_TEST_ENV_ALLOW_UNMANAGED_PORTABLE_SERVICES=1\n'
        printf 'export WARP_TEST_ENV_PORTABLE_ROOT=%q\n' "$physical"
        printf 'export WARP_TEST_ENV_PORTABLE_POSTGRES_AUTHORITY=127.0.0.1:%s\n' "$PORT_POSTGRES"
        printf 'export WARP_TEST_ENV_PORTABLE_REDIS_AUTHORITY=127.0.0.1:%s\n' "$PORT_REDIS"
        printf 'export BRINGYOUR_POSTGRES_HOSTNAME=127.0.0.1\nexport BRINGYOUR_REDIS_HOSTNAME=127.0.0.1\n'
        printf 'export WARP_SITE_HOME=%q\n' "$physical/site"
    } >"$STATE_DIR/test-env-portable.sh"
    log "db: up; for TestCryptoRegtestEndToEnd: source local/crypto-regtest/.state/test-env-portable.sh, then ./test-env.sh"
}

MONEROD_URL="http://127.0.0.1:$PORT_MONEROD/json_rpc"
MONERO_FUNDER_URL="http://127.0.0.1:$PORT_MONERO_FUNDER/json_rpc"
MONERO_VIEW_URL="http://127.0.0.1:$PORT_MONERO_VIEW/json_rpc"
ZEBRAD_URL="http://127.0.0.1:$PORT_ZEBRAD/"
ZECD_FUNDER_URL="http://127.0.0.1:$PORT_ZECD_FUNDER/"
ZECD_VIEW_URL="http://127.0.0.1:$PORT_ZECD_VIEW/"

# Blocks mined to the Monero funder: coinbase unlocks after 60, and a spend
# needs decoys for a ring of 16.
MONERO_FUNDING_BLOCKS=120
# Blocks mined to the Zcash funder's transparent address: coinbase matures
# after 100, so about 30 outputs are shieldable at once (z_shieldcoinbase
# takes up to 50).
ZCASH_FUNDING_BLOCKS=130
# The bootstrap miner of the first two Zcash blocks (zecd init needs a tree
# state above genesis before the funder's keys exist): the regtest lockbox
# address of the §A.6 config. Those coins are never used.
ZCASH_BOOTSTRAP_MINER=t27eWDgjFYJGVXmzrXeVjnb5J3uXDM9xH9v

# ----- 0. a clean start -----

existing="$( (docker ps --all --format '{{.Names}}'; docker volume ls --format '{{.Name}}'; docker network ls --format '{{.Name}}') |
    grep '^crypto-regtest-' || true)"
if [[ -n "$existing" ]]; then
    running=0
    for name in crypto-regtest-monerod crypto-regtest-monero-funder crypto-regtest-monero-view \
        crypto-regtest-zebrad crypto-regtest-zecd-funder crypto-regtest-zecd-view; do
        if [[ "$(docker inspect --format '{{.State.Running}}' "$name" 2>/dev/null || true)" == true ]]; then
            running=$((running + 1))
        fi
    done
    if [[ -f "$HARNESS_JSON" && $running -eq 6 ]]; then
        if [[ $with_db -eq 1 ]]; then
            start_db
            exit 0
        fi
        log "already up (.state/harness.json); ./down.sh to start over"
        exit 0
    fi
    die "leftovers of an earlier run exist (crypto-regtest-* containers, volumes or network); run ./down.sh first"
fi
rm -rf "$STATE_DIR"

# ----- 1. binaries and the base image -----

"$HARNESS_DIR/fetch.sh"
if ! docker image inspect "$BASE_IMAGE" --platform linux/amd64 >/dev/null 2>&1; then
    log "pulling $BASE_IMAGE (linux/amd64)"
    docker pull --quiet --platform linux/amd64 "$BASE_IMAGE" >/dev/null
fi

# ----- 2. secrets -----

mkdir -p "$SECRETS_DIR"
chmod 700 "$STATE_DIR" "$SECRETS_DIR"
# compose's and the daemons' one-shot output; never a secret
: >"$UP_LOG"
for wallet in monero-funder monero-view; do
    login="$wallet:$(random_secret)"
    printf '%s' "$login" | write_private "$SECRETS_DIR/$wallet.login"
    printf 'RPC_LOGIN=%s\n' "$login" | write_private "$SECRETS_DIR/$wallet.env"
done
random_secret | write_private "$SECRETS_DIR/monero-funder.wallet-password"
random_secret | write_private "$SECRETS_DIR/monero-test-wallet.wallet-password"
random_secret | write_private "$SECRETS_DIR/monero-view.wallet-password"
for wallet in zecd-funder zecd-view; do
    random_secret | write_private "$SECRETS_DIR/$wallet.rpc-password"
done
printf 'funder:%s' "$(read_secret "$SECRETS_DIR/zecd-funder.rpc-password")" | write_private "$SECRETS_DIR/zecd-funder.login"
printf 'urpay:%s' "$(read_secret "$SECRETS_DIR/zecd-view.rpc-password")" | write_private "$SECRETS_DIR/zecd-view.login"
# compose mounts this before keygen writes it; an empty placeholder keeps
# docker from creating a directory in its place
: | write_private "$SECRETS_DIR/zecd-funder.mnemonic"

MONERO_FUNDER_LOGIN="$SECRETS_DIR/monero-funder.login"
MONERO_VIEW_LOGIN="$SECRETS_DIR/monero-view.login"
ZECD_FUNDER_LOGIN="$SECRETS_DIR/zecd-funder.login"
ZECD_VIEW_LOGIN="$SECRETS_DIR/zecd-view.login"

# ----- 3. Monero -----

log "monero: starting monerod (regtest, offline, fixed difficulty 1)"
compose up --detach monerod >>"$UP_LOG" 2>&1
wait_until 300 "monerod" monero_rpc "$MONEROD_URL" "" get_info

log "monero: starting the funder wallet-rpc"
compose up --detach monero-funder >>"$UP_LOG" 2>&1
wait_until 300 "the funder wallet-rpc" monero_rpc "$MONERO_FUNDER_URL" "$MONERO_FUNDER_LOGIN" get_version

# The test wallet is created and read on the funder's wallet-rpc, then closed:
# only its address and private view key leave it. Its keys stay in the
# funder's volume, so the regtest coins it receives remain recoverable.
monero_rpc "$MONERO_FUNDER_URL" "$MONERO_FUNDER_LOGIN" create_wallet \
    "{\"filename\":\"test-wallet\",\"password\":\"$(read_secret "$SECRETS_DIR/monero-test-wallet.wallet-password")\",\"language\":\"English\"}" >/dev/null
monero_test_address="$(monero_rpc "$MONERO_FUNDER_URL" "$MONERO_FUNDER_LOGIN" get_address '{"account_index":0}' | jq -r .address)"
monero_rpc "$MONERO_FUNDER_URL" "$MONERO_FUNDER_LOGIN" query_key '{"key_type":"view_key"}' | jq -r .key |
    write_private "$SECRETS_DIR/monero-test-wallet.view-key"
monero_rpc "$MONERO_FUNDER_URL" "$MONERO_FUNDER_LOGIN" query_key '{"key_type":"mnemonic"}' | jq -r .key |
    write_private "$SECRETS_DIR/monero-test-wallet.REGTEST-ONLY-mnemonic"
monero_rpc "$MONERO_FUNDER_URL" "$MONERO_FUNDER_LOGIN" close_wallet '{"autosave_current":true}' >/dev/null
[[ "$monero_test_address" =~ ^4[1-9A-HJ-NP-Za-km-z]{94}$ ]] || die "monero: the test wallet's address is not a standard address"

monero_rpc "$MONERO_FUNDER_URL" "$MONERO_FUNDER_LOGIN" create_wallet \
    "{\"filename\":\"funder\",\"password\":\"$(read_secret "$SECRETS_DIR/monero-funder.wallet-password")\",\"language\":\"English\"}" >/dev/null
monero_funder_address="$(monero_rpc "$MONERO_FUNDER_URL" "$MONERO_FUNDER_LOGIN" get_address '{"account_index":0}' | jq -r .address)"

log "monero: mining $MONERO_FUNDING_BLOCKS blocks to the funder"
monero_rpc "$MONEROD_URL" "" generateblocks \
    "{\"amount_of_blocks\":$MONERO_FUNDING_BLOCKS,\"wallet_address\":\"$monero_funder_address\"}" >/dev/null
monero_height="$(monero_rpc "$MONEROD_URL" "" get_info | jq -r .height)"

# The view-only wallet: generate_from_keys in --wallet-dir mode, restored from
# a few blocks back, then served in --wallet-file mode like production.
monero_restore_height=$((monero_height - 5))
log "monero: creating the view-only wallet (restore height $monero_restore_height)"
compose --profile bootstrap up --detach monero-view-bootstrap >>"$UP_LOG" 2>&1
wait_until 300 "the view wallet's bootstrap wallet-rpc" monero_rpc "$MONERO_VIEW_URL" "$MONERO_VIEW_LOGIN" get_version
monero_rpc "$MONERO_VIEW_URL" "$MONERO_VIEW_LOGIN" generate_from_keys \
    "{\"filename\":\"wallet\",\"address\":\"$monero_test_address\",\"viewkey\":\"$(read_secret "$SECRETS_DIR/monero-test-wallet.view-key")\",\"password\":\"$(read_secret "$SECRETS_DIR/monero-view.wallet-password")\",\"restore_height\":$monero_restore_height,\"autosave_current\":true}" >/dev/null
monero_rpc "$MONERO_VIEW_URL" "$MONERO_VIEW_LOGIN" close_wallet '{"autosave_current":true}' >/dev/null
compose --profile bootstrap rm --stop --force monero-view-bootstrap >>"$UP_LOG" 2>&1

log "monero: starting the view-only wallet-rpc"
compose up --detach monero-view >>"$UP_LOG" 2>&1
wait_until 300 "the view-only wallet-rpc" monero_rpc "$MONERO_VIEW_URL" "$MONERO_VIEW_LOGIN" get_version
served="$(monero_rpc "$MONERO_VIEW_URL" "$MONERO_VIEW_LOGIN" get_address '{"account_index":0}' | jq -r .address)"
[[ "$served" == "$monero_test_address" ]] || die "monero: the view-only wallet serves another address than the test wallet's"
if monero_rpc "$MONERO_VIEW_URL" "$MONERO_VIEW_LOGIN" query_key '{"key_type":"spend_key"}' >/dev/null 2>&1; then
    die "monero: the wallet under test holds a spend key"
fi
monero_rpc "$MONERO_VIEW_URL" "$MONERO_VIEW_LOGIN" refresh '{}' >/dev/null

log "monero: waiting for the funder's mined coins to unlock"
monero_funder_unlocked() {
    monero_rpc "$MONERO_FUNDER_URL" "$MONERO_FUNDER_LOGIN" refresh '{}' >/dev/null &&
        monero_rpc "$MONERO_FUNDER_URL" "$MONERO_FUNDER_LOGIN" get_balance '{"account_index":0}' |
        jq -e '.unlocked_balance > 0' >/dev/null
}
wait_until 600 "the monero funder's unlocked balance" monero_funder_unlocked

# ----- 4. Zcash -----

write_zebrad_toml() {
    local miner="$1"
    sed -e "s/__NU63_HEIGHT__/$ZECD_REGTEST_NU63_HEIGHT/" \
        -e "s/__MINING_SECTION__/[mining]\\
miner_address = \"$miner\"/" \
        "$HARNESS_DIR/config/zebrad.toml.in" >"$STATE_DIR/zebrad.toml"
}

zebra_height() {
    zcash_rpc "$ZEBRAD_URL" "" getblockcount
}

zebra_generate() {
    local blocks="$1" chunk
    while [[ $blocks -gt 0 ]]; do
        chunk=$((blocks < 25 ? blocks : 25))
        zcash_rpc "$ZEBRAD_URL" "" generate "[$chunk]" >/dev/null
        blocks=$((blocks - chunk))
    done
}

# a zecd has scanned to Zebra's tip
zecd_synced() {
    local url="$1" login="$2" tip blocks
    tip="$(zebra_height)" || return 1
    blocks="$(zcash_rpc "$url" "$login" getblockchaininfo | jq -r .blocks)" || return 1
    [[ "$blocks" == "$tip" ]]
}

log "zcash: starting zebrad (regtest) with the bootstrap miner"
write_zebrad_toml "$ZCASH_BOOTSTRAP_MINER"
compose up --detach zebrad >>"$UP_LOG" 2>&1
wait_until 300 "zebrad" zebra_height
zebra_generate 2

# Keys: the funder's mnemonic and transparent address, and a separate test
# wallet whose UFVK the watch-only zecd gets. Each is made by `zecd init` in a
# throwaway datadir; only the files below leave the container.
log "zcash: generating the funder's and the test wallet's keys"
compose --profile tool run --rm --no-deps -T --name crypto-regtest-zecd-keygen zecd-tool sh -c '
    set -e
    umask 077
    # on failure, the log tail with anything mnemonic- or key-shaped redacted
    trap '"'"'[ $? -eq 0 ] || tail -n 5 /tmp/keygen.log 2>/dev/null | sed -E "s/[a-z]+( [a-z]+){3,}/<redacted>/g; s/u(view|ivk)[a-z0-9]+/<redacted>/g"'"'"' EXIT
    zecd=/opt/zecd/zecd
    $zecd --conf /config/zecd-funder.toml --datadir /tmp/funder init --wallet default \
        >/secrets/zecd-funder.mnemonic 2>/tmp/keygen.log
    ZECD_MNEMONIC="$(cat /secrets/zecd-funder.mnemonic)" $zecd --conf /config/zecd-funder.toml \
        --datadir /tmp/derive derive-address --mnemonic --address-type transparent --index 0 --json \
        >/secrets/zecd-funder.taddr.json 2>>/tmp/keygen.log
    $zecd --conf /config/zecd-view.toml --datadir /tmp/test init --wallet default \
        >/secrets/zecd-test-wallet.REGTEST-ONLY-mnemonic 2>>/tmp/keygen.log
    $zecd --conf /config/zecd-view.toml --datadir /tmp/test export-ufvk --wallet default \
        2>>/tmp/keygen.log | tail -n 1 >/secrets/zecd-test-wallet.ufvk
' >>"$UP_LOG" 2>&1 || die "zcash: keygen failed (.state/up.log)"
for secret in zecd-funder.mnemonic zecd-test-wallet.REGTEST-ONLY-mnemonic; do
    [[ "$(wc -w <"$SECRETS_DIR/$secret" | tr -d ' ')" == 24 ]] || die "zcash: $secret is not a 24-word mnemonic"
    chmod 600 "$SECRETS_DIR/$secret"
done
chmod 600 "$SECRETS_DIR/zecd-test-wallet.ufvk" "$SECRETS_DIR/zecd-funder.taddr.json"
grep -q '^uviewregtest1' "$SECRETS_DIR/zecd-test-wallet.ufvk" || die "zcash: the test wallet's UFVK is not a regtest UFVK"
zcash_funder_taddr="$(jq -r '.addresses[0].address' "$SECRETS_DIR/zecd-funder.taddr.json")"
[[ "$zcash_funder_taddr" =~ ^tm[1-9A-HJ-NP-Za-km-z]{33}$ ]] || die "zcash: the funder's transparent address is not a regtest t-address"

log "zcash: restarting zebrad with the funder's transparent address as the miner"
write_zebrad_toml "$zcash_funder_taddr"
compose up --detach --force-recreate zebrad >>"$UP_LOG" 2>&1
wait_until 300 "zebrad" zebra_height
log "zcash: mining $ZCASH_FUNDING_BLOCKS blocks to the funder"
zebra_generate "$ZCASH_FUNDING_BLOCKS"

# zecd's regtest harness: restore the funder with birthday 2, after the
# coinbase it will shield was mined
log "zcash: restoring the funder zecd (birthday 2) and starting it"
compose run --rm --no-deps -T --name crypto-regtest-zecd-funder-init zecd-funder \
    /opt/zecd/zecd --conf /config/zecd.toml --datadir /data init --wallet default \
    --restore --mnemonic-file /secrets/zecd-funder.mnemonic --birthday 2 >>"$UP_LOG" 2>&1 ||
    die "zcash: the funder's init failed"
compose up --detach zecd-funder >>"$UP_LOG" 2>&1
wait_until 900 "the funder zecd's scan" zecd_synced "$ZECD_FUNDER_URL" "$ZECD_FUNDER_LOGIN"

log "zcash: shielding the funder's coinbase"
zcash_funder_ua="$(zcash_rpc "$ZECD_FUNDER_URL" "$ZECD_FUNDER_LOGIN" getnewaddress '["", "orchard"]' | jq -r .)"
shield_opid="$(zcash_rpc "$ZECD_FUNDER_URL" "$ZECD_FUNDER_LOGIN" z_shieldcoinbase "[\"*\", \"$zcash_funder_ua\"]" | jq -r .opid)"
# an asynchronous operation: wait for it, and stop on its error
shield_deadline=$(($(date +%s) + 900))
while :; do
    shield_status="$(zcash_rpc "$ZECD_FUNDER_URL" "$ZECD_FUNDER_LOGIN" z_getoperationstatus "[[\"$shield_opid\"]]" || printf '[]')"
    case "$(printf '%s' "$shield_status" | jq -r '.[0].status // ""')" in
        success) break ;;
        failed | cancelled) die "zcash: z_shieldcoinbase failed: $(printf '%s' "$shield_status" | jq -c '.[0].error')" ;;
    esac
    [[ $(date +%s) -lt $shield_deadline ]] || die "zcash: z_shieldcoinbase did not finish in 900s"
    sleep 2
done
zcash_rpc "$ZECD_FUNDER_URL" "$ZECD_FUNDER_LOGIN" z_getoperationresult "[[\"$shield_opid\"]]" >/dev/null
zebra_generate 1
zecd_shielded_note() {
    zcash_rpc "$ZECD_FUNDER_URL" "$ZECD_FUNDER_LOGIN" listunspent '[1]' |
        jq -e 'map(select(.pool != "transparent" and .amount > 0)) | length > 0' >/dev/null
}
wait_until 300 "the funder's shielded note" zecd_shielded_note

zcash_tip="$(zebra_height)"
zcash_birthday=$((zcash_tip - 5))
log "zcash: creating the watch-only zecd (birthday $zcash_birthday) and starting it"
compose run --rm --no-deps -T --name crypto-regtest-zecd-view-init \
    --volume "$SECRETS_DIR/zecd-test-wallet.ufvk:/secrets/zecd-test-wallet.ufvk:ro" zecd-view \
    sh -c '/opt/zecd/zecd --conf /config/zecd.toml --datadir /data init --wallet default \
        --ufvk "$(cat /secrets/zecd-test-wallet.ufvk)" --birthday '"$zcash_birthday" >>"$UP_LOG" 2>&1 ||
    die "zcash: the watch-only init failed"
compose up --detach zecd-view >>"$UP_LOG" 2>&1
wait_until 900 "the watch-only zecd's scan" zecd_synced "$ZECD_VIEW_URL" "$ZECD_VIEW_LOGIN"
zcash_rpc "$ZECD_VIEW_URL" "$ZECD_VIEW_LOGIN" getwalletinfo | jq -e '.private_keys_enabled == false' >/dev/null ||
    die "zcash: the wallet under test holds spending keys"

# ----- 5. harness.json -----

jq -n \
    --arg monero_daemon "$MONEROD_URL" \
    --arg monero_funder "$MONERO_FUNDER_URL" \
    --rawfile monero_funder_login "$MONERO_FUNDER_LOGIN" \
    --arg monero_funder_address "$monero_funder_address" \
    --arg monero_view "$MONERO_VIEW_URL" \
    --rawfile monero_view_login "$MONERO_VIEW_LOGIN" \
    --rawfile monero_view_wallet_password "$SECRETS_DIR/monero-view.wallet-password" \
    --arg monero_primary "$monero_test_address" \
    --arg monero_view_container "$CONTAINER_MONERO_VIEW" \
    --arg zebrad "$ZEBRAD_URL" \
    --arg zcash_funder "$ZECD_FUNDER_URL" \
    --rawfile zcash_funder_login "$ZECD_FUNDER_LOGIN" \
    --arg zcash_funder_ua "$zcash_funder_ua" \
    --arg zcash_view "$ZECD_VIEW_URL" \
    --rawfile zcash_view_login "$ZECD_VIEW_LOGIN" \
    --arg zcash_view_container "$CONTAINER_ZECD_VIEW" \
    --argjson zcash_birthday "$zcash_birthday" \
    --argjson monero_restore_height "$monero_restore_height" \
    'def trim: rtrimstr("\n") | rtrimstr("\r");
    {
        monero: {
            daemon_rpc_url: $monero_daemon,
            funder_rpc_url: $monero_funder, funder_login: ($monero_funder_login | trim), funder_address: $monero_funder_address,
            view_rpc_url: $monero_view, view_login: ($monero_view_login | trim),
            view_wallet_password: ($monero_view_wallet_password | trim),
            primary_address: $monero_primary, restore_height: $monero_restore_height,
            view_container: $monero_view_container
        },
        zcash: {
            node_rpc_url: $zebrad,
            funder_rpc_url: $zcash_funder, funder_login: ($zcash_funder_login | trim), funder_address: $zcash_funder_ua,
            view_rpc_url: $zcash_view, view_login: ($zcash_view_login | trim), birthday: $zcash_birthday,
            view_container: $zcash_view_container
        }
    }' | write_private "$HARNESS_JSON"

if [[ $with_db -eq 1 ]]; then
    start_db
fi

log "up in $(($(date +%s) - started_at))s: monero height $(monero_rpc "$MONEROD_URL" "" get_info | jq -r .height), zcash height $(zebra_height)"
log "run the tests: see README.md (URNETWORK_CRYPTO_REGTEST=1 go test -tags crypto_regtest ...)"
