# Warm-reader negative-control qualification: correctness and offline build only

This corrected-release screen passed 52 existing resident/packet race tests
three times: 156 new passing executions. Exact c1 baseline correctness evidence
from logs e43/c70/e226 was reused explicitly; no baseline or performance
measurements were reused or generated. Snapshot-only tests/benchmarks were not
added. The earlier 312-test screen and approximately 17.8%-slower pilot used the
old release and are planning evidence only, not current-release performance.

A second fresh clean ten-repository source was extracted from exact base image
002c3904…; the immutable builder applied and committed only warm-readers.patch
2957ef50…. It produced candidate commit e445ffb75fc3305df6c1ccd5d99e768e45e1142e,
image c675a215…, and binary fc3709dc…. Actual Go 1.27.3 and VCS modified=false
were independently read back. Full identities, test-log digest table and build
metadata are in `summary.json` and `build-metadata.json`.

The archive retains all 12 root-level regular evidence files plus the image
readback companion: 13 selected originals. The inventory binds original paths,
sizes and SHA-256; independent reread and decompressed streaming verification
checked all bytes, rejecting unsafe/link/duplicate/extra/missing entries.
Full source checkouts, service directories/credentials, binary payloads and
caches are explicitly excluded. Source pins, patch, policy bytes, both source
receipts and controller remain reproducible and retained. Two readable copies
match the archived originals.

Cleanup returned zero; original three containers/four networks remained and
the shared slot was free. The expected candidate image cache was not pruned.
No simulator, score, held-out workload, probe or new calibration cohort ran.
This is not an official baseline or current-release worse-performance proof.
