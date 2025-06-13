#!/usr/bin/env zsh

for d in `find . -iname '*_test.go' | xargs -n 1 dirname | sort | uniq | paste -sd ' ' -`; do
    # if [[ $1 == "" || $1 == `basename $d` ]]; then
        pushd $d
        # highlight source files in this dir
        match="/$(basename $(pwd))/\\S*\.go\|^\\S*_test.go"
        export WARP_ENV="local"
        export BRINGYOUR_POSTGRES_HOSTNAME="local-pg.bringyour.com"
        export BRINGYOUR_REDIS_HOSTNAME="local-redis.bringyour.com"
        GORACE="log_path=profile/race.out halt_on_error=1" go test -v -race -cpuprofile profile/cpu -memprofile profile/memory -timeout 120m "$@" -args -v 0 -logtostderr true | grep --color=always -e "^" -e "$match"
            # -trace profile/trace -coverprofile profile/cover 
        if [[ ${pipestatus[1]} != 0 ]]; then
            exit ${pipestatus[1]}
        fi
        popd
    # fi
done
# stdbuf -i0 -o0 -e0 

# go tool trace profile/trace
# PPROF_BINARY_PATH=. go tool pprof profile/cpu

# store default.pgo
# https://go.dev/doc/pgo


# go test -short
