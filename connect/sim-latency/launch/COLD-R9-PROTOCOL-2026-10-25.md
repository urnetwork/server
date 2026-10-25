# Cold R9 evaluator qualification — predeclared protocol

Status: **completed locally, exit 0, 2026-10-25T06:12:12Z**. All nine controls
and nine candidates, offline build, G1–G6 and exact cleanup passed in 10,608
seconds, 192 seconds inside the cap. The no-op was placeable but not significant
or takeover-eligible. [Complete retained evidence](../baseline/prelaunch-cold-r9-2026-10-25/completed/README.md)
includes independent original-byte/binding verification. The last attempt and
exact request are recorded in [current-state.json](current-state.json).
This checks the leading
frontier candidate before a production scale is frozen. It is not the official
epoch control, a variance estimate, independent-seed qualification, API/MinIO
retention proof or provider-backed honesty-review qualification.

## Fixed inputs

- Actual Go 1.27.3; frozen server
  `0d58d0d91b069c62bcfb39cd72166c26925b786a`; canonical ten-repository lock
  `37555f58bbf1235f10600f8c1de7dafeef356985f719f5e029fdb8c2e69c9b4a`.
- Exact evaluator image ID
  `sha256:effe9ba5dc9cc337b4f250977c5f921ea42e4a336b30e3a5b54a9fd1bbe7f793`;
  simulator/scorer SHA-256
  `4993081b4076693f231ca2e5d3ee4468646c54d2d6cbc89a01fe325fccf57dbc`.
- Candidate: existing comment-only `evaluator/references/2026-10-24/noop.patch`,
  SHA-256 `1076be3ed61df0e14ae90c13bbe4c35442de6371064fd7d78f61ed6834e170f1`.
  The private warm-reader optimization is **not** this control candidate.
- Leading scale: 3,600 providers, 200 clients, 80 arrivals/minute, quality
  window 2, four exchange hosts and four fleet shards, impairment enabled.
- Ramp 60 seconds, synthetic prewarm 13 hours, settle 60 seconds, client warmup
  limit 20 minutes, measured window 180 seconds, request timeout 120 seconds;
  unchanged three-second test/two-second announce and ten-second pipeline values.
- Public exploratory workload seed 20261024. Any offline request nonce or
  synthetic control-plane identity must be explicitly labeled as a fixture;
  do not invent live API, worker, provider, source-readiness or MinIO receipts.

Record the complete typed request, actual source/config/input/controller hashes,
execution identity and UTC start before invoking the trusted evaluator. Any
fixture-only identity is not a production attestation. Only direct synthetic
config/local and vault/local leaves are available to candidate execution.

## Execution and acceptance

1. Hold the shared scoring lock. Verify the exact image, source and mount
   identities, idle evaluation slot, inactive staging supervisor, original
   persistent containers/networks and sufficient evidence capacity. No other
   tests, builds, scoring or resource-pressure work may overlap.
   Preflight and evaluator must inherit the **same actual manager credentials**.
   This deployment's configured manager is UID 0. Run the whole preflight under
   that identity and directly open/hash the private local leaves there; using
   sudo for individual probes cannot validate an unprivileged manager. Preserve
   the zero-replicate admission failure and deterministic principal tests in
   [the cold-admission record](../baseline/prelaunch-cold-r9-2026-10-25/README.md).
2. Use the real trusted worker-to-Compose evaluator, not the short development
   smoke or a loop around the direct frontier controller. Use fresh exact
   temporary source checkouts, offline candidate build and the normal resource,
   accounting, protected-source and artifact-integrity paths.
3. Supply no cached round baseline. Measure **nine fresh control replicates**,
   freeze their authenticated baseline, then build and measure **nine fresh
   comment-only candidate replicates**. Keep the configured provisional 16.1%
   margin for this compatibility/budget check; it is not a fresh noise estimate.
4. Bound the complete cold invocation to the existing three-hour submission
   budget, including ordinary build/scoring/drain overhead. Retain the actual
   wall time and phase times, not a projection from one frontier point. Always
   retain bounded safety cleanup evidence if the execution deadline is hit.
5. Require authenticated completion, unchanged official G1–G6 gate inputs,
   valid final resource/accounting reports and exact cleanup. A no-op should
   be placeable but must not be accepted as a significant takeover. Do not
   filter SQL/recovery logs, waive a gate, discard an inconvenient replicate,
   reduce R9, shorten the measured window or restart for a cleaner result.
6. Preserve every outcome before a new attempt. A failure triggers diagnosis
   and a deterministic regression for a real correctness defect. If evidence
   establishes a scale/budget limit instead, test the next frontier candidate
   under the same full protocol; do not silently declare the smaller point
   qualified from its single exploratory run.

One authenticated absolute control belongs to each real epoch and is shared by
that epoch's jobs. This offline cold check creates no production database row
or eligible submission. Its controls must not be substituted for another round,
seed, source or image. Independent-reference campaigns must declare their own
round/seed identities and control-sharing boundaries before measurement.

## Remaining statistical freeze

After full cold qualification, predeclare and retain the fresh same-seed and
independent-seed reference campaigns, including the actual run/seed counts,
acceptance criteria, pilot/held-out separation and margin-selection algorithm.
Use the approved launch compromises explicitly rather than claiming the original
20-seed/19-of-20 target from fewer observations. No genuinely faster reference
is currently qualified. Freeze scale and initial percentage only from the new
checkpoint's evidence, and bind the final release/host qualification to the
exact resulting source, image and policy. Historical v1 measurements remain
historical.
