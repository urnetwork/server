#!/usr/bin/env bash
# Compile only the reviewed control-plane validator; no tests or measured code.
set -Eeuo pipefail
umask 077
[[ ${1:-} == --build-reviewed-validator && $# == 1 ]] || exit 2
readonly ROOT=/home/by/urnetwork/temp/mono-root-c2-release-20261026.PUHdHLML
readonly CONTROL="$ROOT/trusted-control/server-qualified-v3"
readonly REVISION=7c4dc74e5d53d7b42e22e059565d778b35bee83e
readonly SLOT=/home/by/urnetwork/temp/production-frontier-20261024.63AtMXS5/scoring.lock
readonly TOOLCHAIN=/home/by/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.27.3.linux-amd64/bin
[[ $(id -u) == 0 ]] || { printf 'bounded validator compilation requires the operator root wrapper\n' >&2; exit 2; }
[[ -f "$SLOT" && ! -L "$SLOT" ]]
exec 9<>"$SLOT"
flock -n 9
[[ ! -e "$ROOT/trusted-control/validator-build.started" ]] || exit 2
[[ $(sudo -n -u by git -C "$CONTROL" rev-parse HEAD) == "$REVISION" ]]
[[ -z $(sudo -n -u by git -C "$CONTROL" status --porcelain=v1 --untracked-files=all) ]]
[[ $(sha256sum "$ROOT/trusted-control/control.work" | cut -d' ' -f1) == 88becfd2d37bfb04495c6b2d05d854729bdcee105ed07d0dfbe3d0e53c223bef ]]
[[ $("$TOOLCHAIN/go" env GOVERSION) == go1.27.3 ]]
readonly DEPENDENCIES=/home/by/urnetwork/temp/main-targeted-2f46-corrected-20261026.3Nd6V666
for repository in connect sdk proxy glog goidenticons userwireguard sn warp gvisor; do
    case "$repository" in
        connect) expected=e2b45a4687656d68ae7113f5a701905bf868a87a ;;
        sdk) expected=b5ea15df3621899777e35942b8f2efedfec85579 ;;
        proxy) expected=af1d364fe34b807c31936642bff7d3fd849ab2a4 ;;
        glog) expected=1c4954b017d9e8140a4e7020f914eb27410a41c6 ;;
        goidenticons) expected=39ccec1b69db3a6f67e7346d6f44863499e6241d ;;
        userwireguard) expected=f9a92f0e5f5577a80d358065c4d39ca21f971a1c ;;
        sn) expected=dc57cca3170816241cc3413c36b6843402c72130 ;;
        warp) expected=97937410120279583842caa6ea7fd6512aca335f ;;
        gvisor) expected=57c9e137f7407ad6cd41dcca1ba304a446e09c79 ;;
    esac
    [[ $(git -c safe.directory="$DEPENDENCIES/$repository" -C "$DEPENDENCIES/$repository" rev-parse HEAD) == "$expected" ]]
    [[ -z $(git -c safe.directory="$DEPENDENCIES/$repository" -C "$DEPENDENCIES/$repository" status --porcelain=v1 --untracked-files=all) ]]
done
date -u +started_at=%Y-%m-%dT%H:%M:%SZ > "$ROOT/trusted-control/validator-build.started"
start_seconds=$SECONDS
set +e
systemd-run --quiet --wait --pipe --collect --service-type=exec \
    --unit=urnetwork-c2-validator-7c4dc74e5 \
    --property=AllowedCPUs=20,22 --property=MemoryMax=12G --property=MemorySwapMax=0 \
    --property=TasksMax=4096 --property=RuntimeMaxSec=900 --property=KillMode=control-group \
    --working-directory="$CONTROL" --uid=by --gid=by \
    /usr/bin/env -i PATH="$TOOLCHAIN:/usr/bin:/bin" LANG=C LC_ALL=C \
    GOTOOLCHAIN=local GOMAXPROCS=2 GOFLAGS='-mod=readonly -p=2' CGO_ENABLED=0 \
    GOPROXY=off GOSUMDB=off GOMODCACHE=/home/by/go/pkg/mod GOCACHE=/home/by/.cache/go-build \
    GOWORK="$ROOT/trusted-control/control.work" GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null \
    GIT_TERMINAL_PROMPT=0 \
    /usr/bin/timeout --signal=TERM --kill-after=30s 840s \
    "$TOOLCHAIN/go" build -trimpath -o "$ROOT/trusted-control/competitionpatch" ./cli/competitionpatch \
    > "$ROOT/trusted-control/validator-build.stdout" 2> "$ROOT/trusted-control/validator-build.stderr"
status=$?
set -e
printf 'exit=%s\nduration_seconds=%s\nended_at=%s\n' "$status" "$((SECONDS-start_seconds))" "$(date -u +%FT%TZ)" > "$ROOT/trusted-control/validator-build.terminal"
[[ "$status" == 0 ]] || exit "$status"
"$TOOLCHAIN/go" version -m "$ROOT/trusted-control/competitionpatch" > "$ROOT/trusted-control/validator-buildinfo.txt"
[[ $(sed -n 's/.*vcs.revision=//p' "$ROOT/trusted-control/validator-buildinfo.txt") == "$REVISION" ]]
[[ $(sed -n 's/.*vcs.modified=//p' "$ROOT/trusted-control/validator-buildinfo.txt") == false ]]
sha256sum "$ROOT/trusted-control/competitionpatch" > "$ROOT/trusted-control/validator.sha256"
sha256sum "$ROOT/trusted-control/validator-build.stdout" "$ROOT/trusted-control/validator-build.stderr" "$ROOT/trusted-control/validator-build.terminal" "$ROOT/trusted-control/validator-buildinfo.txt"
printf 'validator_built_not_yet_qualified\n'
