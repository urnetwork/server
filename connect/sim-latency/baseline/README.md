# Sim-latency baseline preservation

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
epoch baseline or final production scale. Capacity bracketing and impairment-off
measurements remain necessary. The subsequent 3,600-provider on point also
completed (9m28s, 2,482 measured rows, two failed rows), with retained SQL
serialization/cancellation events and no direct-controller resource report.
Neither exploratory completion is a formal scoring-gate qualification.

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

[`final-baseline.html`](final-baseline.html) is the human-readable visual report
for this evidence. It is covered by the directory manifest alongside this
README, the index, verifier, and immutable `v1` data.

## Identity

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
