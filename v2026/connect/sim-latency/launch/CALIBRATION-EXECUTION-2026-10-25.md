# Calibration analyzer execution handoff

**Stage A stopped at its first cohort; it is not accepted.** The first cohort
ended at 2026-10-25T11:44:08Z with exit 1 after 5,223 seconds. All nine controls
completed, but the baseline scorer rejected control 1 for `unexpected_recovery`.
No candidate build, candidate measurement or later cohort started. Terminal
cleanup was verified. The original failed evidence and all five frozen positions
must be retained; do not retry or substitute a favorable draw. Diagnosis of the
canceled database operation's joined error is in progress; no gate is relaxed.

Its first cohort
started at 2026-10-25T10:17:05Z, after
[protocol publication](CALIBRATION-FREEZE-2026-10-25.md) in commit `72046d145`.
The analyzer at `30f340a27` passed all qualification steps below, including
three complete 501-pass suites; do not rerun those steps while scoring is live.
This is not an accepted calibration or production activation. The warm-reader
pilot completed with cleanup and failed its directional screen; see
[its preserved outcome](../baseline/prelaunch-warm-reader-pilot-2026-10-25/completed/README.md).
The first analyzer snapshot passed 52 named race-enabled tests, six behavioral
pre-fix/fixed regressions and real frozen-image control/candidate replay.
The latest merged-main draft passed 55 named tests, all nine pre-fix/fixed
regressions and another real-image replay; see
[the exact smoke receipts](CALIBRATION-QUALIFICATION-2026-10-25.md).
The exact completed qualification and full-suite receipts are recorded there.
For the current cohort and next action, check
[current-state.json](current-state.json), then verify the actual live handle.
Do not overlap tests/builds/replay with scoring. Actual Go 1.27.3 is mandatory;
use Sol 6.1 low for independent test execution and Sol 6.1 max for diagnosis,
code evaluation and merges. The sequence below is retained as the completed
qualification procedure, not an instruction to overlap another validation with
the live campaign. Formal results and later launch gates still need acceptance.

## Completed qualification sequence

1. Inspect the same live pilot handle. At terminal verify all artifacts, exact
   cleanup, elapsed time and the predeclared directional decision. Preserve all
   outcomes, whether the patch looks faster or not. Do not retry for a better
   draw or promote its source.
2. In a fresh private validation checkout, copy the explicit pending analyzer
   files and `main.go`; use the established exact dependency workspace, actual
   Go 1.27.3/local, management affinity and synthetic-local test services. Keep
   full logs private and report command, source/input hashes, exit, count and
   failure excerpts. No live main DB/Redis/vault is test infrastructure.
3. Run all new `TestCalibration*` cases with the race detector. Default execution
   of `TestCalibrationRetainedArtifactQualificationHelper` is only a no-op
   subprocess helper, **not** genuine image qualification or another regression.
4. Demonstrate the null-value and cleanup root-cause regressions against the
   preserved original failing function bodies, then the corrected bodies.
   Snapshots and digests are in current-state.json. APIs evolved during the
   draft, so transplant the relevant original **function bodies** into a private
   compatible checkout using a Go AST helper; do not replace whole files and
   mistake a compile error for the root-cause failure. For nulls replace
   `validateCalibrationRequest`; for cleanup replace `replayCalibrationScore`.
   Run the corresponding exact regression names, retain expected behavioral
   failures, restore fixed sources and prove success. Do not mutate the operator
   checkout or any evaluated-source branch for this exercise.
   Also demonstrate that a declared failed cohort cannot bypass validation of
   another cohort or inflate `validated_observed_replicates` with partial failed
   records: the two corresponding `TestCalibrationAnalysis*` regressions must
   fail against the original early-return/counting logic and pass after fixing
   it. These are draft-analyzer defects, not deployed evaluator incidents.
   For the private-umask regression, transplant the preserved original
   `copyCalibrationReplayCorpus` body from the replay snapshot. The isolated
   subprocess test must detect unreadable copied directory permissions under
   umask 0077, while the corrected path must keep the private parent 0700,
   copied directories 0755 and files 0444. The genuine image replay remains
   necessary to prove remapped-scorer operation beyond permission assertions.
   Also prove control-identity coverage: the old analysis body must fail the
   repeated-control and control/candidate reuse regressions, and the old cohort
   validator must fail the missing/empty control-identity regression. Restore
   fixed bodies and prove all pass. Original sidecar identities, not new round
   labels, establish that controls are fresh across cohorts.
5. Explicitly qualify retained-cold replay with the compiled test binary, root
   principal, actual management CPU set and the existing singleton lock. Run
   **only** the helper below, with a bounded test deadline and no outer process
   already holding the same lock. The helper acquires it itself:

   ```sh
   sudo -n env \
     SIM_LATENCY_CALIBRATION_QUALIFY_ROOT=/absolute/private/completed-cold-attempt \
     SIM_LATENCY_CALIBRATION_QUALIFY_LOCK=/absolute/existing/scoring.lock \
     SIM_LATENCY_CALIBRATION_QUALIFY_CPUSET=APPROVED_MANAGEMENT_CPUSET \
     /absolute/private/sim-latency.test \
       -test.v -test.timeout=10m \
       -test.run='^TestCalibrationRetainedArtifactQualificationHelper$'
   ```

   Paths/CPU set above are placeholders, not deployed values. Use the exact
   original completed cold root recorded in current-state.json. The helper
   reauthenticates source/workload/policy, all evidence bindings and the worker
   record, then runs the original immutable image for both baseline rebuilding
   and candidate scoring. Both outputs must match retained bytes exactly. It
   must remove its owned replay containers and temporary corpora, or fail and
   retain context for reconciliation. Record original-only resource census.
   This one-cohort qualification does **not** call the formal bootstrap or
   qualify a release margin. Unset all three variables afterward.
6. After smoke/prefix/real-replay success, commit, pull/merge and push the phase
   before larger validation. Then run the complete `tests.sh -count=1` three
   consecutive times at the exact recorded committed revision, with synthetic
   local services and verified cleanup. Fix root causes with deterministic
   regressions; never substitute three cached or partial results.
7. Build and bind the exact Go 1.27.3 analyzer executable. Finalize/publish the
   [draft statistical protocol](CALIBRATION-PROTOCOL-2026-10-25.md), recording
   source/executable digests, settings and predeclared cohort IDs before the
   first formal observation. Only then start the serial campaign.

## Analyzer contract

The file-only operator command is:

```sh
sudo -n /absolute/pinned/sim-latency calibrate \
  --calibration-inputs=/absolute/private/input.json \
  --calibration-inputs-sha256=EXACT_INPUT_MANIFEST_SHA256 \
  --calibration-lock=/absolute/existing/scoring.lock \
  --calibration-cpuset=APPROVED_MANAGEMENT_CPUSET
```

It requires an existing, canonical regular lock and refuses an occupied slot;
it never creates a lookalike lock. Output is JSON on stdout only. Invalid input,
replay or cleanup exits nonzero and cannot produce acceptance. A valid but
unqualified statistical result has `accepted: false`; inspect that field rather
than equating process exit 0 with release readiness. Capture output in a new
private artifact; never redirect over original evidence.

The input schema is `calibrationFileManifest` in `calibration_files.go`. It binds
source, immutable image, workload/patch/policy digests, seed nonce, source epoch
and exactly five ordered positions. Each `calibrationFileCohort` binds the whole
original private artifact directory, job/round IDs, request, baseline, score,
worker result, evidence manifest and the manager terminal receipt. The terminal
log remains in the attempt parent, outside the immutable `artifacts` tree.

`Inputs` and `BaselineInputs` are distinct `ScoreInputs` bundles. JSON keys for
their existing shared type are `Runs`, `Stderr`, `Accounting`, `Samples`,
`ResourceReports`, `Markers`, and `BaselineManifest`; values are paths relative
to the original artifact directory. Both baselines refer to
`evidence/scorer-input/baseline.json`. All run/sample files must be evidence-bound.
The actual replay preserves the original `/artifacts` namespace using a fresh
private, read-only data copy, never edited original sidecars or a relaxed path
guard. Only fixed `score-baseline` and `score` commands run; no candidate code,
main credentials, source checkout or Docker socket enters a replay container.

The report separates `validated_observed_replicates` from `bootstrap_draws` and
retains margin terms, gate rates and replay observation identities. A replay
inspection fingerprint is not a provider/host attestation. Failed cleanup
retains its private corpus path; reconcile the exact owned container before
resuming work. Do not globally prune Docker or delete a broad temporary root.

No formal cohort, held-out result, final margin, genuine model qualification or
production activation is claimed by this prepared procedure.
