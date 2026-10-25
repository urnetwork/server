# Stage A stopped at its first cohort

The first of five predeclared fresh A/A cohorts failed. This is retained
failure evidence, **not an accepted baseline or a completed calibration**.
The [frozen protocol](../../../launch/CALIBRATION-FREEZE-2026-10-25.md)
was published in server `72046d14581306754529960f37a6c1ba6c3253ae`
before the first observation.

## Terminal outcome

- Start: 2026-10-25T10:17:05Z; end: 2026-10-25T11:44:08Z.
- Evaluator exit: 1; elapsed: 5,223 seconds; three-hour bound unchanged.
- Nine fresh controls completed; no candidate build or candidate run started.
- Baseline scorer rejected control 1: `unexpected_recovery`.
- Terminal cleanup was verified; positions 2–5 remain unstarted.
- No automatic retry, replacement sample, next position or acceptance follows.

The source is server `0d58d0d91b069c62bcfb39cd72166c26925b786a`,
canonical ten-repository lock
`37555f58bbf1235f10600f8c1de7dafeef356985f719f5e029fdb8c2e69c9b4a`,
exact Go 1.27.3 and evaluator image
`sha256:effe9ba5dc9cc337b4f250977c5f921ea42e4a336b30e3a5b54a9fd1bbe7f793`.
The workload remains 3,600 providers, 200 clients, impairment on, public seed
20261024, nine controls and nine planned candidate replicates.

## Evidenced failure and correction boundary

Control 1 logged an unexpected recovery during announce-time peer-profile
lookup. Its database operation stopped with a joined legacy done marker,
socket-write timeout and `context.Canceled`. The database boundary intentionally
retained every original diagnostic cause; the recovery classifier required
every joined cause to be independently benign, so the physical timeout made
the canceled operation unexpected.

The root-cause correction and deterministic regression qualification are
tracked separately. Do not reinterpret these originals with a weakened G5,
mark this position successful, or silently rerun the frozen plan. A corrected
source requires a new immutable release and prospectively published protocol.
Deadline, independent database/application failure, integrity and containment
checks remain required. The preserved measurements cannot qualify that new
release.

## Evidence preservation

`failed-evidence.tar.gz.part-00` is an ordinary Git blob containing the complete
sanitized `failed-evidence/` tree. It is smaller than 32 MiB; extract only into
a fresh private directory. SHA-256:
`e76abdad43d682c066c4b09ba37be2ff53e26f54c6ccbdfda1b1fbe03ac05e0d`.
Readable original companions are retained alongside it:

| File | SHA-256 |
| --- | --- |
| `failure.json` | `de83c05135585dd815c788d5f6468decadb7b9df86fbc170583a53ac9a82ff23` |
| `baseline-scorer.log` | `4a073532b9f45c72665b1311087526b9ea64ba65720722df1954d79ab0e7cc86` |
| `manager-terminal.log` | `b15b67fe287f362f5fcd81509f6bbcf3f0ec78611f4b28265712b0b767c5e6dc` |

Original restricted evidence remains unchanged. Independent Go-only archive
verification passed: **125 original regular files**, one chunk and three
readable companions match every original byte and size. No links, special,
unsafe, duplicate, extra or missing entries were found. The retained
[verification log](archive-verification.log) SHA-256 is
`6bb3602a5c1ee2025239366d3546c9c6dcbf0403631d7354badea30cb3139cea`.
The executed private verifier SHA-256 is
`d6ede1b3f44bfe9d2262afd9d5117110ed3fce4fda7a07d64f8d6f252d9dd24b`.
No `worker-result.json` was produced
because the failure preceded candidate evaluation. This offline run used
explicitly synthetic control-plane identities, not live readiness receipts.
