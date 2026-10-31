# Snow Muse structured-output incident — 2026-10-27

The V7 qualification remains failed closed. A specific upstream enforcement
defect exists in the configured vLLM release; it is not grounds to repair the
answer, retry V7, drop Muse, weaken the schema, or claim qualification.
No provider call, SSH session, remote deployment or client change was made in
this investigation.

## Evidence and confidence

V7's benign case retained a 2,297-byte Muse judgment missing its final outer
`}`. The member failed strict decoding and the aggregate stayed pending.
Correlation ID: `resp_a88eecad0d2a8cf3`; retained transport SHA256:
`90ba0daf0da5b1c9669b58ae7d674eab29ed275e162f8dc1ea29aafab437e2b2`.
Request SHA256:
`870632dd984e628ce55634e5b64e31d5a96b5a86c17474034df5cbf4d8512eb7`.
The actual raw HTTP/SSE response was **not retained**: only its digest and
extracted judgment were journaled. Exact final-token/stop-cause reconstruction
therefore still requires existing Snow-side evidence. Report SHA256 remains
`19a916dee78e8e2e39b92354a8141ec001e486ae100f140c81fcf1467f9c69bb`.

Local authoritative configuration is xops commit
`9bd20fbc60bb60f84b063aec969530675cd4b8d5`:

- `main/ansible/host_files/snow/vllm/vars.yml:15-17` pins vLLM 0.30.0 image
  `sha256:8a69ffad015f138d7170c4ddc429e230a3bc1c1719f67e14324749df200a4b90`.
- The same file at lines 240-250 selects `muse_glimmer` reasoning/tool parsers,
  four-token n-gram speculation, and the xhigh reasoning mapping. The model
  checkpoint remains `e83ead547c81973aaa09c0072c0dbc916e0d8190`.
- nginx forwards to llama-swap 257, which proxies to the per-model vLLM
  process. These templates contain no custom Responses-schema translation.
- `tasks/vllm-model-gates.yml:99-118` tests an unconstrained Responses “pong”
  request and accepts either completed or incomplete status. It does **not**
  establish strict JSON-schema enforcement.

These are configuration facts, not a readback of Snow's running image,
installed parser files, effective flags or dependency versions.

## Source-level cause

The v0.30.0 tag resolves to commit
`ced6857afa0ea7b2e3f0846a62e1394e90f15607`. Its Responses adapter correctly
maps `text.format.type=json_schema` and `schema` into structured-output
parameters. The broker's request shape is correct; adding another JSON wrapper
is not the fix. [Responses mapping, lines 351-371](https://github.com/vllm-project/vllm/blob/ced6857afa0ea7b2e3f0846a62e1394e90f15607/vllm/entrypoints/openai/responses/protocol.py#L351-L371).

The Muse parser reports reasoning-end only for a tool/ATEM handoff, not the
ordinary `to=user` answer. The grammar manager reuses that predicate to decide
when constraints start, while constraints during reasoning default to false.
Consequently an ordinary tool-free Muse answer can remain unconstrained even
with a requested schema. This explains an enforcement gap consistent with V7;
it does not independently prove which missing byte the running server emitted.
[Muse predicate, lines 130-148](https://github.com/vllm-project/vllm/blob/ced6857afa0ea7b2e3f0846a62e1394e90f15607/vllm/reasoning/muse_glimmer_reasoning_parser.py#L130-L148),
[grammar boundary, lines 200-265](https://github.com/vllm-project/vllm/blob/ced6857afa0ea7b2e3f0846a62e1394e90f15607/vllm/v1/structured_output/__init__.py#L200-L265),
[default configuration, lines 18-39](https://github.com/vllm-project/vllm/blob/ced6857afa0ea7b2e3f0846a62e1394e90f15607/vllm/config/structured_outputs.py#L18-L39).

The exact six-criterion schema must still be compiled against the installed
backend. `prefixItems` and `items:false` are not among this release's explicit
XGrammar unsupported-keyword checks; that is not proof of full enforcement.
[Backend validation, lines 227-359](https://github.com/vllm-project/vllm/blob/ced6857afa0ea7b2e3f0846a62e1394e90f15607/vllm/v1/structured_output/backend_xgrammar.py#L227-L359).

## Minimal safe mitigation for Snow operations

1. **Read back before changing anything.** Preserve existing logs for the V7
   correlation ID; record the actual image ID/digest, vLLM version, effective
   Muse launch flags, parser/manager file hashes, XGrammar/Guidance versions
   and generation/stop configuration. Use the established verified host-access
   procedure; do not disable SSH host-key verification or expose credentials.
   Relevant existing logs are `journalctl -u llama-swap` and the retained Muse
   engine logs. Do not submit a new judgment to diagnose this one.

2. **Prefer an immutable, reviewed backend correction with the existing model
   and xhigh profile.** Separate grammar activation at the user-answer boundary
   from tool-parser stream ownership. The smaller upstream proposal is
   [PR 53404](https://github.com/vllm-project/vllm/pull/53404), inspected at
   `3d7c5688cee92cb4f2511f463da3493fc5b1b4fa`: it adds a distinct grammar-start
   predicate without changing tool handoff. Its older manager call sites do
   not match v0.30.0's consolidated boundary routine, so do **not** apply it
   blindly. Review every prompt, speculative-token and committed-token
   constraint-start path in this pinned release. [PR 52390](https://github.com/vllm-project/vllm/pull/52390),
   inspected at `e494e371500330c3875448105ed37be3c1b111e2`, instead introduces
   a dedicated Muse composite parser and broader routing changes. Both were
   open/unmerged when checked; neither is an already qualified deployment.
   Pin the reviewed patch, dependencies, resulting image digest and model
   snapshots; keep the old digest/configuration as rollback artifacts.

3. **Require no-inference regression gates before deployment.** With stub
   tokenizers and a recording/mock grammar, prove that self-channel tokens
   remain unconstrained, the completed user-channel header activates the
   grammar before the first answer token, and tool handoff remains unchanged.
   Cover headers split across tokens, four-token speculative windows, prompt
   initialization and committed-token advancement. Compile the exact broker
   schema on CPU; reject malformed JSON, missing/duplicate criteria, extra
   findings and forbidden values. Exercise both JSON and SSE Responses
   translation, including a final-brace boundary and malformed terminal text.
   These gates need no model weights, GPU inference or provider request.
   They have not been executed as part of this document.

4. **Deploy only after operator review; qualify only under a new precommit.**
   Retain V7 and every prior outcome unchanged. A later explicitly authorized
   strict structured-output provider gate must require completed/pinned
   responses and native schema validation, not a “pong” smoke test. Only then
   run a newly reviewed genuine qualification with all three selected members;
   no malformed-answer repair or automatic retry. If deployment or gates fail,
   restore the prior immutable image/configuration, retain the failure, and
   keep automated review unresolved—the old release is not thereby qualified.

Do **not** flip `enable_in_reasoning=true` as a quick fix: it applies the JSON
grammar from token zero and can suppress the selected reasoning behavior.
Removing the reasoning parser or forcing a nonreasoning template is likewise a
different launch variant requiring explicit profile approval, channel/framing
tests and fresh qualification. The current xops effort prefix maps `none` to
`low`; sending `reasoning.effort=none` is not an established off switch.

## Operations resolution — 2026-10-28

The running Snow backend was read back before changes: the pinned image and all
three affected parser/manager preimages matched the source above. Installed
versions were vLLM 0.30.0, XGrammar 0.2.7, llguidance 1.7.6, Transformers 5.17.0,
and Torch 2.13.0+cu130. The engine configuration used backend `auto` and
`enable_in_reasoning=False`.

A reviewed derivative separates grammar activation from tool handoff and keeps
the earliest completed user header across speculative windows. Review found an
additional non-monotone window case; its failing controls were retained and the
final correction preserves the previous algorithm for parsers without an
explicit separate grammar hook. Existing Muse tool/extraction methods, model
selection, templates, launch arguments and four-token speculation are unchanged.

The actual built image passed 21 CPU boundary/protocol controls and 22 grammar
controls with Snow's real tokenizer and both unchanged native broker schema
exports. Deployment-template checks passed 24 tests. The original backend's
causal failure and initial candidate's two speculative failures are retained.
The schema exports use exact function-body AST checks with a projected policy
type; they are not proof of the missing historical V7 request body.

Deployment completed at **2026-10-28 16:40:22 UTC**. Muse runs immutable image
`sha256:c097ef57f805db9eb39b1cbb4d5d479c6dfbe2e796ea41bacef09e9a37a98663`.
Live health returned HTTP 200, installed hashes matched the reviewed image, and
all three selected models remained advertised. Qwen/Gemma configuration hashes
were unchanged. Two operator-runner failures (router readiness and an unbound
loopback ingress check) were retained; the final attempt passed. The image and
configuration backup for rollback remain on Snow.

The sanitized receipt is on **sille**:

`/home/by/urnetwork/temp/snow-muse-structured-output-20261028T164022Z/message.md`

Its sibling `receipt.json`, `SHA256SUMS`, regression logs, reviewed source bundle
and `rollback.md` contain the exact identities, versions, flags and recovery
instructions. Source/build/tests are committed in xops fix `4ed245e`, synchronized
main `7b5ec9d`.

No inference requests, malformed-answer repair, schema relaxation or V7 rerun
were performed by operations. **V7 remains failed closed and its exact truncation
cause remains unknown.** A fresh, prospectively recorded three-model
qualification is the next acceptance step; deployment and CPU controls do not
confer competition approval.
