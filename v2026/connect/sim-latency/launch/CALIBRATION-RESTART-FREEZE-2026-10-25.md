# Corrected-release Stage A freeze

All five fresh inputs are generated and independently verified before any
measurement. This freeze applies to the
[replacement protocol](CALIBRATION-RESTART-2026-10-25.md), not the stopped old
campaign and not production activation. Execute only after this record and its
complete baseline-preserved inputs are committed, pulled and pushed.

## Immutable input and executable identities

- [Ordered five-position plan](../baseline/prelaunch-corrected-same-seed-r9-2026-10-25/predeclared/plan.json):
  `24f9b6ea9fe6fbfb4381ca29535b81a1cd4966b397c701b04f5b09ac74eb11d8`.
- [Typed canonical evaluation policy](../baseline/prelaunch-corrected-same-seed-r9-2026-10-25/predeclared/evaluation-policy-canonical.json):
  `d6f22dc1b4b420b4b6724183c7f93b4b92f42c787d754b4a677fd3599f92f2a7`.
  Only simulator/scorer identity fields differ from the old policy; both now
  bind pristine binary `6c8587be62b8292c5d4ec27a8b5c3353e84a8121a6054ba3755d022d2372e9a3`.
- New root-owned 0555 runner:
  `/usr/local/libexec/urnetwork/competition-calibration-c1f9dda60-isolated-v1/cohort-runner.sh`;
  `76ffb7087ba19097e5ec0291c3d2780cceb9404fb51ba44efe8ad8462f102304`.
- Trusted evaluator release, image, evaluated source and unchanged analyzer are
  bound in the replacement protocol. The script bundle includes the new
  network helper and isolated-gateway Compose policy; the runner checks all
  three exact file hashes before invoking it.

Input root:
`/home/by/urnetwork/temp/canceled-stop-calibration-20261025.0G7LKkuh`.
It and cohort directories are root-owned 0700; generated input files are 0400.
All requests, provider/no-op bytes and provenance records are also preserved
under the new baseline dataset, without copying config/vault contents.

| Position | Job ID | Round ID |
| --- | --- | --- |
| 1 | `eb5458d2-27bc-4c10-9233-b7c8be1692b5` | `c8193705-e5e7-415f-988b-bc0c9d2978ac` |
| 2 | `8cd8edae-c30c-407f-b611-bda14dfe2764` | `9dfa6395-6c81-4a74-94bc-d8538fabb169` |
| 3 | `0f2f95e7-abd1-4d30-9f3e-133eab0e5781` | `d3ee1e9e-fd82-46ce-83f3-8d61acb007ee` |
| 4 | `586107b8-a7bd-407d-b545-3da1615d6428` | `030cf71b-63dc-4cb3-8a34-eed819af4907` |
| 5 | `d4df22e9-5ed5-4c08-8470-02eedeb9e2fe` | `22b1a1d8-fa93-40f9-a895-351cf15a005e` |

## Qualification and stop rules

The exact control source passed three complete 514-test suites with cleanup;
the corrected evaluated source previously passed three complete 340-test suites.
The [network qualification](NETWORK-BOUNDARY-2026-10-25.md) links the actual
isolated-container checks and updated Docker lifecycle smoke.

Independent Go preparation verification checked all ten UUIDs unique, every
request/provenance digest, attempt 1, R9, provider/no-op identities, new source
and image, new pristine binary bindings, every unchanged policy field, no cached
baseline, root-only directory/file ownership and no started/result files.
Verification log SHA-256:
`9a7f122d3f8869c92f81aa1b716a388bba3e1e7f98520c0ede59af0a18ed7173`.
Generation log SHA-256:
`34a0620ad62c7de28a64cb0edb2db801a936fc77099b25474f1456ea72d13f50`.
These are input checks, not measurements.

Independent preservation verification passed the baseline manifest's exact
coverage and all SHA checks for **3,029 files**, exit 0; log SHA-256
`219b0b31c22b784fc4325abe05eb964ef53a45894f481f4726a4a5cf488b22a8`.
All 26 preserved payloads matched their originals byte-for-byte, and the
installed runner matched the staged copy. Installed evaluator/helper/Compose
matched the clean tested source. Evidence root:
`temp/corrected-predeclared-verification-20261025.1KwflAFo/`;
comparison log SHA-256
`9ae72b5d88adac6e269af07cb0b668dd9d1af7775b40744dc9e46a9e45429475`.
Original containers/networks were unchanged and the scoring slot was free.

Run positions in order, once, under the existing canonical slot, root principal
and management CPUs 20,22. Each gets its own three-hour whole-invocation limit
and bounded cleanup; use an outer 10,860-second watchdog with 15-second kill
grace. No other test, build, simulation or pressure workload may overlap.
Retain every outcome. Execution, gate, integrity, timeout or cleanup failure
stops the campaign; do not replace or silently retry a position. A complete,
placeable no-op's unfavorable direction or low p-value alone is not a reason
to discard it: the unchanged analyzer chooses the margin from all five pairs.
Do not skip the independent-seed screen or call this a production baseline.

The first position also supplies full-window functional qualification, as
declared before observation; it is still only one aggregate cohort. After
all five complete, authenticate them in this order, run the unchanged qualified
analyzer, require actual acceptance and freeze the resulting margin before the
separately precommitted held-out reference screen.

Main maintenance and live/provider/artifact/host-readiness gates remain separate
holds on activation. No database mutation or main restart is authorized here.
