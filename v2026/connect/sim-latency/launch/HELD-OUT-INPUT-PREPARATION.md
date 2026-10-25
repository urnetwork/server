# Held-out input preparation

This is an execution checklist for Stage B of the
[frozen statistical protocol](CALIBRATION-PROTOCOL-2026-10-25.md), not a new
statistical rule, accepted calibration or authorization to start measurements.
Stage A must finish and its accepted margin must be frozen first. A faster
reference still needs correctness and independent directional qualification.

## Bind seeds to the workload actually executed

The simulator consumes the seed and fleet in `providers.yml`. The evaluator
validates `round_seed_hex` but does not regenerate that file from the request.
Changing only that request field would not create an independent workload.
The public-seed Stage A fixture intentionally supplies its fixed public
workload; do not copy those provider bytes into five supposedly independent
Stage B rounds.

For each fresh private 32-byte seed, use the frozen source's
`controller.ControlPlaneWorkloadGenerator.Generate` with the frozen evaluation
policy. This is the production generator, including its domain-separated
`workloadSeed` derivation; do not truncate the seed or treat its last bytes as
the simulator's integer seed. Retain the generated exact provider bytes, their
SHA-256 and decoded simulator seed. Independently verify that all five decoded
seeds and provider digests differ and exclude every prior pilot/public/campaign
workload seed. Generate no replacement after seeing results.

The same generated provider bytes must be used by the control and all three
references within one seed's round. Every request binds their digest; every
replicate's scorer manifest must bind the same digest and workload settings.
Different seeds must not share a baseline. Seed commitment/reveal records must
bind the private seed, its round, resulting workload digest and fixed source
and policy identities, not merely a human-readable seed label.

## Commit before observing held-out results

Freeze all five seed commitments, randomized per-seed reference order, round/job
identities, exact no-op/better/worse patch and build identities, source/image,
workload digests, accepted margin and typed policy before the first result.
Keep raw seeds private until the campaign ends. Preserve the generator source,
toolchain identity, independently checked input census and publication receipt.

Each seed's first reference invocation creates its one fresh R9 baseline.
That baseline's content digest cannot be known before measurement. Predeclare
this dependency rather than inventing a digest or claiming every final request
hash existed beforehand: the other two requests may bind only the exact
successful first invocation's baseline bytes in that same round. Preserve
their finalized request/provenance hashes before invoking them. No late-bound
field may change the seed, patch, reference order, policy, workload or source.

Authenticate the baseline's round, workload, source/scorer contract, R9 count,
margin and integrity before copying it read-only as `round-baseline.json`.
Require each consuming request's `round_baseline_sha256` and emitted baseline
digest to match the original. Never remeasure, replace or choose a favorable
control for a later reference. Retain all original control artifacts as well
as the shared baseline manifest.

## Qualification and serial execution

Before using a preparation tool on real held-out inputs, test its positive
path and deterministic refusals for repeated effective seeds, unchanged
provider bytes despite changed request seeds, wrong workload/policy, reordered
references, and baseline substitution or cross-round reuse. A compile-only or
unset-environment pass is insufficient.

Use actual Go 1.27.3 and the canonical exclusive host slot. Do not overlap
builds, tests, pressure probes or other scoring with measured runs. Keep every
outcome, original whole-job deadline and cleanup receipt. Apply the original
four-of-five ordering/no-op acceptance rules without optional stopping,
favorable-draw retries or post-result patch/margin tuning.

No Stage B generator or input set is qualified by this checklist. Its purpose
is to make the next executable preparation bind real independent workloads
and the deliberately shared same-round control correctly.
