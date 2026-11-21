#!/usr/bin/env bash
# Run the gate's named test under the same private store preflight as sidecar-t3.sh.
set -u
cd "$1" || exit $?
shift
source ./test-env.sh || exit $?
export WARP_TEST_ENV_FAIL_FAST=1
exec "${GO:-go}" test "$@"
