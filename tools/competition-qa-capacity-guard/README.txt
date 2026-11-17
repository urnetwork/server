Private management-capacity guards and deterministic standalone Go tests.
These scripts never run inside the evaluator or change a scoring policy.

The normal and race scripts differ ONLY in their fixed owned service name.
They accept a measurement only after a fresh command exits zero and emits one
complete canonical decimal result. At most three attempts are made; no partial
du output or ENOENT error is accepted. Commands have a five-second deadline and
one-second kill grace. Persistent uncertainty stops qualification and requests
TERM only for the original manager-confirmed invocation/main pid. Unknown or
replaced ownership is never signaled. The sole chain owner MUST join the guard
before reusing the literal unit name; a unit-name kill is not an atomic manager
compare-and-signal operation against an uncoordinated privileged launcher.

Unchanged limits are root free33GiB, Go-cache growth2GiB, temporary bytes1.25GiB.
A guard error never means a successful qualification or permission to replay.
The two historical scripts remain exact causal controls and MUST NOT be used
to protect new work: their set-e/pipefail du error exits without fencing work.

Tests use only private fake commands in the independently built test executable;
they contact no real systemd, database, Docker, cache or evaluation process.
The hung-command case uses GNU timeout and positive TERM/join acknowledgements.
The cancellation fake signals only its own parent guard shell. The test watchdog
cannot be counted as a successful result. Fake helper GORACE overrides only
atexit_sleep_ms=0; race error detection remains enabled.

From this standalone module with Go1.27.4, supply source identities explicitly:
  export QA_GUARD_NORMAL="$PWD/normal.sh"
  export QA_GUARD_RACE="$PWD/race.sh"
  export QA_GUARD_OLD_NORMAL="$PWD/normal-before-measurement-fix.sh"
  export QA_GUARD_OLD_RACE="$PWD/race-before-measurement-fix.sh"
  GOWORK=off go test -p=1 -run '^TestGuard' -count=3 -timeout=10m .
  GOWORK=off go test -race -p=1 -run '^TestGuard' -count=3 -timeout=10m .

Independent original-source qualification:11 top-level cases,33 normal and33
race passed, zero failure/skip, source before/after equal. The versioned six
source blobs are byte-identical; no new binary or test result is inferred from
versioning. SOURCE.sha256 also covers this explanatory readme.
