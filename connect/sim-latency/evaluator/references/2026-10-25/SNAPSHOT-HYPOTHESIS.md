# Immutable positive-cache snapshot: static research only

A draft patch exists, but **has not been compiled, tested or measured**. This
is a separate hypothesis against the corrected frozen source, not a revision
to the failed warm-reader pilot or authorization to change the baseline.
The existing owner-admission-map draft is also unqualified.

[`warm-snapshot.patch`](warm-snapshot.patch), SHA-256
`36f2f0fe83d63bdc2336d57f4638915e539f7899b1abe344e99e73bea52bb904`,
changes only `connect/resident_contract_manager.go` against frozen blob
`ba3c09f7960f789b37a262b1542ee4dbdca93996`. Six candidate-only deterministic
test drafts are preserved as
[`snapshot-qualification.go.txt`](snapshot-qualification.go.txt), SHA-256
`b930f9f05df788388533be3e1ca68c42b07add6e9eb7ec1a32d44bbddd6ca37f`.
The `.txt` fixture is not compiled by the simulator suite or submitted patch.
Preparation workspace: `temp/reference-snapshot-draft-20261025.BFH83oNJ/`.

An additional unexecuted
[`snapshot-benchmark.go.txt`](snapshot-benchmark.go.txt), SHA-256
`f7a84b34cdcacf09e57004259410d43ef7b48f4d0663503b51add584aab28060`,
uses the actual constructor/default freshness policy and identical code on
baseline/candidate. For an idle-slot supporting screen, use five observations
per source at `-benchtime=100ms`, management CPUs 20,22 and GOMAXPROCS=2.
Its 32 parallel callers exercise one warm pair; source reads other than the
single initial read or any refused hit invalidate that observation. Keep every
output, ns/op and allocation result. Do not reroll an unfavorable result.
Median candidate time must improve before spending a full directional pilot.
These deliberately narrow two-CPU mechanism observations are not the real
ten-core workload, confidence proof, calibrated margin or end-to-end acceptance.
The same fixture separately benchmarks publication with 128 live pairs using
the actual registered read-worker seam. Retain its allocation and owner-lock
cost beside the warm-hit result; that direct seam deliberately excludes Redis
and admission latency. A faster warm lookup alone does not establish that the
tradeoff pays at the real refresh rate. Full-window memory/resource and score
qualification remain mandatory, even if the supporting hit benchmark improves.

## Mechanism to investigate

Warm first-half positive cache hits currently acquire the manager's mutex.
The prior reader-lock pilot did not improve its measured workload; another
reader lock is not assumed sufficient. An immutable, owner-published snapshot
could let that specific path read without a shared lock, while cold admission,
refresh, map sweeping and read-worker registration keep their existing owner
lock. This is a mechanism hypothesis, not evidence that contention dominates
the measured score or that the improvement exceeds noise.

The uncompiled draft uses one `atomic.Pointer` to an immutable snapshot
containing copied `activeContractEntry` values. Publication happens under the
existing owner lock after authoritative updates/removals. No mutable map or
entry pointer may be modified after publication. Cold and second-half requests
fall back to the existing admission path; they do not acquire new queues or
wait for a refresh owned by another flow.

The draft changes the existing close flag to one atomic authority, clears the
snapshot on close, refuses late publication, avoids copying for an absent
negative result and moves the touched admission/sweep interval to private
per-owner settings with the same one-second default. Actual equivalence is not
established by this description; the correctness screen remains mandatory.

## Correctness constraints from the static review

- **No extended authority:** both local freshness and returned Redis source
  lease deadlines remain ceilings. A fast-path hit is limited to the first
  half of their effective intersection, with current time/deadline checks and
  no stale timestamp carried across a potentially blocking boundary.
- **Close visibility:** snapshot readers cannot read the lock-owned plain
  `readsClosed` field concurrently. A prospective atomic closed flag must be
  the same authoritative state used by slow admission and Close, not a second
  eventually synchronized shutdown decision. Closing invalidates the snapshot;
  late completing reads cannot republish a usable snapshot after close.
- **Registration and joins:** fast cache hits register no read worker. All new
  cold/refresh reads still register under the owner lock before shutdown can
  start its join. Retain the existing cancellation and panic handling.
- **Invalidation:** negative refresh, source expiry, freshness expiry and sweep
  removal must not leave a usable old snapshot. A read that linearizes before
  an update still needs its own immutable deadline; no latest-result promise
  should be invented that the existing cache never supplied.
- **Bounded retention:** snapshots must not create unbounded histories. Do not
  retain whole old maps across slow Redis operations or blocked callbacks.
  Include map-copy allocation, retired snapshots and peak overlap in actual
  memory accounting, rather than assuming lock removal is free.
- **Owner settings:** touched legacy admission/sweep timing tunables must move
  to per-owner settings with an explicit defaults constructor, preserving
  current defaults and bounds. Do not repurpose the positive-cache freshness
  setting as an unrelated admission interval or add process-global budgets.
- **Same product behavior:** no measurement-aware branches, bypassed
  authorization, relaxed limits, changed workload or test detection. The
  candidate remains subject to protected-source and honesty checks.

## Evidence needed before advancement

Use fresh private checkouts of exact frozen source. First run the existing
resident-contract and composed packet-path correctness screen on baseline and
candidate. Add deterministic candidate tests for the actual lock-free entry
path: valid warm progress while the slow lock is held; expired/negative results;
half-window refresh ownership; independent pairs; close and in-flight read
join ordering; bounded snapshot retention; and PostgreSQL acquisition tripwires.
Use explicit barriers or state changes, not sleeps as the primary proof.
The six drafted tests cover a held-lock/slow-path tripwire warm hit, exact
virtual-clock source expiry and immutable old contents, negative refresh,
barrier-controlled late refresh after close, bounded recent provider cohorts
and independent owner shutdown. They must actually compile and pass, together
with the existing composed PostgreSQL-tripwire tests, before any pilot.

Only after correctness succeeds, freeze a separate full R9 fresh-control pilot
with exact patch/build identities before observing performance. Keep every
outcome. Do not reuse formal calibration controls, retry an unfavorable result
or call this a qualified better reference before an actual directional pass
and the independently committed held-out screen. No builds, tests or probes
may overlap the running calibration cohort.
