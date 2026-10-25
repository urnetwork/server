# Corrected-release calibration: prospective restart

Status at prospective publication: preparation only; no new campaign measurements had started. Opening
remains **2026-10-28T00:00:00Z**. Main maintenance still prohibits production
activation and main database/service mutations.

Current progress is recorded separately in [the acceptance matrix](LAUNCH-ACCEPTANCE.md)
and [the preserved first cohort](../baseline/prelaunch-corrected-same-seed-r9-2026-10-25/completed/cohort-01/README.md).
The prospective sampling and acceptance rules below remain unchanged.

## Why this is a new campaign

The previous Stage A stopped after its first cohort failed. Its nine control
runs, failure and complete archive remain historical evidence, not accepted
calibration. Positions 2–5 will not run. Neither those controls nor the earlier
cold check or unsuccessful warm-reader pilot enters this replacement sample.

The correction preserves the stability gate and changes only classification
of a fully inspected cancellation-owned socket-stop error. See
[the root-cause qualification](DB-CANCELLATION-RECOVERY-2026-10-25.md).
The independently demonstrated host-gateway containment defect is corrected
in a new trusted script bundle, separately from evaluated application code;
see [network qualification](NETWORK-BOUNDARY-2026-10-25.md).

## Replacement release bindings

- Evaluated server: `c1f9dda603d6279666fca5d92b99c5663306c5f7`.
- Canonical ten-repository source digest:
  `5e00bf5ee7b4708be94ac1ebc6daf5c024f4afe467ca30c35024f65f96b6c90e`.
  The other nine repository pins are unchanged; operator-proxy is excluded.
- Actual compiler/runtime: exactly **Go 1.27.3**.
- Evaluator image:
  `sha256:002c39040ce23dee27b171c7e657f749c6a6d05b1e9b97d90ac87336470dbaf3`.
- Installed trusted scripts:
  `/usr/local/libexec/urnetwork/competition-737dd2371-go1273-isolated-v1/container/`.
  Evaluator SHA-256:
  `263ee68ab058889c22de51d47d8779d95fd219e92ea7642430f61740d15d8d5e`.
  Network helper and Compose hashes are recorded in the network qualification.
- Qualified analyzer remains unchanged:
  `/usr/local/libexec/urnetwork/competition-calibration-30f340a27-ac60524d/sim-latency`,
  SHA-256 `ac60524d915cf8a0a74e1e61d74d4668b04b722b103a7ed0e2b9b2953eb2d52f`.

## Prospective sampling and acceptance

All statistical rules, scale, R9 counts, timers, containment/resource budgets,
bootstrap seeds/draw count, margin selection and held-out acceptance rules in
[the statistical protocol](CALIBRATION-PROTOCOL-2026-10-25.md) are unchanged.
Only the explicitly identified source/image/script release and fresh campaign
identities/output paths replace its obsolete release bindings. The policy's
`simulator_sha256` and `scorer_sha256` must both bind the new pristine binary
`6c8587be62b8292c5d4ec27a8b5c3353e84a8121a6054ba3755d022d2372e9a3`.
Consequently its canonical policy hash changes; that identity-only change is
not a change to workload, timing, statistics or gate semantics. The generator
checks every other policy field against the retained template before writing.

Stage A collects **five fresh ordered A/A cohorts**, each nine new controls and
nine new comment-only candidates: 45 controls plus 45 candidates. Public seed
20261024, 3,600 providers, 200 clients, impairment enabled, full measured windows,
three-hour whole-job limits and G1–G6 remain unchanged. There is no cross-cohort
baseline reuse and no optional stopping or replacement of unfavorable outcomes.

The first cohort also supplies the corrected release's full-window cold
functional qualification. This dual use is declared before observing results:
it is **one cohort**, not two independent observations, and all its measurements
remain in the five-cohort calibration. It must meet both functional and
statistical campaign requirements. No additional discarded cold sample is
required merely to repeat the same complete paired workload.

The first cohort cannot start until three consecutive complete script/control
test suites pass, the installed bundle matches its recorded bytes, all five
requests/provenance records and the runner are frozen and published, the
canonical slot is free and original-only resource inventory is verified.
Only direct synthetic config/local and vault/local leaves are mounted.

Every failure is retained and stops this campaign. Do not restart the failed
old campaign, hide an instrument change as a retry, or substitute favorable
draws. Synthetic API/worker/hardware labels remain explicitly offline fixtures,
not live readiness receipts.

The resulting margin must be selected by the unchanged qualified analyzer and
frozen before the five-seed held-out reference screen. The failed warm-reader
pilot is not a faster reference. Any new reference must pass its correctness
screen and independent directional pilot before held-out patch/seed/order
commitments. The accepted compromise remains four correctly ordered seeds out
of five, not a claim of nineteen out of twenty.

## Outstanding publication and launch gates

The new five-position plan and runner hashes are recorded in the
[replacement freeze](CALIBRATION-RESTART-FREEZE-2026-10-25.md). Do not execute
until that record and its preserved inputs are published to origin.
Full-window qualification, accepted calibration, exact-image host rebaseline,
live API/worker acknowledgments, review-provider qualification, artifact-storage
durability and monitoring delivery remain launch gates. Passing local tests or
installing the bundle does not imply those gates passed.
