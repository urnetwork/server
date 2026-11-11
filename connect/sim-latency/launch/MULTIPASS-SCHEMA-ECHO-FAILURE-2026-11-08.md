# Corrected qualification: provider schema echo rejected

The approved corrected qualification is terminal, not running or qualified.
The original attempt and its evidence remain preserved; no successor draw or
deadline renewal has been authorized or started.

- Unit: `urnetwork-review-qg-controls-7a7b2f8dd-20261108-v1.service`.
- Owner: `ba4644184f5528c543126c4779d7cdbc7b06b4dd6af822d063ab039676c562f0`.
- Native test failed after 6852.53 seconds at invocation 183, `qwen-scan-0001`.
- All 183 invocation commit records remain retained.
- The wrapper recorded broker exit 1 and verified cleanup.
- The provider transport returned HTTP 200 and a `response.completed` event:
  model `qwen3.8-27b`, status `completed`, no incomplete details or provider error,
  24,816 input tokens and 9,295 output tokens.

## Concrete cause

`winnerMultipassVisibleResponse` calls `rejectWinnerReviewCaseAliases` recursively
on the whole provider response. The preserved response echoes the requested
structured-output schema under `text.format.schema`. Valid JSON Schema keywords
there include `additionalProperties`, `minLength`, `maxLength`, and `maxItems`.
The recursive lowercase-key check rejects those keywords before validating the
actual review observation. This is a client validation defect, not a demonstrated
candidate failure, provider timeout, or token-limit exhaustion.

The exact-version readback of `transport.response` was SHA-256 authenticated:
`796e0fadce73da4674a93f0d74185168bcdc9703c37bcfe371919517aa9d2c3a`.
Private diagnostic files and the bounded Go reader are retained at
`/home/by/urnetwork/temp/qg-terminal-diagnostic-20261108.pelp0O8I`.
The reader performs no model calls and grants no qualification authority.

## Required correction and next gate

Separate the provider envelope/schema metadata from the strict review-output
contract. Add deterministic regression controls for echoed schema keywords and
adjacent alias attacks. Preserve original response bytes, digest binding, model,
usage, terminal-status, privacy and structured-observation checks. Do not simply
disable alias validation on review decisions or strip original response bytes.

After offline qualification and immutable release preparation, obtain explicit
permission before another provider qualification attempt. Formal candidate
review has not started. Rewards and source promotion remain held pending review
and final user approval, including the unaccepted-calibration caveat.

## Correction checkpoint

Main source fix: `be0fbadbb`. Frozen reviewer branch
`competition-review-schema-echo-20261108`: `94c5bfb14`, based on the previous
`7a7b2f8dd` reviewer source, not moving main. The installed reviewer is unchanged.

The schema-echo regression fails against the original validator and passes after
the correction. Composed JSON and SSE capture/replay tests retain exact original
bytes and refuse a second draw. Alias attacks outside `text.format.schema` remain
rejected. Adjacent privacy and composed provider tests pass.

Focused race validation: 9 top-level tests passed, zero failures/skips, 2.962s.
Raw log: `/home/by/urnetwork/temp/schema-echo-validation-20261108.P7Jbq0lJ/focused-race.jsonl`.
SHA-256: `d7dde67ba4337e9ad157698fe7338cb372ea38fa8979874ee58d3113cede205b`.
This is offline source validation, not genuine-model qualification or winner
approval. Broader frozen-source qualification remains required.

The first broader run on `94c5bfb14` completed in 186.624s with 141 top-level
passes and one failure: `TestWinnerMultipassCompiledQualificationUsesOnlyTopLevelTestEvents`.
The new regression tests used nested `t.Run` cases, which the immutable native
release census prohibits. All cases are now retained in flat loops, committed
on main as `520441e41` and frozen on reviewer branch
`competition-review-schema-echo-flat-20261108` at
`4ac7ea6f3b008238a466d41948ead57cc4531861`.

The old race run was deliberately stopped after this deterministic failure;
its partial log is not qualification evidence. Both broader suites are rerunning
on the flattened source. The inactive `94c5bfb14` staged release/image is preserved
but superseded and must not be activated. The installed reviewer remains unchanged.
