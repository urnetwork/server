# Epoch-1 candidate review preflight

This is preparation evidence, not reviewer qualification, a model verdict, or
winner approval. The live frozen qualification run remains unchanged.

## Observed offline layout

The authenticated prepared candidate was inspected using the frozen planner in a
separately identified, non-authorizing diagnostic utility. It made no provider
calls, claimed no review owner, and reserved no storage.

- Prepared input SHA-256: `52932c0d7a30ca9a3354a6e1417612ffc8927d24b86311f6e64942a72d49bb2e`.
- Diagnostic output SHA-256: `0b38f8a588d71293bc22e12e04bd4bf24f1b6276433e6b41cf4a667bbcd18a80`.
- Source artifacts: 256, totaling 8,546,335 bytes; coverage units: 159.
- Default layout: 216 generation calls and 376 tokenizer invocations.
- Prospective spooled local peak: 3,323,767,921 bytes; memory:
  3,153,211,242 bytes; protected storage: 42,842,680,369 bytes.

These are **default-grouping estimates**, not token-admitted candidate groups or
proof that the formal freeze will succeed. The utility does not check the
separate 128-MiB serialized-evidence document limit. Passing its storage and memory
estimate must not be presented as complete admission.

## Remaining preparation checks

1. After the existing qualification releases its exclusive Snow lane, pack the
   complete candidate using authenticated, exact non-inference token counts.
   Do not compete with the running qualification or substitute byte estimates.
2. Measure the resulting exact plan's serialized-evidence limit, storage, memory,
   and planning time before claiming a real candidate review owner.
3. Account for the native freeze callback's repeated capacity computation inside
   the database transaction. An offline diagnostic budget expiry is not itself a
   candidate defect or proof of an actual database timeout.
4. The current native command exposes prepare/freeze/capture/validate, but not a
   candidate token-packing command. Resolve that preparation integration without
   relabeling an auxiliary binary as the qualified native driver.

Do not silently raise evidence limits, extend frozen clocks, truncate source
coverage, reuse another candidate's admissions, or treat the direct source-only
assessment as the required Qwen/Gemma verdict.
