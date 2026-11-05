#!/usr/bin/env bash
# Bind a preflight's actual exit directly to one unchanged launch argv.
set -u
[[ ${1-} == --preflight ]] || exit 90
shift
preflight=()
while (( $# )) && [[ $1 != --launch ]]; do
    preflight+=("$1")
    shift
done
(( ${#preflight[@]} && $# )) || exit 90
shift
(( $# )) || exit 90
# Do not depend on inherited errexit or a newline's eventual status.
if "${preflight[@]}"; then
    exec "$@"
else
    preflight_exit=$?
    exit "$preflight_exit"
fi
