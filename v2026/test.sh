#!/usr/bin/env zsh

export WARP_ENV="local"
export WARP_SERVICE="test"
export WARP_DOMAIN="bringyour.com"
export WARP_BLOCK="test"
export WARP_VERSION="0.0.0"
export BRINGYOUR_POSTGRES_HOSTNAME="local-pg.bringyour.com"
export BRINGYOUR_REDIS_HOSTNAME="local-redis.bringyour.com"

# The proxy integration tests (./proxy) drive real-time wireguard/gvisor packet
# paths and real outbound TLS. Like the connect packet-translation tests, the
# race detector's scheduling overhead slows that real-time delivery enough to
# stall them, and with the main loop's `-timeout 0` a stall hangs the whole
# integration run indefinitely. Run the proxy package on its own, up front in a
# fresh process, the way proxy/test.sh runs it: no -race, and a finite timeout
# so a stall fails the step instead of hanging forever. It is skipped in the
# -race loop below.
proxy_dir="./proxy"
if [[ -d $proxy_dir ]]; then
    pushd $proxy_dir
    match="/$(basename $(pwd))/\\S*\.go\|^\\S*_test.go"
    go test -timeout 30m -v "$@" | grep --color=always -e "^" -e "$match"
    if [[ ${pipestatus[1]} != 0 ]]; then
        exit ${pipestatus[1]}
    fi
    popd
fi

for d in `find . -iname '*_test.go' | xargs -n 1 dirname | sort | uniq | paste -sd ' ' -`; do
    if [[ $d == $proxy_dir ]]; then
        # run separately above (no -race, finite timeout)
        continue
    fi
    # if [[ $1 == "" || $1 == `basename $d` ]]; then
        pushd $d
        # highlight source files in this dir
        match="/$(basename $(pwd))/\\S*\.go\|^\\S*_test.go"
        # go test -v "$@" | grep --color=always -e "^" -e "$match"
        GORACE="log_path=profile/race.out halt_on_error=1" go test -timeout 0 -v -race -cpuprofile profile/cpu -memprofile profile/memory "$@" | grep --color=always -e "^" -e "$match"
        if [[ ${pipestatus[1]} != 0 ]]; then
            exit ${pipestatus[1]}
        fi
        popd
    # fi
done
# stdbuf -i0 -o0 -e0

# ./test.sh -run 'pattern'
# ./test.sh -short
