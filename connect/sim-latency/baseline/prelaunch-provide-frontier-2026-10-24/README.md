# Go 1.27.3 provide-lock frontier — retained failure

This is exploratory public-seed evidence, **not an official baseline, accepted
scale, variance estimate or significance threshold**. It predates the subsequent
read-committed and shard-construction-readiness corrections. Preserve it; do
not silently replace it with a successful attempt on another source.

## Identity and workload

- Frozen server: `6261c15454adec5a00680e4c3332f628a0aed1e7`.
- Evaluator image ID:
  `sha256:d526a183a2ecf96793f736e40ef52eadf1bf0ef212efc1ea66c1d8f296f951bf`.
- Simulator/scorer SHA-256:
  `95aa128f56c311c27030eb7ed2abddc9f23151417d07c3003d6579a8724872f5`.
- Go 1.27.3; 15,000 providers, 200 clients, 80 arrivals/minute, quality window 2;
  four exchange hosts and four fleet shards; impairment enabled, public seed
  20261024. Actual parent/child command arguments were inspected.
- Fresh image-extracted source, fresh PostgreSQL/Redis and trusted migrations.
  Only direct synthetic local configuration leaves were mounted; no main/all
  credentials or configuration were exposed. Evaluation CPU set
  `0,2,4,6,8,10,12,14,16,18`; runner limit 72 GiB.

## Terminal result

Exit 1 after 945 seconds. All 200 clients established, but matchmaking probe 6
exhausted its HTTP attempts. Shard 1 required KILL after its TERM deadline.
The sidecar is explicitly `incomplete`: 592 rows in the measured window, 103
failures. There is no completion marker or final resource report. Incomplete
samples must not be scored or substituted for an epoch baseline.

The service log contains 449 provide-key uniqueness errors. A later deterministic
regression established that the advisory-lock backport also needed read-committed
isolation; retries had hidden those errors from the earlier final-state test.
Parent prewarm also finished before the first fleet ticks. The later readiness
handshake orders construction before ramp/prewarm. Neither finding is represented
as proof of what caused probe 6's HTTP failure.

All owned containers/networks were removed; the original persistent services
and BuildKit remained. There was no external retry or additional point.
See [the investigation](../../launch/STARTUP-AND-SNAPSHOT-2026-10-24.md)
for the prefix/fixed regression evidence and replacement-release gates.

## Preserved bytes

`failed-p15000-on/` contains the exact final sidecar, CSV, Docker ID-map
receipt, generated public-seed workload and full runner/service logs. Gzip
uses deterministic `-n` headers; decompression reproduces original bytes.
`controller/` retains the scripts exactly as executed, including their original
private paths for provenance, not as a portable deployment command.
`image-record.json` binds the built base and source-file identities.

The enclosing `baseline/MANIFEST.sha256` authenticates every retained file.
No historical `v1` evidence or earlier frontier bytes are changed.
