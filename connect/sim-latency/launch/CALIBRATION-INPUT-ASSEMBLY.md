# Prospective final input assembly

The [Go discovery helper](calibration-input-assembly.go.txt) passed synthetic
qualification: 14 race tests, three consecutive passes, zero skips. See
[the qualification record](INPUT-TOOL-QUALIFICATION-2026-10-26.md). It has not
assembled the real unfinished campaign or invoked the analyzer. It is a
preparation aid, not a scorer or accepted calibration.
No statistical algorithm, count, gate, bootstrap seed or threshold is changed.

After all five fixed cohorts finish successfully and their original evidence
is preserved, copy the helper into a separate tested analyzer-source checkout
as `calibration_input_assembly_test.go`. Do not modify the operator checkout,
evaluated repositories or installed analyzer binary. Use that checkout's exact
frozen dependency workspace, actual Go 1.27.3/local, management CPUs 20,22 and
the existing canonical scoring slot. Compile/qualify this helper only while
scoring is idle; unset its opt-in variables for ordinary suites.

Explicit opt-ins:

- `SIM_LATENCY_ASSEMBLE_CAMPAIGN_ROOT`: the private root containing the five
  predeclared cohort directories and original `plan.json`;
- `SIM_LATENCY_ASSEMBLE_PLAN_SHA256`:
  `24f9b6ea9fe6fbfb4381ca29535b81a1cd4966b397c701b04f5b09ac74eb11d8`.

Select only `TestCalibrationAssembleFrozenCampaignHelper`. The helper reuses
the existing retained-artifact loader, verifies the fixed plan digest/order,
request/provenance identities, shared source/image/policy, complete R9 counts,
typed successful/placeable worker completion, cleanup and bounded terminal
receipts, then exclusively creates root-only
`analysis-inputs.json`. It does not choose successful positions from a larger
sample, follow request-supplied original paths, rewrite observations, start
Docker or perform bootstrap analysis. A missing, failed or partial position
must prevent normal assembly; retain that failure instead of substituting data.

Qualify the helper with synthetic positive and refusal fixtures before using
real results: incomplete/failing terminal, wrong ordered identity/request hash,
changed shared policy, redirected root, typed failure or nonplaceable score
despite process exit 0, missing replicate and attempted output overwrite.
This synthetic guard qualification passed with exact source and retained logs;
an unset-environment no-op or compile pass alone would be insufficient.

A [Go qualification source](calibration-input-assembly-qualification.go.txt)
now supplies a complete five-position synthetic discovery layout and isolated
subprocess refusals for failed/partial/over-budget terminals, changed job or
request identity, redirected/aliased roots, changed shared policy, missing
replicates, typed/misspelled worker errors, nonplaceable scores, incomplete
cleanup, reordered positions, changed release headers and output overwrite.
Copy it beside the assembly helper as
`calibration_input_assembly_qualification_test.go` in the isolated analyzer
checkout. Set `SIM_LATENCY_QUALIFY_ASSEMBLY=1` and explicitly run
`^TestAssemblyQualification` as root with actual Go 1.27.3, management affinity,
and the canonical exclusive idle slot. It forks only its own test executable
to observe expected helper failures; no scoring or Docker is invoked.

Its small placeholder artifacts test discovery/assembly guards only; they
intentionally cannot stand in for
authenticated real scorer evidence. The separately qualified native loader
and unchanged analyzer must still authenticate and replay the complete real
campaign. Do not call an opt-in skip, a compile pass, or these synthetic
assembly checks an accepted calibration.

Pin and preserve the generated manifest's digest, then release the assembly
lock before invoking the unchanged installed analyzer: it independently obtains
that same lock through its own guard. Use `calibrate`, `--calibration-inputs`,
`--calibration-inputs-sha256`, `--calibration-lock` and
`--calibration-cpuset=20,22`. Keep full stdout/stderr as new artifacts. The
analyzer authenticates originals, replays the exact pristine scorer and runs
the predeclared reduction. Require actual `accepted: true`, complete replay
receipts and all margin terms—not simply process exit 0—before freezing the
initial margin for the independently committed reference screen.
