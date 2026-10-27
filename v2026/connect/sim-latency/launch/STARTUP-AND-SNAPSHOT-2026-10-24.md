# Frontier startup and transaction-snapshot corrections

Status: focused regressions pass; full-suite validation, a coherent replacement
source ledger/image and fresh performance calibration remain required. No main
DB, running round or production policy was changed. Go 1.27.3 remains mandatory.

## Retained failed frontier point

The first fresh 15,000-provider impairment-on point on evaluator
`sha256:d526a183a2ecf96793f736e40ef52eadf1bf0ef212efc1ea66c1d8f296f951bf`
(server `6261c15454adec5a00680e4c3332f628a0aed1e7`) failed after 945 seconds.
All 200 clients established, but matchmaking probe 6 exhausted its HTTP attempts.
Teardown required killing shard 1 after its TERM deadline. The sidecar marks the
run `incomplete`; there is no completion marker or resource report. Its 592
measured rows include 103 failures. This is **not a baseline or accepted scale**.

Evidence is retained at mono-root-relative
`temp/provide-frontier-20261024.B0P1kUpz`:

| Artifact | SHA-256 |
| --- | --- |
| `p15000-on-initial.log` | `c87cbf5976213f15a2368d8b0d9b94bd9ca171926dd6a52f8270fe9b3c0d0033` |
| `p15000-on-initial/output/run.json` | `8bb53e9d92a5ba5eb6ca428edd424df6b1b584b0f0e28c488d66443d82d27750` |
| `p15000-on-initial/output/results.csv` | `4b36456d9b82033572adf8982ccdbbdd44e0861b01e8ca9e98cb9d0c0e03f551` |

Exact-owned cleanup left only the three pre-existing containers and original
networks. No hidden external retries, replacement samples or second point ran.

## SQL locking must also refresh the snapshot

The service log contains 449 `provide_key_pkey` uniqueness failures, with no
deadlock or serialization-error matches. The earlier minimal backport added
the per-client advisory lock but omitted main's `TxReadCommitted` option.
The default repeatable-read transaction can acquire its snapshot in the lock
query before it waits. A later delete then misses the preceding writer's new
rows, and the insert conflicts despite serialized lock ownership.

The old regression checked ordering and final state, but did not trace batched
SQL errors: transaction retries could hide the failed attempt. The new
`TestSetProvideLockWaitUsesCommittedPredecessor` traces both queries and batches,
holds the first public writer after lock acquisition, observes the second
writer's actual PostgreSQL lock wait, and proves an unrelated client can finish.
It checks complete final keys **and zero failed SQL attempts**.

Against the locked-but-repeatable-read source, it failed deterministically with
SQLSTATE 23505. Adding the missing isolation option then passed the new and old
regressions three times with race detection (6/6). Main already uses read
committed; its new regression also passed three race repetitions (3/3).
The tested source-branch correction is commit `559409607` on `sim-latency`.

Prefix evidence: `temp/provide-snapshot-prefix-20261024.hpJDTCX0/prefix.log`,
SHA-256 `e5499b9238a10f9467811547421a893ce82f0c94437d5bd32e8a94062a220800`.
Fixed evidence: `temp/provide-snapshot-fixed-20261024.5WpDi21Y`:

- `private.log`: `bee60583bc4b1591395e9aa6614143c0f31034deadabc4925eff8b423dfe1158`;
- `main.log`: `6d065c7384ee9bdf73f400062a4ae9836e6a84bc8f7790384a08805b077b001b`.

These tests prove the collision mechanism and correction. They do **not** prove
that this mechanism caused matchmaking probe 6's failure.

## Construction precedes ramp and prewarm

The parent previously returned from launching shard processes and began its
fixed ramp/prewarm wait without acknowledging their construction. Each child
constructs all its providers before starting its fleet loop. The retained run
finished prewarm at log line 690; the first fleet ticks appear only at lines
26010, 26028, 26186 and 26704. A fixed elapsed wait is not a construction barrier.

The private control protocol now has an explicit `ready` acknowledgement,
served only after `NewFleet` returns. The parent requests every shard first,
then waits for all acknowledgements before starting its ramp/prewarm clocks.
This does not start dynamics or reset accounting. Invalid/closed responses,
child exits and cancellation fail closed and join every local response waiter.
The caller's evaluation lifetime bounds the wait; no replicate or margin was
reduced. Construction time and actual capacity still require measurement.

The protocol regression failed before the fix on the unknown `ready` command.
Real command-pipe tests, cancellation/join and later-child-exit controls,
strict-response checks, parent call-order wiring, accounting, dynamics and
fresh-child impairment regressions then passed three race repetitions:
21 selected tests, 63/63 passes. Fixed log:
`temp/provide-frontier-20261024.B0P1kUpz/fleet-ready-fixed.log`, SHA-256
`aabf82a0502f4a19723b3d15ec8b7f28581aa222c583adcdc3e667732c74345e`.

The same 21 tests passed three race repetitions on the frozen-source checkout
(63/63), log `fleet-ready-private-fixed.log` in that root, SHA-256
`0244f9c2550a47eacffa4dfbc1626922ee38d846a317612d7600ddae331957d1`.
Both fixes are published together on `sim-latency` at server
`0d58d0d91b069c62bcfb39cd72166c26925b786a`. Main's tested harness/regression
commit is `afea543ff`; main's product transaction already had the correct
isolation. Neither source publication changes a frozen round or activates an
image. The last published source ledger still identifies the earlier checkpoint
until full validation and coherent replacement-artifact preparation finish.

The failed run's complete log/workload/partial-result bytes are also preserved
in [the versioned baseline archive](../baseline/prelaunch-provide-frontier-2026-10-24/README.md).

No fresh complete frontier point has yet qualified these two fixes together.
Do not reinterpret the failed run as successful or reuse it as their control.
