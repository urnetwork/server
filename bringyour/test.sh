#!/usr/bin/env bash

for d in `find . -iname '*_test.go' | xargs -n 1 dirname | sort | uniq | paste -sd ' ' -`; do
    if [[ $1 == "" || $1 == `basename $d` ]]; then
        pushd $d
        export WARP_ENV="local"; \
            export BRINGYOUR_POSTGRES_HOSTNAME="local-pg.bringyour.com"; \
            export BRINGYOUR_REDIS_HOSTNAME="local-redis.bringyour.com"; \
            go test -v | grep --color=always -e "^" -e "_test.go" 
        popd
    fi
done
# stdbuf -i0 -o0 -e0 
