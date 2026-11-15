# Completed qualification evidence retention

The [receipt summary](receipt-summary.json) identifies a 452,073,472-byte private
MinIO archive. Its exact source version was read back and matched the complete
SHA-256. Retention is COMPLIANCE through January 1, 2028. Configured replication
was verified; delivery of this archive to a replica and off-site backup are not
proved. No originals were deleted.

The archive contains completed Qwen/Gemma six-case qualification and H1 worse
112-by-three correctness evidence. It does not contain later better/no-op runs,
pending operator qualification, calibration measurements, or Docker image
payloads. Retention is not calibration acceptance or permission to enable scoring.

[The source package](archive-go-source.tar.gz) preserves the exact Go-only
archive adapter and tests, plus its standalone module files. It passed three
independent 29-test race processes. Its seven unchanged core files reuse the
earlier reviewed retention implementation; only the closed context and metadata
identities changed, with two additional deterministic policy tests. Native
[build information](native.buildinfo.txt) records the actual dependency graph.

For restoration, use the existing protected `server/blob` store to read the
recorded key **and exact version**. Stream-check the archive length and SHA-256,
then verify its internal inventory and regular members in a fresh private
directory before trusting any restored executable. Do not fall back to the
current object version or delete source evidence after a successful readback.
