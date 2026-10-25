# October 28 production launch acceptance

Public admission opens **2026-10-28T00:00:00Z**. That date is fixed. This
checklist is the acceptance index for the new release, not an authorization to
activate production. Historical v1 checkmarks do not qualify a different source,
image, host manifest, policy or review provider.

Observation: **2026-10-25T06:12:28Z**. The cold qualification is terminal,
exit 0, 9+9 complete, all gates passed and exact cleanup verified. Independent
archive verification also passed. See [the complete cold evidence](../baseline/prelaunch-cold-r9-2026-10-25/completed/README.md)
and [current-state.json](current-state.json). This local acceptance does not
substitute for formal calibration or live readiness.

## Release gates and required proof

| Gate | Evidence required for acceptance | Current evidence / remaining action |
| --- | --- | --- |
| Evaluated source and compiler | Exact ten-repository commits, canonical lock, actual binary build metadata and Go 1.27.3; no operator-proxy dependency | [Source preflight](SOURCE-PREFLIGHT-2026-10-24.md) and [image record](ARTIFACTS-READY-SNAPSHOT-2026-10-25.md) identify the prepared release. Preserve these exact identities through subsequent qualification. |
| Full cold evaluation | Nine fresh controls, nine fresh no-op candidates, authenticated score and unchanged G1–G6 inputs, resource/accounting reports, actual total wall time within three hours, exact cleanup | Passed locally under the [predeclared protocol](COLD-R9-PROTOCOL-2026-10-25.md): 10,608 seconds, 192 seconds headroom; no-op placeable/non-takeover. Every original byte and binding is independently verified. One public-seed execution is not proof that every seed fits. |
| Statistical calibration | Predeclared fresh run/seed counts and algorithm, retained failures, measured variance, no-op placement, reference ordering and initial margin supported by this source/image | Not complete. Frontier points and the cold compatibility check are not substitutes. Historical 16.1% is provisional, not a newly measured estimate. |
| Reference patches | Correctness checks, exact patch/build identities and measured no-op/worse/better behavior; separate pilot and held-out seeds | [Warm-reader candidate](../evaluator/references/2026-10-25/README.md) has correctness evidence only. No measured better reference is qualified. |
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
