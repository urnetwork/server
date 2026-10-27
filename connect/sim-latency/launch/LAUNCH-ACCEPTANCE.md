# October 28 production launch acceptance

Public admission opens **2026-10-28T00:00:00Z**. That date is fixed. This
checklist is the acceptance index for the new release, not an authorization to
activate production. Historical v1 checkmarks do not qualify a different source,
image, host manifest, policy or review provider.

## Current checkpoint — October 27, 02:08 UTC

The following supersedes the provider and baseline-preparation progress in the
dated bullets below; those bullets retain the earlier qualification history.

- Genuine seven-key test-result attestation passed: two actual isolated Go
  executions (one passing, one deliberately failing), then six provider calls.
  Receipt SHA-256 `0784336a82e4fa899aa3c61d7961774135cb5c738d48a15e68af90eed82f591f`.
  This proves mechanical observation attestation, not winner honesty or the
  full winner test suite.
- A subsequent review qualification exposed inconsistent member/global reason
  handling. Fix `b316e0d406349bd4c281473b4692fb02eb6aacaa` preserves the original
  findings and applies the same disqualification reason projection at both
  validation boundaries. Deterministic pre-fix controls and 444 selected test
  executions passed before the fresh attempt. Its immutable precommit is
  `SNOW-REVIEW-QUALIFICATION-PRECOMMIT-2026-10-27.json` (published in `9f7a29329`).
  That attempt stopped at its first failure after 1,208.82 seconds, exit 1,
  cleanup verified. Cancellation and benign approval passed; the fabrication
  case exposed another member-validation projection error: a supported
  fabrication rejection with one uncertain criterion was passed to the legacy
  finding validator. The original findings and failed attempt are preserved;
  no retry or complete six-case qualification is claimed. Report SHA-256
  `347920e25e3af5fc45ab7500c36a9379bbea63081ee332ace6d7054b572f64d1`.
  The earlier HTTP-200 response lacking
  a retained terminal object remains inconclusive, not evidence of token
  exhaustion or a valid model identity.
- The mixed-uncertainty correction now shares strict Snow validation while
  distinguishing one member's six unattributed findings from the public union's
  eighteen attributed findings. Independent review found no blocker; 12 model
  and 35 simulator smoke tests passed with zero skips. A compiled old-projection
  control reproduced the exact failure, then restored-source tests passed again.
  Receipt SHA-256 `25d969c22eb793d594173593f726c95bb2df49f4d561b3c37c8a2173a41a4abc`.
  Three-pass and genuine-provider qualification remain pending.
- Shared-baseline preparation before the first submission clock is committed
  in `cf22d1a05`, with 19 smoke tests passing, including real isolated PostgreSQL
  cases. Independent review then found a claim/reservation race. Its correction
  and deterministic transaction-boundary regressions are in progress; the
  feature is still opt-in, not installed or production-qualified. Its corrected
  claim/scanner smoke subsequently passed 23 controller tests plus two shell
  guards, zero skips, with original container/network inventories restored.
  Compiled old-source controls reproduced all three races and both failed-scan
  pointer defects. Larger validation remains pending.
  Fixes are pushed in `c37f9cea0`. The separate v4 helper and exact profile are
  installed inactive: 17 root-owned read-only artifacts, unchanged v3 dependency
  hashes, no helper or worker execution and no service activation. Installation
  receipt SHA-256 `e51f219353e2f1c912c4beda8a9d12f0394e1524b3efba0b9036ff3d76d47db7`.
- Replacement bridge authorization and the independent-seed amendment remain
  unresolved. No corrected bridge or independent-seed scoring has started.
- Actual main PostgreSQL was audited at 01:33:35 UTC in a verified read-only,
  repeatable-read transaction. All 890 deployed migration identities match;
  migrations **891–893** (mini provider, multi-repository promotion, Snow union)
  remain pending normal deployment. All 12 existing rounds are staging, all 35
  jobs are terminal, and the global worker slot is unowned. The intended
  production namespace is empty. Recheck these facts before bootstrap; the
  audit neither creates a round nor authorizes deleting historical state.
  Receipt SHA-256: `a30144529f1b657f4dfa1fab79ba394f3689541e8cb752aad0fb081d8c5fe1ea`.

- The frozen performance source is C2 server `a9e8e1a7`, ten-repository lock
  `ec322ed2…`, image `sha256:b1632a55…`, actual Go **1.27.3**. Operator-proxy
  is not a dependency. The new control-plane fixes do not change that source.
  All ten remote `sim-latency` branch heads were independently checked against
  that checkpoint at 23:44 UTC; see the [remote branch receipt](FROZEN-SOURCE-CHECKPOINT-2026-10-26.json).
- The three original C1 pairs are accepted evidence for the **26.9%** margin.
  The single fresh C2 R9/R9 bridge **failed**, exit 1 at **22:22:15 UTC** after
  5,239 seconds; exact cleanup is verified. All nine baseline simulations
  completed, but an exact binary64 metadata comparison rejected `26.9` versus
  `0.269 × 100` before baseline scoring. The image already has the correct
  **26.9%** source configuration; it is not a 16.1% configuration error.
  Nine original raw baseline outputs are retained **unscored**; zero candidate
  runs occurred. Exact byte/file/native-sidecar/finalized-segment authentication
  passed, without decompression or semantic scoring. The existing protocol
  cannot resume this attempt; a new recovery/protocol decision remains open.
  No rerun, resampling, new independent
  draw or bridge acceptance is authorized by this failure. Historical reports
  and the sealed reviewed exclusion catalog remain unchanged.
- A thin shell-only overlay, image `sha256:98c9061c…`, built and read back in
  five seconds. It changes only `official-run.sh` and the image entrypoint;
  frozen binary `efd1077d…`, C2 source, source configuration `589ab624…` and
  raw lock are unchanged. Its stopped readback container was removed. A separate
  actual non-root, read-only, offline `margin-preflight` passed with the exact
  frozen binary, source epoch and margin. This is not a successful R9/R9 bridge.
  The separate inactive host bundle changes only evaluator/entrypoint shells;
  its new builder identity is `ca3c84c6…`. The inactive root-owned manifests now
  bind actual correction commit `ed028faa…`; the new runner has not executed.
  Complete independent QA, newly authenticated references and any fresh
  measurement authorization remain gates; no old image or build receipt is
  relabeled.
- Runtime model calls use Snow's **Qwen3.8 27B + Gemma 4 26B A4B + Muse Glimmer
  30B**, serially, with unanimous approval and all attributed findings retained.
  No OpenAI key or credit is required. Independent maintainer tests use Sol 6.1
  low. The genuine three-model attempt failed closed on response-contract and
  evidence validation; it did not qualify the provider. Schema, diagnostic,
  cache and host-probe fixes passed a 173-test smoke run and six compiled
  pre-fix regression controls. Current clean-main simulator/controller/model
  scopes passed three times (145/19/10 tests per pass; zero skips); the separate
  60-test native scope also passed three times. That is 702 selected test
  executions, not real provider or calibration runs. Fixes are pushed
  through `ebf0b61ce`. Scratch image `sha256:c5afcdc7…` passed actual non-root
  offline startup after an explicit COPY-mode fix; this is image qualification,
  not genuine review-provider qualification.
  The distinct seven-key source-test attestation still needs its own genuine
  pass/fail Go-observation qualification; honesty-review cases do not cover it.
- Current-boot CPU/memory-bomb cleanup passed in **653 ms** with a real 72 GiB
  runner OOM. The 5 GiB workspace passed broad ten-repository offline vet/build;
  that build executed zero assertion tests. Neither fact is final host readiness.
- MinIO source and replica exact-version reads match, including payload and
  COMPLIANCE retention through **2028-01-01**. The replica is co-located on
  edge-5, not off-site. The approved **10 TB decimal** allocation is not a proven
  bucket quota. Native read-only version-aware accounting passed at 23:38 UTC:
  464 retained versions, 39,153,130 bytes used against that allocation; zero
  writes. Physical free space is not proved. Final artifact publication remains
  open.
- The first queued submission currently shares its three-hour budget with
  baseline preparation, leaving little measured cold-build headroom. A separately
  bounded, durable shared-baseline preparation path before the submission clock
  starts is being implemented; it does not relax the three-hour candidate cap.
- Production is **not activated**. Source/runtime acknowledgments, a scheduled
  October 28 round, live heartbeat/alert delivery and partner handoff still need
  proof. Use [current-state.json](current-state.json) for dated handles and
  hashes and [the preview](../baseline/final-preview.html) for the four-section
  visual summary. The gate matrix below remains authoritative about open work.

<details>
<summary>Historical dated observations — superseded provider and process states, not current policy</summary>

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

</details>

## Release gates and required proof

| Gate | Evidence required for acceptance | Current evidence / remaining action |
| --- | --- | --- |
| Evaluated source and compiler | Exact ten-repository commits, canonical lock, actual binary build metadata and Go 1.27.3; no operator-proxy dependency | C2 server `a9e8e1a7`, image `b1632a55…`, binary `efd1077d…`, actual Go 1.27.3 and canonical lock `ec322ed2…` are recorded in the [bridge precommit](../baseline/prelaunch-c2-bridge-2026-10-26/README.md). C1 identities remain historical A evidence, never relabeled C2. |
| Full cold evaluation | Nine fresh controls, nine fresh no-op candidates, authenticated score and unchanged G1–G6 inputs, resource/accounting reports, actual total wall time within three hours, exact cleanup | Passed the prospectively declared corrected-release first cohort: 10,498 seconds, all gates and exact cleanup. Complete [original evidence](../baseline/prelaunch-corrected-same-seed-r9-2026-10-25/completed/cohort-01/README.md) is verified. It is one observation serving two checks, not two independent cohorts; other seeds are not yet qualified. |
| Statistical calibration | Approved named three-pair/bridge/three-seed protocol; unchanged R9, algorithm, gates, significance, integrity and deadlines; retained outcomes and measured variance | Three-pair native reduction accepted; 99.525% bootstrap placeability; prospective practical margin frozen at **26.9%**. Original means changed −2.80%, +1.16%, +3.44%; p=0.297/0.579/0.783; comparison-specific fringes 9.018%, 10.046%, 7.496%, no significance/takeover. C2 bridge failed after nine complete but **unscored** baseline simulations, zero candidates: shell binary64 metadata comparison, not a wrong source margin. Raw byte/sidecar authentication passed, without semantic scoring. No continuation or resampling is authorized; a new protocol decision remains open. Bridge acceptance and all-three independent reference orderings remain open. The floor was 16.1%, not the selected margin. Reduced confidence is explicit; production qualification is incomplete. |
| Reference patches | Correctness checks, exact patch/build identities and measured no-op/worse/better behavior; separate pilot and held-out seeds | The [snapshot pilot](SNAPSHOT-PILOT-2026-10-25.md) completed fresh 9+9 with all gates and cleanup in 10,562 seconds. Both predeclared directional conditions passed: mean 19,053.39→18,206.07 ms (4.45% lower), median 18,903.60→18,285.95 ms. Welch p=0.2407: not significant or a takeover; this is a proposed better reference, not held-out qualification. The unfavorable [warm-reader pilot](../baseline/prelaunch-warm-reader-pilot-2026-10-25/completed/README.md) remains preserved. Its unchanged patch has now passed [current-source negative-reference correctness/build qualification](../evaluator/references/2026-10-25/NEGATIVE-QUALIFICATION.md), but fresh independent-seed ordering still must pass. Neither pilot enters Stage A. |
| Host containment and rebaseline | Exact-image host fingerprint, reserved management capacity, network/mount containment, adversarial cleanup and independently authenticated readiness | Current-boot production-size CPU/memory-bomb cleanup passed in 653 ms; 5 GiB broad-build workspace passed. Final C2 image/actual-round host rebaseline, production metadata binding and readiness remain unverified. Calibration fixture hardware labels are not positive host attestations. |
| Honesty and source-test providers | Genuine Snow three-member review/test-broker qualification; maintainer tests on Sol 6.1 low; retained acceptance/rejection/uncertainty/unsafe/prompt-injection evidence | No key/credit dependency. Latest wrapper passed 45 guards three times plus three deterministic failure controls. Real cancellation passed, but Qwen response identity/status was rejected, Gemma supplied incomplete evidence, and Muse returned the wrong judgment shape. All were retained and held; fixed schema/diagnostics/cache need tests, new broker image and fresh full qualification. No model/effort/timeout downgrade. |
| Automated epoch lifecycle | Real evaluation → provisional decision → review/reject-and-advance → approved source build/qualification → next weekly round; exhaustion carries incumbent, uncertainty pauses; restart recovery | Control suites passed at their recorded revisions in [control validation](CONTROL-VALIDATION-2026-10-25.md). Genuine provider and live runtime lifecycle acceptance remains open; do not equate unit tests with deployed proof. |
| Production policy and timing | New production season and scoped credentials, $20 fee, unbounded FIFO, three-hour job bound, one authenticated R9 control per epoch, six full weekly windows, no early admission | Prepared source ledger is not activation. Inspect deployed policy, API advertisement, supervisor journal and readiness acknowledgments after maintenance ends. Never reinterpret an old staging round as production. |
| Artifact durability and distribution | Exact image/archive digest and retained distribution/readback record; live MinIO locking/versioning, replication, capacity and retention proof | Source and replica exact-version payload/COMPLIANCE-retention readback passed; native bucket-protection proof passed. Replica is co-located, not off-site. Ordinary replica access remains denied; verified administrative restore did not broaden it. Version-aware accounting passed: 464 versions / 39,153,130 bytes against the approved decimal 10 TB allocation (not a physical quota/free-space proof). Final retained publication and control-image distribution remain open. `support@ur.xyz` owns post-retention deletion. |
| API, worker and supervisor readiness | Authenticated public contract and submit/poll/result proof, exact runtime manifests, independent API/worker acknowledgments, singleton durable supervisor | Maintenance is explicitly cleared. API source_not_ready remains an exact-envelope deployment/preparation gate, not an external maintenance hold. Install and qualify the new release with separate production inventories; an offline fixture is not a live job/readiness receipt. |
| Monitoring and operations | Delivered heartbeat every 15 seconds, warning after 30 seconds missing; queue/current job/elapsed/significance/backlog panels; delivered alert routed to support@ur.xyz | Existing implementation and historical deployment evidence do not prove delivery for the new runner. Verify actual ingestion and routing after maintenance. |
| Partner and public handoff | Current immutable contract, source/image distribution, onboarding/revocation proof, Macrocosmos finalize/decision acceptance, published commercial/appeal terms and updated reports | [Partner handoff](APEX-HANDOFF.md) records release deltas. Partner draft still requires reconciliation; final report must use newly qualified data and show improvement fringes wherever baselines appear. |

## Execution order

1. Publish the completed cold phase and preserved outcomes before the next
   larger validation. Keep every subsequent scoring attempt serial under the
   shared slot; do not overlap it with builds, tests or resource-pressure work.
2. Qualify the metadata-precision shell correction and preserve the completed
   read-only byte/sidecar authentication of nine unscored bridge baselines.
   Obtain an explicit new protocol
   decision before any scoring; this failure authorizes no rerun or resampling.
   A thin overlay may change only the reviewed shell files, with the frozen
   application binary verified unchanged; publish its new immutable identity.
   Bridge and independent reference acceptance still require genuine evidence.
3. Main maintenance is cleared. Complete exact-image host and artifact proofs,
   genuine provider qualification, scoped policy and independent readiness.
   Pure preparations can proceed separately; they must not perturb scoring.
4. Schedule the qualified production season for the fixed opening. Verify the
   advertised future start, rejection of early submissions, automatic lifecycle,
   monitoring and partner handoff before declaring launch readiness.

At final sign-off, replace each pending item with its exact immutable receipt,
test/log digest or deployed observation. A plausible configuration, a green
subset of tests or the absence of a known failure is not proof of completion.
