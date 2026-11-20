# Recovery qualification: session-memory diagnostic

This concerns an isolated recovery-tool test, not a submission evaluation or
the production PostgreSQL service. No scoring policy, baseline, source checkpoint,
job deadline, production database setting, or recovery grant changed.

The instrumented reproduction retained the full 221-prefix test and every
fault/restore check, with PostgreSQL capped at 1 GiB and no extra swap. It failed
at 16:50:12 UTC after 1,242.356 seconds: PostgreSQL killed backend 563, and the
test reported `conn closed`. The guard and sampler joined successfully, source
and dependency hashes were unchanged, and owned cleanup succeeded.

The last complete event was number 14,563: before batch 20 at schema head 928,
123,605 statement bytes, SHA-256
`c903f370fcdb78c6e4f0ed264f513e37a7442356b0292b939145a61ff5208f29`.
There was no matching after-batch event. At the last pre-failure sample,
cgroup memory was 1,072,689,152 bytes, including 892,121,088 anonymous bytes.
Several reused backend processes had resident PSS around 160–173 MiB while
sampled live PostgreSQL contexts were about 21 MB. These are different accounting
domains: do not add file and shmem, sum shared RSS, or infer an allocator cause
solely from these figures. The underlying allocation mechanism remains unproven.

## Independent outcomes

| Stage | Outcome | Meaning |
| --- | --- | --- |
| Native Go test | Failed; isolated PostgreSQL OOM | No native qualification credit |
| Original per-record jq extraction | Partial, with parse errors despite exit zero | Unqualified view; retained unchanged |
| Qualified streaming reassembly | Complete projection; native outcome still `fail` | Reliable diagnostics, not a repaired test |

Go test-json splits long log lines across `Output` records. The reassembler in
`tools/competition-census-memory-projector` joins ordered bytes for the exact
test/package before parsing complete lines. Its twelve deterministic tests
passed three ordinary and three race repetitions at the published source path:
36 + 36 passes, zero failures/skips. They cover the original 1-KiB split,
ownership, malformed/truncated JSON, bounds, output errors and file custody.

The actual postprocessing run exited zero after 539 ms, with a 2.4 MiB peak:

- Input: 8,715,321 bytes, SHA-256 `78f547ddabdba1b8c82f80cbd26deff22579fe03fb98e79b7fc5444c37531d1d`.
- Events: 14,563 contiguous; 188 before-apply and 187 after-census markers.
- Reassembled output SHA-256: `7878c4dcf160261ab84e94c4ef6b9005d2f70670f7fc1a9fcbae929e9f483ce7`.
- Receipt SHA-256: `2c57113dcdd5bfda4aaedd3b4d90967142e4306e9f0670af8a8f8d26e784ca35`.
- Native `test_outcome`: `fail`, preserved independently of parsing success.

## Retention and next control

Protected durable evidence is retained in
`/var/lib/urnetwork/bounded-census-session-memory-diagnostic-7880-20261112.4LfIg9fa`;
its manifest SHA-256 is
`abd96924f360eddddedb94f623e4134875d1b8865465b83579c71b2c376c9d56`.
The directory is root-only and files read-only. It contains raw/reassembled logs,
memory/PSS evidence, receipts, source manifests and sanitized container metadata;
it does not copy credentials, caches or live services.

Source inspection confirms the production monitor uses a fresh direct psql
connection per census, while the ordinary test adapter reused pooled backends.
A minimal fixture-lifetime parity candidate is being prepared. It must preserve
the caller-owned fault transaction, original errors, full test scope and 1 GiB
cap, then pass independent lifecycle controls and the complete native test.
It is not yet a qualified fix. Recovery qualification remains 203/260, and failed
submissions have not been requeued by this diagnostic or parser correction.
