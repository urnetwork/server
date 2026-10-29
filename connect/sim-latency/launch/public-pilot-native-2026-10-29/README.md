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

These are three of seven required native prerequisite artifacts. Better and
no-op still need their new native images and exact source/readback bindings.
The [publication checkpoint](PUBLICATION-CHECKPOINT.json) records the three
actual Git publications and their Root-installed receipts. Their publication
time is the successful readback observation, not a backdated push timestamp.
All seven publications must precede the native pilot plan and measurements.
No public pilot, new hidden seed, calibration acceptance, or scoring-hold release
is claimed by this checkpoint.
