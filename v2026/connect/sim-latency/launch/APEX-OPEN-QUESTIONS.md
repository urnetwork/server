# Sim-latency: remaining Apex launch inputs and actions

Status: ready to share with the Apex/Macrocosmos team, 2026-08-29.

Macrocosmos design approval is complete. UR is in direct communication with
Macrocosmos, and Macrocosmos has approved the sim-latency competition design,
including every deliberate difference from the ordinary Apex sandbox profile.
This document does not ask the Apex team to approve those decisions again. It
contains only the concrete values, owners, release records, and staging actions
still needed to activate the competition through Apex.

## Approved contract — no response required

- Six epochs with exactly seven days of admissions per epoch and a 16-hour
  preparation/rebaseline interval before the next epoch.
- An asynchronous external evaluator on UR's qualified host rather than the
  standard Apex player/referee sandbox resource profile.
- Ten evaluation CPUs, approximately 13 GiB at the frozen scale, PostgreSQL and
  Redis in the evaluation boundary, and a three-hour hard limit per score job.
- Immediate durable FIFO evaluation, unbounded paid submissions, and continued
  grading after the submission window closes until every admitted job is
  terminal.
- A fixed $20 USD fee for every production Apex identity, collected exactly
  once before the UR submission API is called. Epoch-zero staging identities
  are explicitly fee-free and never evaluated.
- Scores, ranks, and evaluation errors remain embargoed through epoch close,
  complete backlog drain, and manual honesty review. Only the atomically
  finalized leaderboard is public.
- UR's epoch-specific improvement margin and one-sided Welch significance test
  replace the ordinary one-percent Apex solo takeover rule. Epoch 1 starts at a
  16.1% margin; every evaluation records its variance and significance result.
- A statistically significant submission is only a review candidate. The first
  honest significant candidate wins; if none remains, there is no winner and
  the source commits and threshold carry forward.
- Every epoch locks the `sim-latency` branches of `server`, `connect`, `sdk`,
  `proxy`, `glog`, `goidenticons`, `userwireguard`, and `sn`. A winning canonical
  patch changes only the evaluated server-tree surface; unchanged repository
  commits carry forward. The main API and worker remain continuously maintained
  control-plane services.

## 1. Identifiers, calendar, and economics

- [ ] Provide the Apex competition id and immutable release/spec version that
  map to UR competition id `sim-latency`.
- [ ] Provide the epoch-1 activation time in UTC. UR will derive subsequent
  epoch start/end times after each drain, review, promotion or carry-forward,
  and rebaseline.
- [ ] Provide the final incentive weight.
- [ ] Provide the reward amount, asset/currency, payout cadence, and payer.
- [ ] Provide the final fee receipt and refund policy for invalid patches,
  build failures, three-hour timeouts, infrastructure failures, and
  canonical-patch cache hits. Transport retries never collect a second fee.
- [ ] Provide participant eligibility, geographic/KYC restrictions,
  wallet/account rules, and any participant-level submission restrictions. The
  UR evaluator itself has no submission cap.
- [ ] Provide the disqualification notification, appeal, and abuse process for
  dishonest submissions.
- [ ] Decide whether Apex publishes finalized rows by job/patch id only or joins
  those ids to a public miner identity in its own publication layer.

## 2. Integration ownership and release identity

- [ ] Name the Apex product owner and integration engineer.
- [ ] Record whether Apex will integrate the external evaluator directly into
  its platform or operate a standalone adapter. UR provides a durable Go
  reference adapter and conformance suite; it is not itself a deployed Apex
  service.
- [ ] Record the exact stage and production release identity for that
  integration: an image digest and cosign identity for a standalone adapter, or
  the equivalent digest-pinned Apex platform release for direct integration.
- [ ] Confirm the transport mapping supplies one stable Apex submission id and
  the exact canonical text patch, up to 262,144 bytes, for the reviewed
  server-tree patch surface. Player-built images and dependency-repository
  patches are not submitted to the UR API.
- [ ] Confirm durable idempotency across HTTP 429, typed retriable 5xx,
  connection loss, and process restart without a second fee or admission.
- [ ] Record how this approved external-evaluator integration is represented in
  the private Apex registry, including its registry entry identifier.
- [ ] State whether Apex requires any additional screening beyond UR's
  structural patch validation, isolated evaluator, statistical gates, and
  manual honesty review.

## 3. Credentials, staging, and operational handoff

- [ ] Name the Apex credential custodian, notification owner, incident contact,
  and legal/abuse owner. UR's operational and evidence-deletion contact is
  `support@ur.xyz`.
- [ ] Choose a private credential-delivery channel and complete one stage-token
  issue, rotation, and revocation exercise. Apex receives only a submitter
  credential—not a hidden seed, MinIO credential, operator token, Docker
  socket, or candidate filesystem.
- [ ] Complete one epoch-zero API stage submission and reconcile the Apex
  submission id, `job_id`, `round_id`, `staging: true`, `patch_sha256`,
  immutable `status_url`, and the expected `staging_discarded` cancellation
  when epoch 1 is committed. Staging has no leaderboard row; reconcile the
  first production result with the finalized leaderboard separately.
- [ ] Record the stage and production activation identifiers.
- [ ] Confirm the participant-facing status text for queued work, a long
  post-close drain, no-winner epochs, dishonest-candidate rejection, delayed
  epoch openings, and incidents. No provisional score is public during the
  embargo.
- [ ] State any Apex-required minimum evidence-retention date. If Apex has no
  additional requirement, UR will set `retain_until` under its retention
  policy.

## 4. Onboarding package and final record

- [ ] Specify which ordinary onboarding artifacts are required, replaced, or
  waived for the approved external-evaluator path: `spec.yaml`, player image,
  referee image, optional screen image, baseline submission, adversarial
  submission set, and per-task evaluation records.
- [ ] Confirm whether UR should use the public Competition onboarding issue or
  a private intake route for this approved non-standard integration, and name
  the Macrocosmos reviewer.
- [ ] Provide a private channel for the threat model and security questionnaire.
  Apex's builder says defenses belong in the handoff rather than miner-visible
  documentation, so sensitive review material must not be pasted into a public
  issue.
- [ ] Name the Macrocosmos registry/activation owner and provide the durable
  private-registry review record after UR submits the package.
- [ ] Identify the authorized Macrocosmos signer and the durable location for
  the final acceptance record. UR will identify its signer.
- [ ] Countersign the final handoff containing the approved integration path,
  release identities, staging proof, registry activation, economics,
  participant policy, named owners, and any standard-artifact waivers.

After Apex returns the items above, UR will supply the competition repository
URL, released tag, required spec or waiver manifest, digest-pinned artifacts,
cosign identities, completed handoff, stage credential, and evidence bundle.

## Already complete on the UR side

- Baseline, evaluator image, scorer, source ledger, significance calculation,
  structural patch validation, Docker isolation, and resource-bomb cleanup.
- Authenticated generate/submit/poll/reveal/leaderboard API and OpenAPI package
  at `https://api.bringyour.com`.
- Durable PostgreSQL ordering plus Redis-list wake-up, cache recovery, the
  three-hour timeout, post-close drain, and one-shot worker exit.
- Manual honesty-review and next-candidate gate, no-winner carry-forward, and
  temporary-clone winner promotion.
- Immutable MinIO evidence implementation and Grafana dashboard, alerts,
  heartbeat, queue/backlog, evaluation progress, and significance signals.
- Go reference adapter/conformance tests, launch preflight, handoff manifest,
  credential rotation/revocation tooling, and operator runbooks.

## Apex contract reference

The ordinary Apex contract is included only to document why the approved
external-evaluator integration needs a custom registry representation. It was
reviewed at public builder commit
`73311da2d56d58c364deb2071291021aceafeaa5`:

- [Competition-builder overview and shipping flow](https://github.com/macrocosm-os/apex-competitions-builder/blob/73311da2d56d58c364deb2071291021aceafeaa5/README.md)
- [`apex.competition.v1` schema](https://github.com/macrocosm-os/apex-competitions-builder/blob/73311da2d56d58c364deb2071291021aceafeaa5/src/apex_sdk/schema/apex.competition.v1.json)
- [Official onboarding manifest](https://github.com/macrocosm-os/apex-competitions-builder/blob/73311da2d56d58c364deb2071291021aceafeaa5/skills/apex-competition-builder/HANDOFF.md)
- [Competition onboarding/activation issue](https://github.com/macrocosm-os/apex-competitions-builder/blob/73311da2d56d58c364deb2071291021aceafeaa5/.github/ISSUE_TEMPLATE/competition-onboarding.yml)

UR technical details and exact route mappings are in `APEX-HANDOFF.md` and
`sn/api/competition.yml`.
