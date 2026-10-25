# October 28 production launch acceptance

Public admission opens **2026-10-28T00:00:00Z**. That date is fixed. This
checklist is the acceptance index for the new release, not an authorization to
activate production. Historical v1 checkmarks do not qualify a different source,
image, host manifest, policy or review provider.

Observation: **2026-10-25T15:29:30Z**. The corrected source/image and isolated
trusted script bundle are prepared. Three consecutive complete control suites
passed 514 tests each, with cleanup verified. See
[network release qualification](NETWORK-BOUNDARY-2026-10-25.md) and the
[prospective replacement protocol](CALIBRATION-RESTART-2026-10-25.md).
The corrected-release first formal cohort started at **15:28:07 UTC**, with a
three-hour deadline at **18:28:07 UTC**, after all five fresh inputs and the
freeze were independently verified and pushed as `2844161a9`. No calibration
acceptance is claimed from an in-progress cohort. The latest read-only
API probe returned HTTP 503, MinIO refused its connection and the dedicated
review-provider key remains absent. User-confirmed main maintenance remains
an activation hold.

The previous Stage A stopped at its first cohort:
nine controls completed, but control 1 failed baseline stability for
`unexpected_recovery`. No candidate build or later cohort started; exact
cleanup was verified. Preserve this failed position, diagnose and qualify the
correction, then publish a new immutable release/protocol before new scoring.
See [the retained failure](../baseline/prelaunch-same-seed-r9-2026-10-25/failed-cohort-01/README.md).
The previous release's cold qualification is terminal,
exit 0, 9+9 complete, all gates passed and exact cleanup verified. Independent
archive verification also passed. See [the complete cold evidence](../baseline/prelaunch-cold-r9-2026-10-25/completed/README.md)
and [current-state.json](current-state.json). This local acceptance does not
substitute for formal calibration or live readiness.

## Release gates and required proof

| Gate | Evidence required for acceptance | Current evidence / remaining action |
| --- | --- | --- |
| Evaluated source and compiler | Exact ten-repository commits, canonical lock, actual binary build metadata and Go 1.27.3; no operator-proxy dependency | Corrected evaluated server `c1f9dda60`, image `002c3904…`, actual Go 1.27.3 and canonical lock `5e00bf5e…` are recorded in [the corrected image qualification](ARTIFACTS-CANCELED-STOP-2026-10-25.md). Preserve these exact identities through subsequent qualification. |
| Full cold evaluation | Nine fresh controls, nine fresh no-op candidates, authenticated score and unchanged G1–G6 inputs, resource/accounting reports, actual total wall time within three hours, exact cleanup | Pending for the corrected release. Prospectively, the first new formal A/A cohort also serves full-window functional qualification, without double-counting observations. The previous release's 10,608-second cold pass is planning evidence only, not acceptance of corrected bytes. |
| Statistical calibration | Predeclared fresh run/seed counts and algorithm, retained failures, measured variance, no-op placement, reference ordering and initial margin supported by this source/image | Not complete. Frontier points and the cold compatibility check are not substitutes. Historical 16.1% is provisional, not a newly measured estimate. |
| Reference patches | Correctness checks, exact patch/build identities and measured no-op/worse/better behavior; separate pilot and held-out seeds | [Warm-reader pilot](../baseline/prelaunch-warm-reader-pilot-2026-10-25/completed/README.md) completed 9+9 with all gates and cleanup, but both candidate mean and median were worse. It failed its predeclared directional screen; no faster reference is qualified, and no retry for a favorable draw is permitted. |
| Host containment and rebaseline | Exact-image host fingerprint, reserved management capacity, network/mount containment, adversarial cleanup and independently authenticated readiness | Local isolation and host bomb evidence exists in the image record. New image-bound host rebaseline and live readiness remain unverified. |
| Honesty and source-test providers | Genuine provider-backed Sol 6.1 max review and Sol 6.1 low test execution; retained identities/results for acceptance, rejection, uncertainty and unsafe/prompt-injected input | Dedicated review key absent at the last check. Fixtures or synthetic model labels cannot satisfy this gate. Never expose the key in logs or reports. |
| Automated epoch lifecycle | Real evaluation → provisional decision → review/reject-and-advance → approved source build/qualification → next weekly round; exhaustion carries incumbent, uncertainty pauses; restart recovery | Control suites passed at their recorded revisions in [control validation](CONTROL-VALIDATION-2026-10-25.md). Genuine provider and live runtime lifecycle acceptance remains open; do not equate unit tests with deployed proof. |
| Production policy and timing | New production season and scoped credentials, $20 fee, unbounded FIFO, three-hour job bound, one authenticated R9 control per epoch, six full weekly windows, no early admission | Prepared source ledger is not activation. Inspect deployed policy, API advertisement, supervisor journal and readiness acknowledgments after maintenance ends. Never reinterpret an old staging round as production. |
| Artifact durability and distribution | Exact image/archive digest and retained distribution/readback record; live MinIO locking/versioning, replication, capacity and retention proof | Local image exists; live proof remains open. `support@ur.xyz` owns evidence deletion after retention. Local files alone are not durable artifact acceptance. |
| API, worker and supervisor readiness | Authenticated public contract and submit/poll/result proof, exact runtime manifests, independent API/worker acknowledgments, singleton durable supervisor | Main is under user-confirmed maintenance. No activation, DB mutation or restart until maintenance is cleared. An offline fixture is not a live job or readiness receipt. |
| Monitoring and operations | Delivered heartbeat every 15 seconds, warning after 30 seconds missing; queue/current job/elapsed/significance/backlog panels; delivered alert routed to support@ur.xyz | Existing implementation and historical deployment evidence do not prove delivery for the new runner. Verify actual ingestion and routing after maintenance. |
| Partner and public handoff | Current immutable contract, source/image distribution, onboarding/revocation proof, Macrocosmos finalize/decision acceptance, published commercial/appeal terms and updated reports | [Partner handoff](APEX-HANDOFF.md) records release deltas. Partner draft still requires reconciliation; final report must use newly qualified data and show improvement fringes wherever baselines appear. |

## Execution order

1. Publish the completed cold phase and preserved outcomes before the next
   larger validation. Keep every subsequent scoring attempt serial under the
   shared slot; do not overlap it with builds, tests or resource-pressure work.
2. Resolve any evidenced defect or scale/budget limit, then complete the fresh
   predeclared calibration and reference qualification. Changing evaluated code
   or its instrument requires a new immutable release and matching evidence.
3. Once main maintenance ends, complete exact-image host and artifact proofs,
   genuine provider qualification, scoped policy and independent readiness.
   Pure preparations can proceed separately; they must not perturb scoring.
4. Schedule the qualified production season for the fixed opening. Verify the
   advertised future start, rejection of early submissions, automatic lifecycle,
   monitoring and partner handoff before declaring launch readiness.

At final sign-off, replace each pending item with its exact immutable receipt,
test/log digest or deployed observation. A plausible configuration, a green
subset of tests or the absence of a known failure is not proof of completion.
