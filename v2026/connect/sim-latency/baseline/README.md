# Sim-latency baseline preservation

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

## Identity

- Evaluated server source: `46515d82fe98ff666c61b2b5bb1d34a89cf4dad8`
- Evaluator image: `sha256:2abcf145c0f914899debbd2fd52e57a16cf20072165c8d13f04a0ba487198a4c`
- Scale: 1,800 providers, 200 clients, 80 arrivals/minute, quality window 2
- Measurement: 180 seconds, impairment enabled, median of 9 replicates
- Takeover margin: 16.1% (`candidate <= same-round baseline * 0.839`), plus G1–G6

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
