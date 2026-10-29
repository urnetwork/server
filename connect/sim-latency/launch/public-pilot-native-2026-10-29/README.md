# Native public-pilot prerequisites

The [completed operator checkpoint](COMPLETED-675-CHECKPOINT.json) records three
independent 225-test race processes: 675 top-level passes, no failures or skips.
All three used the same executable and passed source/dependency integrity checks.
The [native qualification](tool-qualification.json) is byte-identical to the
Root-installed artifact used by the prospective calibration operator.

The [source manifest](ADAPTER-SOURCE-MANIFEST.json) authenticates the 19 Go-text
fragments in `adapter-source/`. This bounded snapshot is not the complete
241-file compilation workspace; the retained manifests and terminal receipt
record that larger closure.

The worse reference's [build](worse-build-qualification.json) and
[correctness](worse-correctness-qualification.json) records bind the actual
`647922…` image and its tested source. They establish build/readback and
correctness, not measured regression direction.

All seven native prerequisite artifacts are now present: the tool and the
build/correctness pair for each of worse, better and no-op. The
[better binding](better-SOURCE-AND-EXECUTION-BINDING.json) and
[no-op binding](no-op-SOURCE-AND-EXECUTION-BINDING.json) connect the actual new
images to their independently tested product bytes (303 and 279 runtime passes).
The [supplement record](readback-supplement-summary.json) preserves the initial
unsupported-identity invocation failure and the separately tested, successful
default-entrypoint readback. Neither image was rebuilt for that correction.

The historical [publication checkpoint](PUBLICATION-CHECKPOINT.json) records
the first three actual Git publications and their Root-installed receipts.
The remaining four require their own subsequent publication proof; that first
proof is not overwritten or relabeled. Every publication time is a successful
readback observation, not a backdated push timestamp. All seven publications
must precede the native pilot plan and measurements.
No public pilot, new hidden seed, calibration acceptance, or scoring-hold release
is claimed by this checkpoint.
