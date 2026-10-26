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
authenticated first invocation's baseline bytes in that same round. Preserve
their finalized request/provenance hashes before invoking them. No late-bound
field may change the seed, patch, reference order, policy, workload or source.

Authenticate the baseline's round, workload, source/scorer contract, R9 count,
margin and integrity before copying it read-only as `round-baseline.json`.
Require each consuming request's `round_baseline_sha256` and emitted baseline
digest to match the original. Never remeasure, replace or choose a favorable
control for a later reference. Retain all original control artifacts as well
as the shared baseline manifest.

Candidate success is not a condition for choosing the control. A slow,
non-placeable or non-significant first candidate does not authorize discarding
its authenticated control or generating another one. If the first invocation
does not yield a complete authenticated control and the required evidence,
stop for diagnosis under the frozen retry/deadline rules; do not silently
advance to another reference to obtain a replacement control. A recovered
baseline alone does not turn a failed candidate into a completed reference.

The live worker's validation and append-only storage are separate steps:
`controller/competition_controller.go` validates baseline bytes against the
frozen round in `validateRoundBaselineJson`, authenticates returned artifacts
before accepting them in the evaluator path, and freezes supplied baseline
bytes independently of candidate eligibility in the completion transaction.
The offline Stage B wrapper must enforce equivalent provenance and first-control
identity; mere existence of `baseline.json` is insufficient. These source
observations are design constraints, not a qualified offline implementation.

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

## Unqualified binding-core draft

The [Go binding core](held-out-workload-binding.go.txt) and its
[13 deterministic test drafts](held-out-workload-binding-tests.go.txt) are
**uncompiled and unexecuted**. They are preparation only, not an operator
command or authorization to generate real hidden seeds during Stage A.

The core calls the frozen `ControlPlaneWorkloadGenerator.Generate`, checks its
owned temporary file and digest, independently derives the effective seed,
decodes the actual YAML fleet/client/seed and requires byte identity with the
frozen native encoder. Set validation rejects repeated private/effective seeds,
excluded effective seeds, repeated provider digests and incomplete cardinality.
Sensitive seed/effective-seed/YAML fields are excluded from default JSON
serialization. Temporary native files are removed even when bounded reads refuse.

At a future exclusive idle slot, copy both drafts as `_test.go` files into a
separate frozen **c1** simulator checkout/workspace, with actual Go 1.27.3/local,
root and management CPUs 20,22. Explicitly select `^TestHeldOutBinding`, preserve
all failures and require three consecutive complete race passes with zero skips.
These tests use only fixed synthetic seeds and a small native encoder profile;
they are not held-out measurements. Never run them alongside timed evaluation.

This core is not the complete preparation tool. The operator wrapper still must
authenticate and pin the accepted real Stage A analysis/input manifests, use
the frozen five-seed count and policy, acquire fresh cryptographic entropy,
generate all round/job identities and randomized reference orders, bind exact
reference patches/builds, write exclusive private files and a non-leaking public
commitment plan, and enforce same-round baseline late binding. Its own positive,
ordering/refusal, publication and baseline-substitution tests remain required.
No real seeds, provider inputs, finalized requests or accepted margins have
been created by these drafts.

## Unqualified reference-order draft

The [order-selection core](held-out-reference-order.go.txt) and
[six deterministic test drafts](held-out-reference-order-tests.go.txt) are also
uncompiled and unexecuted. They select all six reference permutations using
bounded rejection sampling, refusing entropy exhaustion without a fallback.
The exhaustive accepted-byte test checks equal permutation counts; adjacent
tests cover surplus-byte rejection, the configured rejection bound, missing
sources, invalid settings and independently owned return values.

Qualify both as `_test.go` files in the same exclusive frozen workspace and
actual Go 1.27.3 environment, selecting `^TestHeldOutOrder` for three complete
race passes with zero skips. Preserve all attempts. The eventual operator must
inject `crypto/rand.Reader`, commit the complete five-round order before the
first result and refuse order changes; this helper alone does none of those
publication or request-binding steps. No tests may overlap a timed evaluation.
