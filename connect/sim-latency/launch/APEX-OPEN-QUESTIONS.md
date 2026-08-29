# Sim-latency: open questions for the Apex team

Status: partner decision checklist, 2026-08-28.

This checklist contains the decisions and acceptance records still needed for
an Apex-facing launch. The evaluator, baseline, scoring contract, and
competition API are already complete; this list does not reopen them.

## 1. Season and publication

- [ ] What Apex competition/season identifier should UR record?
- [ ] What UTC time should epoch 1 open?
- [ ] Does Apex accept the variable season calendar? Each epoch admits
  submissions for exactly seven days, but the next epoch opens only after the
  prior FIFO drains, honesty review finishes, a winner or no-winner is
  finalized, and the next rebaseline completes.
- [ ] What date must immutable submission and score evidence be retained
  through (`retain_until`)?
- [ ] How should Apex describe jobs during the embargo? Jobs may finish before
  the epoch, but scores and rankings remain private until every admitted job is
  terminal and honesty review finalizes the epoch.

## 2. Fees, rewards, and participant policy

- [ ] Confirm that Apex collects the fixed **$20 USD** fee exactly once for
  each durable admission before calling the UR submission API.
- [ ] Approve the fee/refund policy for invalid patches, build failures,
  three-hour timeouts, infrastructure retries, and duplicate canonical
  patches that reuse a cached evaluation.
- [ ] Define the reward amount, asset/currency, payout schedule, and payer.
- [ ] Define participant eligibility, geographic/KYC restrictions, and any
  per-participant submission rules. Evaluation admission itself remains
  unbounded.
- [ ] Approve the rules for dishonest submissions, disqualification,
  notification, appeal, and abuse handling. Statistical significance alone
  does not make a submission the winner.
- [ ] Decide whether public leaderboard rows are displayed by patch/job id only
  or enriched by Apex with a public miner identity.

## 3. Adapter and staging acceptance

- [ ] Accept the asynchronous adapter contract: one FIFO evaluator, immediate
  enqueue, up to three hours per evaluation, and an unbounded post-close drain.
- [ ] Name the Apex owner for adapter credentials, token rotation, and emergency
  revocation. UR's operational contact is `support@ur.xyz`.
- [ ] Confirm Apex polling/backoff behavior for the returned immutable
  `status_url`, including HTTP 429 and typed retriable 5xx responses without
  creating a second admission.
- [ ] Record the stage and production adapter image repository digests and
  signing/cosign identities.
- [ ] Complete one end-to-end staging submission and reconcile its Apex
  identity, `job_id`, `round_id`, `patch_sha256`, terminal state, and finalized
  leaderboard row.
- [ ] Activate the Apex private-registry entry and record its identifier.
- [ ] Publish the Apex competition repository/spec release and public
  `HANDOFF.md` required by the registry process.

## 4. Ownership and signed handoff

- [ ] Name the Apex product owner, notification owner, incident contact, and
  legal/abuse owner.
- [ ] Agree on participant messaging for a long grading backlog, no-winner
  epochs, rejected dishonest candidates, delayed next-epoch openings, and
  service incidents.
- [ ] Identify the authorized Macrocosmos and UR signers and the durable
  location for the acceptance record.
- [ ] Sign the final handoff covering the adapter protocol, release identities,
  staging proof, registry activation, economics, participant policy, and named
  owners.

## Contract already frozen

- Six epochs; each admission window is exactly seven days.
- Immediate Redis-list FIFO evaluation with durable PostgreSQL recovery.
- Unbounded paid submissions and a three-hour hard limit per evaluation.
- Results stay embargoed through close, backlog drain, and honesty review.
- A winner must meet the epoch's margin and one-sided Welch significance test,
  pass all scoring gates, and pass manual honesty review.
- If no honest significant candidate remains, the epoch has no winner and the
  existing source commits and threshold carry forward.
- The main API is `api.bringyour.com`; no separate submission service is
  required.
- UR's MinIO evidence-deletion owner and Grafana/on-call incident contact is
  `support@ur.xyz`.

Technical details and exact route mappings are in
`connect/sim-latency/launch/APEX-HANDOFF.md` and `sn/api/competition.yml`.
