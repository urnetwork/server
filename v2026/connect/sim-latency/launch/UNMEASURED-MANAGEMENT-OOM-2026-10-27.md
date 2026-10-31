# Calibration launcher OOM before measurements

This was a control-launcher configuration error, not a submission defect. The
extra **3 GiB management cgroup limit** added to the calibration invocation was
too small for the race-enabled controller plus tmpfs-backed source extraction.
The frozen evaluator's 96 GiB active / 5 GiB evidence limits were not changed.

The qualified first-position operator's metadata preparation and verification
passed in 118.69 and 61.14 seconds. Its exact selected public plan was pushed in
`fd6928606184c97e55b192619845000e1b35aaf0`, SHA
`a8a57ccfe3d5f050ec76bf475dc1e35fb25011c9b781ee1c90dfa7cca720bb9f`.
Execution session 77172 then terminated with exit 137.

## Actual failure and zero-measurement evidence

- The first job started at `2026-10-27T16:34:06Z`; its immutable original
  deadline remains **`2026-10-27T19:34:06Z`**.
- At 16:34:20, the kernel killed controller PID 970581 in
  `urnetwork-first-committed-execution-20261027-v1.scope`. Systemd records
  `Result=oom-kill`, `MemoryMax=3221225472`.
- The cap was consumed by 1,559,826,432 bytes anonymous memory,
  1,623,650,304 bytes tmpfs/shmem, and 37,748,736 bytes kernel memory.
  This was not ordinary reclaimable disk cache. The triggering operation was
  Docker writing copied frozen baseline sources into tmpfs.
- The retained journal shows only the identity checks and source-extraction
  container. Frozen evaluator ordering places that setup before baseline
  simulations, candidate construction, candidate simulations and scoring.
  `runs` and `score-output` are empty. No progress, baseline, score, completion,
  worker-result, manager-terminal, outcome or campaign-result record exists.
- Other selected jobs and all jobs in the second seed retain only their three
  original inputs. No operator process, attempt cgroup, attempt container or
  attempt network remains. Only the original three Docker containers remain.

The remaining 5 GiB tmpfs mount contained 28,672 bytes. Root copied its contents
byte-for-byte to protected evidence, verified equality, and unmounted that exact
mount normally. No journal, failed artifact, input, source or MinIO object was
deleted or restored. The copy remains available for recovery/audit.

Retained evidence directory:
`/var/lib/urnetwork/competition-first-committed-management-oom-20261027-v1`.

| Evidence | SHA-256 |
| --- | --- |
| `kernel.log` | `fce44dc438b0a697278cc18d7652b5bb8165d8becd1e43329b4d7db2b5ee9020` |
| `management-scope.log` | `1569c144c0e6e29c98aea8c1a3771c58230d4432faf999b9361bb6a949a14035` |
| `scope-properties.txt` | `7488033d7dfbdc76eb3a50af953731a1445b2de9d66f2726fe059f40a440b65c` |
| `mount-before.json` | `ca7325db5d467a9692abae939c81171cc8669adf351a9306cdc6ebaa686f85a8` |
| `residual-content.sha256` | `6dd09ef8703f7a35fecfc0d8afb791e3d60a9275c83c08250b34a678b23f65e7` |

## Recovery boundary

The existing start fences correctly prevent rerunning the qualified operator.
Their hashes remain unchanged:

- Campaign start: `de4445a57e04ab566a3d02280e4286503bbe71f791389503f39b044963a24ead`.
- First-job start: `df4c9e378f48f0c38c7a2ad731f1fae4613ab8de5b2dddfe5507ef0d52badb8d`.

Root requested an explicit owner exception to restart **the same first committed
seed**, only after proving zero measurements and preserving all failed-start
evidence. That would give the corrected first job a fresh three-hour deadline;
it is **not** represented as retaining the old absolute deadline. Approval has
was subsequently received explicitly from the owner. The approval is recorded
in `UNMEASURED-RECOVERY-OWNER-APPROVAL-2026-10-27.json`, SHA-256
`56735963a107e2ad8e4700acd1939a0a3024a755d45a4eded80209a021981c8e`.
The second seed stays unstarted.

The one-use recovery applied successfully on October 27 at approximately
21:26 UTC. Both original start hashes match their archived copies under
`unmeasured-recovery-forensics-000001`; all failed artifacts remain retained.
Only the original three input files were restored to the first job. The
unchanged native executor was launched at 21:26:55 UTC in the approved 8 GiB
management scope after its actual memory preflight passed. Its new first-job
start/deadline must be read from the native record; launcher time is not the
measurement start.

An inactive, Go-only administrative gate passed independent source and actual
configuration/proof review. The security-fixed tests passed 31 cases in each
of three root race runs, with zero skips. A final test-only change makes the
privileged ownership case explicitly skip for non-root developers; its fresh
root qualification also passed 31 cases in each of three consecutive runs,
with zero skips. None of the five production files changed. Final receipt:
`/home/by/urnetwork/temp/unmeasured-recovery-root-final-20261027.V9OnZGkv/qualification.json`,
SHA-256 `04682b8bb7ff5f08330ed4705ce52fc864ba9b0c1914b46b8c46cc8a6db38adb`.

The installed CLI passed a read-only inventory and complete six-job recovery
dry run. Actual management-scope memory preflights rejected 3 GiB and accepted
8 GiB. No recovery was applied and no deadline was reset. Its proposed
management limit is at least 8 GiB: the
unchanged 5 GiB evidence allowance plus 3 GiB controller/kernel headroom, with
ancestor/sibling pressure checked. It requires pinned owner approval, explicit
fresh-clock acknowledgment, exact zero-measurement proof, exclusive legacy
scoring lock, a durable one-use claim, and archival rather than deletion. It
must not run until independently qualified, reviewed, and authorized. Frozen
source, image, R9, evaluation limits, significance and 26.9% margin are unchanged.
