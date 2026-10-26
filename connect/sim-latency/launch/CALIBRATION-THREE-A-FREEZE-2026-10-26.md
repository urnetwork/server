# Three-pair reduction and prospective margin freeze

At **2026-10-26T15:01:22Z**, the native reduction under the user-approved
`launch-3a-bridge-3b-2026-10-26-v1` protocol completed successfully. This record
freezes **takeover_margin = 0.269 (26.9%) before any C2 bridge or held-out result**.
It is not production activation or complete C2 qualification.

## Authenticated result

- Native analyzer source: `21f371d180bc62a5f11a94ee23ccb31c23204314`;
  clean Go 1.27.3 binary SHA-256
  `ab51c1fc39be2ec89ec9797c60a809bfa23d3f2c44a7c80db3077853f0e936a2`.
- Approval SHA-256:
  `a794618968d840163952b7b2058100fd87b71ef7ccc44919ba590c5a253f643c`.
- Original five-position plan SHA-256:
  `24f9b6ea9fe6fbfb4381ca29535b81a1cd4966b397c701b04f5b09ac74eb11d8`.
- [Actual input manifest](../baseline/prelaunch-three-pair-analysis-2026-10-26/analysis-inputs-three-a-v1.json):
  `abf5fb1656358e6c1f2f41bc965ae5fd5e8a4faf510c95f3d5c251a10b392c94`.
- [Complete native report](../baseline/prelaunch-three-pair-analysis-2026-10-26/report.json):
  `e78c1a965d485d0a183f95f72797aa75c5c0ba22e52acbcf28749cfe6ecb3adb`.
- Accepted: true; original positions 01/02/03; 54 actual measured executions;
  100,000 deterministic bootstrap draws; placeability **0.99525**;
  six original-image native scorer replays with verified owned-container cleanup.

The selected term was four-times control noise, **0.2687716400543205**,
rounded upward to 0.001. Other terms were no-op noise 0.221446230519281,
positive no-op improvement p95 0.10870218313555347, and the 0.161 floor and
per-cohort production recommendation/required terms. There were no rejection
reasons and no observed no-op takeover at the selected margin. Bootstrap
draws are simulated resamples, not additional measured runs.

For a baseline median B, the practical candidate boundary is **0.731 × B**.
The one-sided Welch p<0.05 condition and all six gates also remain required;
26.9% is not an exemption from the submission-specific significance fringe.
The pair-specific historic p-values/fringes remain in the original records and
are not rewritten by the prospective margin.

## Instrument boundary and remaining acceptance

These observations belong to frozen C1/image `002c3904…`, not C2. The prospective
minimal C2 commit is `a9e8e1a710b24c749e6499a0606938252c83e136`, with unchanged
other nine repository pins. Its image/source manifest must bind the frozen
26.9% and this exact report. Qualify the new image/validator/scripts, then pass
one fresh fixed-margin R9/R9 bridge and publish the approved engineering
compatibility receipt. Do not claim statistical equivalence or relabel old
measurements as C2. Failure does not authorize changing the margin or redrawing.

Then precommit all three independent seeds and reference orders before their
first outcome. All three must rank better < no-op < worse, with complete gates,
integrity, variance, p-values and cleanup records. No failed or unfavorable
outcome may be filtered or replaced. Positions 04/05 of the original plan
remain retained and unstarted. Live storage, containment, review/provider,
runtime acknowledgments, supervisor and monitoring acceptance remain required.

The new input-path defect was reproduced deterministically with the old body:
requests live under each cohort's `artifacts/` leaf, while provenance lives at
the cohort root. The corrected realistic fixture and regression passed with
the eight existing protocol tests and five assembly guards three consecutive
times (14/pass, zero skips). Initial writable-publication and path-preflight
failures are retained alongside the successful assembly; no run was repeated.
