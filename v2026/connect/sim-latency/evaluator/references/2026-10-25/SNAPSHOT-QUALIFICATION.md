# Snapshot reference: correctness and supporting benchmark screen

This local screen passed. It does **not** qualify a faster end-to-end reference,
change the frozen baseline or provide production runtime/provider readiness.
The exact patch remains
`46bc95e3f2719f26f939be7a86c274d0b98bb2b1027f1ef4537d61de4c4ec924`.

Fresh ten-repository source came from evaluator image
`sha256:002c39040ce23dee27b171c7e657f749c6a6d05b1e9b97d90ac87336470dbaf3`,
including server `c1f9dda603d6279666fca5d92b99c5663306c5f7`.
Actual Go 1.27.3/local, management CPUs 20,22, GOMAXPROCS=2 and `-p=2` were used.
The canonical exclusive slot covered setup, tests, benchmarks and cleanup.
Only disposable synthetic-local PostgreSQL/Redis resources were used.

## Deterministic correctness evidence

The baseline passed all 52 selected resident/packet tests three times. The
candidate passed all 60 selected tests, including eight new tests, three times.
Two separate guard-removal controls then compiled and failed at their intended
barrier assertions: removing the final deadline check admitted an expired lease;
removing the final closed-owner flag admitted a selected hit while cancellation
was held back. Neither result was a compile failure or a timing-luck test.
The candidate was restored byte-for-byte and all 60 tests passed three more
times: **516 passing race-enabled executions**, plus the two expected failures.

| Phase | Wall seconds | Full log SHA-256 |
| --- | --- | --- |
| Baseline 1 | 232 | `e43c0b365ef4ce8ca7e24534c9a95227f990110a6ca8651d24f4b0d0cd9e4a48` |
| Baseline 2 | 65 | `c70a41ddc886c2cc6ea4bf478996d763672ac01b23a3271bd2ee4cb4c18068c4` |
| Baseline 3 | 66 | `e226ef7121c6c05a102eb10de8a3cd9849a0643454512dfb6b346d67f802b466` |
| Candidate 1 | 84 | `4722fd4e172672e8e2f0f21d7e42cad930ca6c05f03444bec36ae8102831741f` |
| Candidate 2 | 66 | `b2770d4f150d18824c7bd97514ca683c94fce3a1b2974c2591118562dd031376` |
| Candidate 3 | 66 | `658b377d5a8bfd73e83f93f7a77f15f61d4db4b8843a4a0d86cecfd91c5bef9d` |
| Deadline guard removed | 26 | `7b10347efccfc1d22d4cd9a48045c819159470349b6e4a586570432a068c2886` |
| Closed-owner guard removed | — | `fc9f549d9f4ef11218737385d9f9f606f975ede1c73f67c0fdebcea6c88f9554` |
| Restored 1 | 66 | `d13ea385281c921250c8e1812b0202f2a4621799f6c980d47ad3603139f7100c` |
| Restored 2 | 66 | `3c75f1aabd789b46456877f1059235bd421d119172e6ba91a21364bdff1f3468` |
| Restored 3 | 66 | `20d323a24553ba5018dd7f29a4007253f4afc5b874d646392eeb0f33b41b0c49` |

The exact restored candidate file SHA-256 is
`3f4914ff712f9fb99a34fd38fd3eb30d4f3a28c0629e91813adc7d3691a80358`.
Qualification fixture: `26c459d5bf580a94de34b45636847a6b6880d190681d66bd5df52d2ca36bd40f`.
Common benchmark fixture: `f7a84b34cdcacf09e57004259410d43ef7b48f4d0663503b51add584aab28060`.

## Supporting observations: retain both benefit and cost

Both sources used the same uninstrumented benchmark, five observations at
100 ms per benchmark. The warm benchmark uses actual constructor/default
freshness policy and 32 callers on two CPUs; every access must remain a hit,
with no source refresh/refusal. Publication separately measures the registered
read-worker seam with 128 live pairs, excluding Redis/admission latency.

| Mechanism | Baseline ns/op, all five | Candidate ns/op, all five |
| --- | --- | --- |
| Warm hit | 390.4, 339.4, 393.0, 333.2, 410.1 | 102.8, 103.3, 103.5, 104.0, 104.3 |
| Refresh publication | 472.0, 459.2, 465.6, 459.9, 472.4 | 19,720, 18,811, 18,672, 18,772, 18,746 |

Warm-hit median fell **73.4887%**, 390.4 → 103.5 ns/op; both arms used
0 B/op and 0 allocations. Publication median rose **40.3179×**,
465.6 → 18,772 ns/op; allocation rose from 48 B/1 allocation to
21,904 B/6 allocations per operation. Copying the map is a real cost, not a
free optimization. These narrow observations do not predict the ten-core
workload's score, memory behavior, significance or takeover eligibility.

Benchmark logs:

- baseline: `67e9336ed04e7a22fc3c95c7063de93e59447d8b623ad6187edeb400b6b6b993`;
- candidate: `dee9b189c109d7990f258ef813dad7ee26f90f73cd19074579b133b71a8dc985`;
- complete launch/identity receipt: `94ea4b7e429ff6ffed212a2059634ac78ba2ca49e69d11f6e1fed8584591644d`.

No samples were rerolled. The warm-hit screen met its prospective directional
criterion, so one separately predeclared fresh-control R9 end-to-end pilot is
warranted. Publication/memory cost remains an explicit risk. A pilot failure
must be retained and may not be rerun for a favorable draw.

## Retention and cleanup

Full local evidence is retained in
`temp/snapshot-screen-qualified-setup-20261025.Eqyjghne/`; benchmark launch and
the first setup diagnostic are retained in
`temp/snapshot-screen-independent-20261025.BxTAR1o5/`.
Controller session 42566 exited 0 with cleanup 0 after exact candidate/fixture
restoration. The original three containers/four networks were restored; the
slot was free and no test process remained. The
[versioned screen dataset](../../../baseline/prelaunch-snapshot-screen-2026-10-25/README.md)
preserves all 30 selected original evidence files in a stream-verified archive,
plus exact readable benchmark copies. Full source checkouts, service credentials
and caches are explicitly excluded; source pins/patch/fixtures reproduce the
tested code. Archive SHA-256:
`9115aeb65fcdd5bfa94adc32bd6d9701106c96c4b7461d2199a1e49939712b90`.
Independent archive-verification log SHA-256:
`e5c22d1f00244c1825ed82bc67d53075499d77eb0e1ea5cf02bac02cbc327e82`.
This is not immutable MinIO proof.

Two initial setup attempts executed zero tests: one selected an old SN
directory outside the frozen Go workspace; the next preflight assumed the
repository name was its module path. The actual `github.com/urfoundation/sn`
module-path positive control passed before the successful fresh setup. Both
diagnostics and cleanup receipts are retained; neither is a discarded
candidate measurement. No scoring, pilot or second calibration cohort ran in
this screen.
