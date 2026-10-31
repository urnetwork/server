# Sim-latency baseline preservation

The four-section [current public-data report](final-baseline.html), dated
October 31, uses only the accepted
[reduced-reference public R9/R9 pilot](prelaunch-public-regression-r9-reduced-2026-10-31/README.md).
It shows practical and statistical improvement fringes, the frozen environment,
a timestamped production-status snapshot, and an explicitly hypothetical
six-round TTFB/throughput projection. Its
[derivation](final-baseline-2026-10-31-derivation.json) keeps the raw median
score boundary separate from the conditional mean-based Welch diagnostic.
It is a **draft preview, not the private production epoch baseline or complete
launch acceptance**. Hidden calibration, independent review and new scoped
COMPLIANCE custody remain pending at that report's snapshot. No browser
rendering, benchmark or test was performed to prepare the report.

The previous [historical v1 report](final-baseline-v1-historical.html) is
preserved byte-for-byte at its new path. The
[report-lineage record](REPORT-LINEAGE-2026-10-31.json) records the prior
index/manifest claims and their observed drift; it does not rewrite old
measurements or assert a new qualification. Follow
[the current checkpoint](../launch/current-state.json) for operational state.

The [October 30 public H1 regression pilot](prelaunch-public-regression-r9-2026-10-30/README.md)
completed all nine controls and nine candidates in 10,763 seconds with clean
teardown, but native verification rejected it: the intentional regression was
too aggressive and failed G1 and the lower G2 bounds. The exact generated
baseline, score, rejected proof and host inventories are versioned with their
improvement/acceptance boundaries. This failed attempt is not an official epoch
baseline or permission to start production scoring.

The [October 27 corrected C2 bridge](prelaunch-corrected-c2-bridge-2026-10-27/README.md)
completed fresh R9/R9 in 10,290 seconds with all gates and cleanup. Independent
native verification accepted it as operational compatibility evidence, not
statistical equivalence. Its versioned public subset retains all 18 CSVs and
36 compressed statistics segments; excluded verbose logs remain private and
the complete native replay tree has an exact-version SHA-equal MinIO readback
with COMPLIANCE retention through 2028-01-01. See the
[archive receipt record](../launch/C2-BRIDGE-ARCHIVE-RETENTION-2026-10-27.md);
replica delivery for that full archive is not independently proved. This is not
an official production epoch baseline or completed independent-seed calibration.
The comparison's significance fringe was 12.6246%; the practical takeover
margin remains 26.9%, and both requirements still apply.

The dated four-section [production environment preview](final-preview.html)
summarizes the proposed environment, all eight exploratory frontier points and
the remaining acceptance gates. It is not a final baseline or launch approval;
its live-run progress is explicitly a timestamped snapshot.

The corrected cold R9 check passed in 10,608 seconds, with nine fresh controls,
nine no-op candidates, all G1–G6 and exact cleanup. Its complete 440-file archive
and independent byte/binding verification, first attempt's zero-replicate
caller-admission failure, deterministic Go principal regressions and root-context precheck are retained in
[`prelaunch-cold-r9-2026-10-25/`](prelaunch-cold-r9-2026-10-25/README.md).
No official epoch baseline or live production readiness is claimed from that preparation.

The corrected Go 1.27.3 readiness/snapshot source's first 15,000-provider
impairment-on attempt is preserved in
[`prelaunch-ready-snapshot-frontier-2026-10-25/`](prelaunch-ready-snapshot-frontier-2026-10-25/README.md).
It failed after 18m10s at matchmaking probe 2 and is not a baseline or accepted
scale. Its startup barrier held and no provide-key duplicate errors were found;
the subsequent 7,200-provider impairment-on point also failed its initial
matchmaking probe after 10m6s, with zero measured rows and clean teardown.
Both complete artifact sets are retained. The next 1,800-provider on point
completed in 8m45s with an authenticated completion marker, 2,441 measured rows,
two failed rows and no SQL errors. It is one exploratory point, not the new
epoch baseline or final production scale. The subsequent 3,600-provider on point also
completed (9m28s, 2,482 measured rows, two failed rows), with retained SQL
serialization/cancellation events and no direct-controller resource report.
Neither exploratory completion is a formal scoring-gate qualification.
The same image's complete impairment-off phase is retained alongside those
on-mode trials: all four tested counts completed, with zero measured failures
at 1,800/3,600/7,200 and 22 at 15,000. The largest tested completed on point is
3,600, pending full gate, cold R9 timing and statistical/reference qualification.

The later Go 1.27.3, provide-lock 15,000-provider impairment-on attempt is in
[`prelaunch-provide-frontier-2026-10-24/`](prelaunch-provide-frontier-2026-10-24/README.md).
It failed at matchmaking probe 6 and is retained with full logs and partial
data. It is not a baseline or accepted scale. Subsequent transaction-snapshot
and fleet-readiness fixes are present in the newer source above; the old run
must not be relabeled as evidence from that newer image.

Corrected-image capacity results are in
[`prelaunch-corrected-frontier-2026-10-24/`](prelaunch-corrected-frontier-2026-10-24/README.md).
The 1,800- and 7,200-provider true impairment-off points both completed, each
with 200 warmed clients and zero measured failures. These single trials are
exploratory capacity evidence, not a final scale or an official baseline.
Earlier requested-off results from the defective image are not true off trials.

The earlier four-exchange-instance, fresh-service nine-run evidence is in
[`prelaunch-four-exchange-2026-10-24/`](prelaunch-four-exchange-2026-10-24/README.md).
It includes failure-charged raw-score analysis, all completion markers, full
compressed logs and the original post-processing failure plus its recovery.
It is exploratory, not a frozen production baseline or margin. It must not be
mixed with the one-exchange-instance dataset below or historical `v1` data.

The October 24 prepared-source exploration is separately retained in
[`prelaunch-2026-10-24/`](prelaunch-2026-10-24/README.md). It contains nine
non-official same-seed replicates and their noise diagnostics, not a replacement
for an authenticated official epoch baseline. The `v1` data below remains
unchanged and historical; its source/image qualification does not qualify a
new production release.

This directory is the Git-versioned, host-independent copy of the terminal
sim-latency calibration data. A clone of the repository contains the data
needed to authenticate and revisit the selected scale, baseline stability,
reference-patch screen, and production-staging result; it does not depend on
the ignored `eval-12c` run tree or `/var/lib/urnetwork/competition`.

The `v1` snapshot contains 2,700 retained files and 562,708,406 bytes:

| Dataset | Files | Bytes | Contents |
|---|---:|---:|---|
| `v1/frontier` | 245 | 63,994,913 | Final impairment on/off frontier, including accepted p1800 and rejected p2700 points |
| `v1/same-seed` | 797 | 161,060,266 | Twelve terminal same-seed A/A pairs, workload, analyses, and R=9 / 16.1% selection |
| `v1/independent-references` | 1,253 | 243,652,260 | Five independent seeds with better, no-op, and worse reference evaluations plus terminal reveal and 4/5 decision |
| `v1/production-staging` | 394 | 93,956,683 | Final API/worker attempt with nine baseline and nine no-op candidate replicates, score, accounting, resources, and evidence manifest |
| `v1/lineage` | 11 | 44,284 | Source lock, policy amendments, production-readiness record, terminal audit, and report-delivery evidence |

The data copy is byte-for-byte from the retained terminal trees. Only generated
Python `__pycache__` directories were excluded; they are executable cache, not
measurement evidence. The retained evidence intentionally includes both run
and scorer-input copies when the original evidence manifest binds both. The
small `lineage` directory also brings the terminal audit records that previously
lived only on the finalization-evidence branch into this self-contained package.

This archive has its own minimal [`go.mod`](go.mod) boundary. Some preserved
measurement inputs are Go test source files that were applied to the live
package during calibration; the boundary prevents repository-wide Go commands
from treating those immutable inputs as a standalone package. It does not
change any file under `v1`, and the module file is covered by the manifest.

[`final-baseline-v1-historical.html`](final-baseline-v1-historical.html) is the
historical v1 visual report, not acceptance of the October 28 release. Its
original measurements, charts and supersession banner remain unchanged. It is
covered by the directory manifest alongside the new draft report, derivation,
this README, the index, verifier and immutable `v1` data.

## Historical v1 identity

- Evaluated server source: `46515d82fe98ff666c61b2b5bb1d34a89cf4dad8`
- Evaluator image: `sha256:2abcf145c0f914899debbd2fd52e57a16cf20072165c8d13f04a0ba487198a4c`
- Scale: 1,800 providers, 200 clients, 80 arrivals/minute, quality window 2
- Measurement: 180 seconds, impairment enabled, median of 9 replicates
- Initial takeover margin: 16.1%
  (`candidate <= same-round baseline * 0.839`), plus G1–G6

The live scorer additionally requires a one-sided run-level Welch result at
`p <= 0.05` and records baseline/candidate sample variances with every
evaluation. A significant winner's supported recommendation becomes the next
source epoch's percentage; without a significant winner, that percentage and
the source commits carry forward unchanged.

Machine-readable dataset counts, key artifact hashes, and source lineage are in
[`INDEX.json`](INDEX.json). Every regular file in this directory except the
manifest itself is covered by [`MANIFEST.sha256`](MANIFEST.sha256).

## Verify

From any checkout:

```bash
connect/sim-latency/baseline/verify.sh
```

The verifier first requires exact manifest coverage—no missing or extra files—
and then checks every SHA-256 digest. It does not consult a host run directory.

`v1` is immutable. Preserve later official baselines as a new version instead
of modifying this snapshot. Never put live credentials, private configuration,
or an unrevealed active-round seed in this directory. The independent-seed
material in `v1` belongs to the completed, revealed calibration campaign.
