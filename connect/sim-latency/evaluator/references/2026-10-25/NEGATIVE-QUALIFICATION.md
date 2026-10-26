# Warm-reader patch: proposed negative control

The unchanged `warm-readers.patch`, SHA-256
`2957ef507f7edc9dadcb09abd6425664e88748612917959a9a65243a0365ebe4`,
is proposed as Stage B's worse control. It was originally a faster-reference
hypothesis. The [retained earlier-release pilot](../../../baseline/prelaunch-warm-reader-pilot-2026-10-25/completed/README.md)
passed all gates but was 17.8346% slower by mean raw score. That unfavorable
result was preserved, not rerolled. It is prior-release planning evidence,
**not measured performance on the corrected release**, and none of its
measurements enters Stage A or Stage B.

This choice does not weaken a gate, insert an artificial wait, alter the
workload or assume every future seed will regress. The fixed held-out ordering
test must still verify `better < no-op < worse` on at least four of five fresh
independent seeds. Freeze this exact patch/build identity before those results;
do not replace it after seeing an unfavorable seed.

## Current-release correctness

A fresh private source tree came from exact image
`sha256:002c39040ce23dee27b171c7e657f749c6a6d05b1e9b97d90ac87336470dbaf3`,
including server `c1f9dda603d6279666fca5d92b99c5663306c5f7` and the unchanged
other nine repository pins. The original resident-manager blob is still
`ba3c09f7960f789b37a262b1542ee4dbdca93996`; only that file received the exact
archived patch. No snapshot-specific fixture was added.

Actual Go 1.27.3/local, management CPUs 20,22, GOMAXPROCS=2 and `-p=2` were used
with disposable synthetic-local PostgreSQL/Redis and the canonical exclusive
slot. All 52 resident/packet correctness tests passed three consecutive
race-enabled runs: **156 new passing test executions**.

| Pass | Wall seconds | Full log SHA-256 |
| --- | --- | --- |
| 1 | 231 | `ebe015b3790b38af7ee63efaa6780bd4b1c896e90757c8705b14851dc7f5e589` |
| 2 | 65 | `6d4357e8c090da1eef31b6af6295f4045020f6e34faf4aea9a319324f03abc2c` |
| 3 | 65 | `2b0c66ab9e4f4ce450212bdd2120d00eebbe105394aaa2113b3e4ce0519e2f17` |

Baseline correctness is the already recorded three 52-test passes on the exact
same corrected source/image, linked in [the snapshot screen](SNAPSHOT-QUALIFICATION.md).
That is explicitly reused **test evidence**, not new independent tests or
reused timing/control measurements. The old 312-test screen and old slower
pilot are not relabeled as corrected-source executions.

## Offline build from a second clean checkout

A second fresh ten-repository source tree, without transient test files or an
already applied patch, was supplied to the immutable qualified builder. It
applied/committed the patch itself and built offline within 213 seconds.

| Identity | Exact value |
| --- | --- |
| Candidate commit | `e445ffb75fc3305df6c1ccd5d99e768e45e1142e` |
| Candidate image | `sha256:c675a2158dc3a3a0238a4421c8486d51051e09b40698c1fc92aff843150c355c` |
| Binary SHA-256 | `fc3709dc5dcd2448f903902d801d0e5dff6ce0274142b012fd5c029892dc9b83` |
| Exact runtime patch-policy bytes | `0a1cb9032ee630042f1a1fe585493fba6f7b3fc10b71b697de39cebe43e00a1e` |
| Composite builder identity | `0c0dff0ad9176986c26d3413b0ef8259859039b6648d6ec3a776354bedf3dd81` |
| Builder entrypoint file | `460a6bb91f8ba20f818f4bcf8b245840aa796f34e9710edf5c359c7c4d4227de` |

Actual binary Go 1.27.3 and clean VCS metadata were verified by readback.
Build log SHA-256:
`934ff275af9a0936583de6119a80d8ed8df3ed7a07203a4501bdf68847c172d7`.
Build metadata SHA-256:
`f856d61a134173378ebf48e2870e995b4883208cff4b918fd7d99b81a6cdfb8c`.
Readback log SHA-256:
`98e266f6d9b2a212519eb56fcdab2a671c2630eb90420075b6205b89446499c5`.

The canonical slot covered setup, tests, build and cleanup. Controller session
46603 finished with cleanup 0, original three containers/four networks and
the slot free. The new candidate cache image is deliberately retained, not an
orphaned evaluation container. No simulator, score, held-out seed or formal
cohort was run in this qualification.

Full local evidence:
`temp/warm-readers-negative-qualification-20261025.15cNHvs8/`.
[The versioned dataset](../../../baseline/prelaunch-warm-reader-negative-build-2026-10-25/README.md)
preserves 13 selected original evidence files, with full source checkouts,
service credentials, binary payloads and caches explicitly excluded. Archive
SHA-256: `f626aa1c328126ac11377e675301d93243448da54b22ff63fabd13209c0d854b`.
Independent source re-read/stream-verification log SHA-256:
`dda295d3c7f7c16e3d3b5ed25054302757040c4c05b6e79d0633a2754c79d6cb`.
This is correctness/build qualification only. Current-release negative
performance, held-out ordering, production readiness and immutable MinIO
retention are not claimed.
