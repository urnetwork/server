# Research references — no faster reference qualified

The warm-reader candidate below completed its pilot but was slower; it is
retained as an unfavorable result, not eligible for advancement as the faster
reference. A separate owner-admission-map hypothesis is drafted at the end of
this record and has not been applied or tested.

A separate [immutable positive-cache snapshot design](SNAPSHOT-HYPOTHESIS.md)
records a lock-free-hit hypothesis and its ownership/expiry/retention test
requirements. It includes an uncompiled draft patch and eight candidate-only
test drafts, but no test pass or measured result. It is not a qualified faster
reference and does not change the baseline.

## Warm-reader candidate

Prepared against exact frozen server
`0d58d0d91b069c62bcfb39cd72166c26925b786a`, not moving main.
`warm-readers.patch` SHA-256:
`2957ef507f7edc9dadcb09abd6425664e88748612917959a9a65243a0365ebe4`.
The original file blob is `ba3c09f7960f789b37a262b1542ee4dbdca93996`.

Only `connect/resident_contract_manager.go` changes. Fresh first-half positive
cache hits share a read lock; admission, refresh, expiry/sweep and shutdown keep
exclusive ownership. Source/cache deadlines are rechecked after unlock. No
authorization bypass, changed TTL, extra goroutine, new I/O or measurement-aware
behavior is intended. This is an optimization hypothesis, not a claimed bug fix
or accepted better patch.

## Independent correctness screen

A fresh workspace extracted from exact evaluator image
`sha256:effe9ba5dc9cc337b4f250977c5f921ea42e4a336b30e3a5b54a9fd1bbe7f793`
matched all ten frozen repository pins. The original Git-index blob matched the
frozen source, and the applied sole-file diff exactly matched the patch digest.
Actual Go 1.27.3, management CPUs 20,22, GOMAXPROCS=2 and -p=2 were used with an
explicit frozen-dependency workspace and disposable, attested synthetic-local
PostgreSQL/Redis. Main services and vault were not used.

All 52 selected `TestHandleContractManager*`, `TestResidentContract*` and
`TestResidentPacket*` tests passed three consecutive times on the baseline,
then three times on the candidate: **312 race-enabled executions, all exit 0**.
They cover lease expiry, delayed admission, blocked refresh, cancellation,
closed-owner joins, bounded cache/denial retention, independent pairs, fallback
guards and composed packet forwarding. No external retry was used.

| Phase/pass | Wall seconds | Log SHA-256 |
| --- | --- | --- |
| Baseline 1 | 231 | `06942993ba5432450666e93b35cdf5f3b47836f474ee60dbed1a9ff825b31978` |
| Baseline 2 | 66 | `bf16ff14c7171cd2e0c888288d3173963e291b23f74225485377570f6a69b886` |
| Baseline 3 | 65 | `08d2566ea0a7ba04d981462abfefad77bb3cac29087319ad350e77b6993ecb06` |
| Candidate 1 | 84 | `673b97f5d40202d3c210c797bd623059c219902efc06d5a1e7bd72f6ed9ebe3e` |
| Candidate 2 | 66 | `765aec489acb30ccc11ee3db297b9be3389b9330ca2b19be0d347dbb7c845f24` |
| Candidate 3 | 65 | `8cc003bce3a440f88e25f326c460eab4407a240ddc31db9e07505aa9acdfc3b6` |

Cleanup exited 0 with the original containers/networks only. The shared scoring
lock covered setup, all tests, patch application and cleanup; no scoring
overlapped. Full local evidence and selected-test census are retained under
mono-root-relative `temp/reference-correctness-independent-20261025.mRfZRDTh/`.
This is not a claim of immutable MinIO retention.

## Measured warm-reader outcome

The [predeclared full pilot](../../../launch/REFERENCE-PILOT-2026-10-25.md)
completed at 2026-10-25T09:20:38Z: nine fresh controls, offline candidate build,
nine candidates, all G1–G6, placement and exact cleanup passed in 10,521 seconds.
However, both candidate mean and median raw score were worse; mean regression
was 17.8346%. The directional screen failed, so the patch must not advance as
the faster reference or be rerun for a favorable draw. The complete
[retained evidence and comparison thresholds](../../../baseline/prelaunch-warm-reader-pilot-2026-10-25/completed/README.md)
are independently verified and versioned. No independent-seed qualification,
significant improvement or production acceptance is claimed. Unit-test timings
above are not performance evidence.

Neither operator product source, the evaluated branch, the source ledger nor
the evaluator image was changed. The separate cold R9 budget check uses the
comment-only no-op, not this candidate; see
[COLD-R9-PROTOCOL-2026-10-25.md](../../../launch/COLD-R9-PROTOCOL-2026-10-25.md).

## Owner-admission-map hypothesis — draft, untested

[`owner-admission-map.patch`](owner-admission-map.patch), SHA-256
`decaf56673e0690a1bd87df13b820db60d9d01bcf28618e9cefd3439d5a0fbb9`,
is a separate single-file proposal against the same frozen source/blob. It is
not a revision to the warm-reader measurement or an applied baseline change.

Existing per-pair `limiter` instances are accessed only while the manager's
`stateLock` is held, yet each has another mutex, repeated context reference and
heap allocation. The draft stores the last admitted timestamp directly in the
same owner-locked map. Admission, sweeping and the cold-read ownership protocol
keep their existing manager lock. The one-second boundary, cancellation recheck,
capacity ceiling, positive/cache/source deadlines, in-flight protection and
shutdown joins are intended to remain identical. No new I/O, goroutine, global
cache or measurement-aware branch is introduced.

This reasoning is **not proof** of correctness, lower allocations or an
end-to-end improvement. Before any performance pilot, use fresh temporary
checkouts of the exact frozen release and run the full resident-contract and
composed packet-path correctness screen on baseline and candidate, including
race detection, exact admission/expiry boundaries, cancellation and bounded
retention. Any additional tests must be deterministic and use synthetic data.
Then predeclare a separate fresh-control R9 pilot and retain every outcome;
the existing pilot and Stage A controls cannot be reused. No testing, build or
pilot may overlap the running formal calibration. Neither this patch nor a
better-reference label is approved for promotion by this draft.
