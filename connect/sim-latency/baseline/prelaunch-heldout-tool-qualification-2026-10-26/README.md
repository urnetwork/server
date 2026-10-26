# Original held-out tool unit qualification

Independent Sol 6.1 low execution passed the same race-enabled binary three
consecutive times: **62 tests/pass, zero skips**, in 572/573/572 seconds. The
exact source/dependency receipts, complete stdout/stderr, launch scripts and
both the failed initial setup and successful corrected setup are retained in
`original-62/`. The binary remains in the restricted execution directory; its
digest is recorded in `INDEX.json` rather than versioning a large executable.

The initial setup failed before compilation/tests because the offline cache
path was wrong and a full module-list operation attempted unavailable lookups.
Its log is preserved; no failed measurement was replaced. The corrected setup
used the explicit populated caches and exact repository receipts. Original
containers/networks were restored and the shared scoring lock was released.

This qualifies only the original fourteen-file, five-seed tooling snapshot at
the recorded source, **not** the later three-seed/bridge amendment. It contains
synthetic deterministic tests, not actual held-out measurements, calibration
acceptance, provider review, a production baseline or live readiness. Later
tool changes require separate qualification.
