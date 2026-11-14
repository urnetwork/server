#!/usr/bin/env bash
# FS-42's schedule on T3 (OPS-M3-04; D-OPS-14, D-OPS-18, FI-12): a Postgres primary lost and restored through xops's
# real dump and restore scripts, in each order of the dumps' snapshot times, and §19.10's restore frame run through
# its api step, every step twice. Nightly and at each Gn-out from M3.
#
# usage: local/sidecar-restore-t3/run.sh [go test args...]
#
# It runs the sidecarrestore package's FS-42 tests (TestFs42*) under the sidecar_pending tag, which links every
# pending migration group's tables, against the private test stack that the environment names: source the stack's env
# and server/test-env.sh first, as for any direct go test on Postgres. Each test makes its own database, roles and
# client container (sidecar-test-restore-*) on the private server sidecar-test-pg and removes them; it dials only
# that server's published port, never a production tunnel port. It needs Docker with the server's image, gpg,
# python3 and xops's sibling checkout. GO names the go to run, and extra arguments go to go test, such as
# -run '^TestFs42MainDumpNewer$'. Later milestones' FS-42 steps (DNS-M5-24, SOCK-M5-17, TLS-M5-26, TLS-M6-07, MAIL's
# M7 tasks, SDK-M8-12, SMTP-M9-19) add their tests on sidecarrestore.PostgresLossFixture and run here with them.
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
server=$(cd "$here/../.." && pwd)
GO=${GO:-go}

if [ -z "${WARP_ENV:-}" ]; then
  echo "source the private test stack's env and server/test-env.sh first: WARP_ENV is not set" >&2
  exit 64
fi
if ! docker inspect sidecar-test-pg >/dev/null 2>&1; then
  echo "the private server sidecar-test-pg is not running" >&2
  exit 69
fi

cd "$server"
export SIDECAR_RESTORE_T3=1
exec "$GO" test -tags sidecar_pending -count=1 -timeout 1800s -run '^TestFs42' "$@" ./sidecarrestore/
