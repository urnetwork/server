# Snapshot mechanism screen — not an end-to-end qualification

The archive preserves 30 selected original files: all 22 root-level regular
files from the successful screen; seven workspace-setup/benchmark companions;
and the module-assertion setup script. `selected-original-inventory.json` binds
each archived path to its original path, size and SHA-256. The single 198,503-byte
gzip chunk contains exactly those 30 regular files, with no links or extras.
Both benchmark logs and the complete benchmark launch receipt are readable
byte-identical copies. Full source repositories, service directories/credentials
and build caches are deliberately excluded. Sources are reproducible from the
retained ten-repository receipts and exact already-versioned patch.

All 516 race-enabled correctness executions passed (52 baseline ×3; 60 candidate
×3; 60 restored ×3). Both guard mutants compiled and failed at their intended
barrier assertions. The two initial setup attempts ran zero tests: an SN fixture
directory outside the selected workspace, then an incorrect module-path
assertion (`github.com/urnetwork/sn` instead of `github.com/urfoundation/sn`).
The latter terminal was observed in the retained tool transcript as
`go: module github.com/urnetwork/sn: not a known dependency`, `cleanup_exit=0`;
no standalone original terminal log existed, so none is invented here. The
positive actual-module control and first setup diagnostic are archived.

Every one of the 20 benchmark observations is retained in `summary.json` and
the raw logs. Warm-hit medians were 390.4 → 103.5 ns/op, both zero allocation.
Publication medians were 465.6 → 18,772 ns/op, with 48 B/1 allocation →
21,904 B/6 allocations. This demonstrates a narrow lookup benefit and substantial
publication cost, not a faster full-workload reference, accepted calibration,
production baseline, or live host/provider readiness.

The exact candidate/fixture was restored; cleanup returned zero, original three
containers/four networks remained, and the canonical slot was free. No scoring,
pilot or second calibration cohort occurred in this screen or preservation.
