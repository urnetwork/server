# October 28 production launch acceptance

Public admission opens **2026-10-28T00:00:00Z**. That date is fixed. This
checklist is the acceptance index for the new release, not an authorization to
activate production. Historical v1 checkmarks do not qualify a different source,
image, host manifest, policy or review provider.

Operator-input update **2026-10-26 05:13 UTC**: the user explicitly cleared
maintenance and approved any file in all ten locked competition repositories,
except the protected simulator/evaluator/scorer/containment instrument and
repository metadata. The dedicated key is provisioned privately and GPT-5.4 mini
model access returned HTTP 200. All competition code/honesty review now requires
`gpt-5.4-mini`/`xhigh` under new policy `elected_winner_gpt54mini_xhigh_v1`;
Sol 6.1 low still owns independent test execution. These resolved inputs are not
live provider qualification, broad-patch qualification, or production activation.
The dated measurement and external observations below remain historical evidence.

Latest update **2026-10-26 15:03 UTC**: all three original R9/R9 pairs are
complete, preserved, checksum-verified and pushed (`f27606218`). Pair 3 passed
all gates and cleanup in 10,160 seconds; its no-op mean was 3.4358% worse,
Welch p=0.7826, with no significance or takeover. The user approved the named
[deadline amendment](CALIBRATION-AMENDMENT-2026-10-26.md): all three original
pairs, one fresh minimal-C2 R9/R9 bridge, then three independent seeds requiring
correct ordering on all three. Original positions 04/05 remain unstarted.
The corrected amended analyzer (`21f371d18`) passed deterministic old/fixed
regression proof and three 14-test runs, zero skips. Its actual reduction
accepted all 54 measured runs with six authenticated replays, 100,000 bootstrap
draws and 99.525% placeability. The [prospective margin is frozen at 26.9%](CALIBRATION-THREE-A-FREEZE-2026-10-26.md),
before bridge/independent outcomes. The minimal C2 image build started at
15:03:34 UTC; it is not yet image-qualified. The first real Mini request returned
HTTP 429 `credit_balance_exhausted`; provider qualification needs funded access,
not another model-metadata probe. Maintenance is cleared and the key is present.
The live API still returns `source_not_ready`; configured MinIO health returns
200, which alone does not prove storage protection. The next paragraph is an
older observation, not current campaign/process authority.

Calibration observation: **2026-10-26T02:30:05Z**; external readiness observations
remain the separately dated checks below. The corrected source/image and isolated
trusted script bundle are prepared. Three consecutive complete control suites
passed 514 tests each, with cleanup verified. See
[network release qualification](NETWORK-BOUNDARY-2026-10-25.md) and the
[prospective replacement protocol](CALIBRATION-RESTART-2026-10-25.md).
The corrected-release first formal cohort started at **15:28:07 UTC**, with a
three-hour deadline at **18:28:07 UTC**, after all five fresh inputs and the
freeze were independently verified and pushed as `2844161a9`. It completed all
nine controls and nine no-op candidates in **10,498 seconds**, 302 seconds
inside the bound. All G1–G6 passed, the pair was placeable/non-significant and
non-takeover, and cleanup was verified. All 481 originals plus four companions
are [preserved and independently verified](../baseline/prelaunch-corrected-same-seed-r9-2026-10-25/completed/cohort-01/README.md).
This is one completed cohort/full-window functional check, not accepted
five-cohort calibration, final scale/margin or a hidden production baseline.
Position 2 completed its original 9+9 attempt at **02:15:37 UTC**, in
**10,300 seconds**, 500 seconds inside its unchanged bound. All gates,
placeability, typed completion and exact cleanup passed. Its mean was 1.16%
worse despite a lower median; p=0.579, not significant or takeover-eligible.
All [481 originals and 7 companions](../baseline/prelaunch-corrected-same-seed-r9-2026-10-25/completed/cohort-02/README.md)
are archived and independently verified, including all 7/465/5 artifact
bindings. This is **2/5 cohorts**, not accepted calibration. Position 3 started
its original attempt at **03:41:45 UTC**, with deadline **06:41:45 UTC**;
positions 4–5 remain unstarted. Assembly/DNS unit guards and reference readbacks
passed in the idle slot and were published as `33e41d6f6`; this does not prove
runtime DNS containment or accept calibration. The latest read-only
API probe returned HTTP 503 and the dedicated review-provider key remains
absent. Current Sille config selects MinIO `172.28.208.176:23900`, which returns
HTTP 200 at `/minio/health/ready`. Earlier `.177` refusals tested the obsolete
endpoint, not this configured service. This corrects the probe, not configuration
or deployment; authenticated locking/versioning/replication/capacity proof is
still pending. User-confirmed main maintenance remains an activation hold.

The API's public 503 body is specifically infrastructure `source_not_ready`:
`the exact runtime source envelope is unavailable` (`retriable: true`). The
process is responding; do not infer a DB outage or failed MinIO connection from
that status alone. The runtime guard requires the exact installed/protected
source envelope and its immutable intent to match. Qualifying the intended
release and its independent readiness acknowledgments remains necessary; no
guard bypass or main restart is authorized by this observation.

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
| Full cold evaluation | Nine fresh controls, nine fresh no-op candidates, authenticated score and unchanged G1–G6 inputs, resource/accounting reports, actual total wall time within three hours, exact cleanup | Passed the prospectively declared corrected-release first cohort: 10,498 seconds, all gates and exact cleanup. Complete [original evidence](../baseline/prelaunch-corrected-same-seed-r9-2026-10-25/completed/cohort-01/README.md) is verified. It is one observation serving two checks, not two independent cohorts; other seeds are not yet qualified. |
| Statistical calibration | Approved named three-pair/bridge/three-seed protocol; unchanged R9, algorithm, gates, significance, integrity and deadlines; retained outcomes and measured variance | Three-pair native reduction accepted; 99.525% bootstrap placeability; prospective practical margin frozen at **26.9%**. Original means changed −2.80%, +1.16%, +3.44%; p=0.297/0.579/0.783; comparison-specific fringes 9.018%, 10.046%, 7.496%, no significance/takeover. Fresh C2 bridge and all-three independent reference orderings remain open. The floor was 16.1%, not the selected margin. Reduced confidence is explicit; production qualification is incomplete. |
| Reference patches | Correctness checks, exact patch/build identities and measured no-op/worse/better behavior; separate pilot and held-out seeds | The [snapshot pilot](SNAPSHOT-PILOT-2026-10-25.md) completed fresh 9+9 with all gates and cleanup in 10,562 seconds. Both predeclared directional conditions passed: mean 19,053.39→18,206.07 ms (4.45% lower), median 18,903.60→18,285.95 ms. Welch p=0.2407: not significant or a takeover; this is a proposed better reference, not held-out qualification. The unfavorable [warm-reader pilot](../baseline/prelaunch-warm-reader-pilot-2026-10-25/completed/README.md) remains preserved. Its unchanged patch has now passed [current-source negative-reference correctness/build qualification](../evaluator/references/2026-10-25/NEGATIVE-QUALIFICATION.md), but fresh independent-seed ordering still must pass. Neither pilot enters Stage A. |
| Host containment and rebaseline | Exact-image host fingerprint, reserved management capacity, network/mount containment, adversarial cleanup and independently authenticated readiness | Local isolation and host bomb evidence exists in the image record. New image-bound host rebaseline and live readiness remain unverified. |
| Honesty and source-test providers | Genuine provider-backed GPT-5.4 mini xhigh review and Sol 6.1 low test execution; retained identities/results for acceptance, rejection, uncertainty and unsafe/prompt-injected input | Dedicated key provisioned; real inference returned HTTP 429 `credit_balance_exhausted`. New policy, isolated broker and fixed acceptance/rejection/uncertainty cases still require genuine qualification after funding. Historical Sol policy receipts retain their original identity. Never expose the key. |
| Automated epoch lifecycle | Real evaluation → provisional decision → review/reject-and-advance → approved source build/qualification → next weekly round; exhaustion carries incumbent, uncertainty pauses; restart recovery | Control suites passed at their recorded revisions in [control validation](CONTROL-VALIDATION-2026-10-25.md). Genuine provider and live runtime lifecycle acceptance remains open; do not equate unit tests with deployed proof. |
| Production policy and timing | New production season and scoped credentials, $20 fee, unbounded FIFO, three-hour job bound, one authenticated R9 control per epoch, six full weekly windows, no early admission | Prepared source ledger is not activation. Inspect deployed policy, API advertisement, supervisor journal and readiness acknowledgments after maintenance ends. Never reinterpret an old staging round as production. |
| Artifact durability and distribution | Exact image/archive digest and retained distribution/readback record; live MinIO locking/versioning, replication, capacity and retention proof | Local image exists; live proof remains open. `support@ur.xyz` owns evidence deletion after retention. Local files alone are not durable artifact acceptance. |
| API, worker and supervisor readiness | Authenticated public contract and submit/poll/result proof, exact runtime manifests, independent API/worker acknowledgments, singleton durable supervisor | Maintenance is explicitly cleared. API source_not_ready remains an exact-envelope deployment/preparation gate, not an external maintenance hold. Install and qualify the new release with separate production inventories; an offline fixture is not a live job/readiness receipt. |
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
