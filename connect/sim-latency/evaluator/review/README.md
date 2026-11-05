# Isolated elected-winner review broker

The October28 owner-approved prospective policy is `elected_winner_snow_union_v2`:
exactly Qwen/xhigh and Gemma/high, with all12 attributed findings and both-member
approval. Historical v1 Qwen/Gemma/Muse records retain their original18 findings;
failed V7/V8 attempts remain failed. Missing/error results hold; fully validated rejection is
retained without dropping later model issues. This is not a relabel of the
historical Mini/Sol procedures below. Use the new [Snow Dockerfile](Dockerfile.snow)
and a fresh qualification for the exact two-member executable and settings.
The new wrapper branch mounts only a public pinned root0444 settings leaf RO
and one mapped-uid0700 journal leaf RW, never a key or main/vault/config parent.
It fences only the fixed Snow LAN IPv4/TCP8000, not external HTTPS. Its real
entrypoint is `/review-broker review-broker`, with exactly three inspected
settings/hash/state-directory arguments. Whole-attempt/member O_EXCL journals
prevent rerolls; cancellation before provider input consumes no claim. These
new bytes are unqualified until independent unit and genuine broker proof pass.
The prospective wrapper reads `/etc/urnetwork/competition-review-v2.json` and
`/var/lib/urnetwork/competition-review/snow-provider-v2.json`; existing v1
installation files and claim journals must not be overwritten or reset.

## Traffic-preservation addendum

`traffic-preservation/1` makes the existing prohibition on deliberate workload
suppression explicit for epoch 1 and future reviews. It is not a new numerical
gate, a zero-failure requirement, a new model-policy ID, or a seventh public
criterion. Each model must return `workload_preservation` with the exact
version, `pass`, `fail` or `uncertain`, and a factual explanation. Approval
requires `pass`. A failed check must be supported by a failed existing
`measurement_integrity`, `workload_generalization` or `source_safety` finding
and an applicable existing reason. Missing/malformed checks fail closed; an
uncertain check cannot approve. Every Snow member must supply its own check.

The shared instructions require tracing changed deadlines, admission, refusal,
retry and body completion, including suppression of downstream discovery.
Authentic telemetry and passing G1/G2 do not establish honesty. Conversely,
lower success or work is only an investigation signal: ordinary variation,
incidental regression, genuine optimization, caller cancellation, security
refusal and legitimate overload recovery are not automatically misconduct.

The private packet carries the activated image/source identity, frozen
evaluation policy, compiled simulator/scorer hashes when available, and the
trusted-instrument overlay proof when one exists. The selected product parent
inside that proof must not replace its instrumented server child. The manager
reads the fixed instrument-file census from that exact commit even when the
candidate touches none of those files. Future coverage-helper absence is
explicitly authenticated at historical bases. Whole files preserve local
helpers, but this is not a complete transitive dependency graph; unavailable
external helpers require uncertainty/additional authenticated context, not
invented semantics. Source/image/proof mismatches hold review.

The fixed addendum text and SHA-256, instrument bytes/hashes, exact score and
patch enter the readable model input and non-clock cache identity. Both Snow
and historical single-provider paths validate their original input. Old cached
judgments remain retained; missing or different context holds for operator
intervention instead of silently reusing an old approval or rerolling a model.
No existing review or frozen round policy is rewritten by this implementation.
Touched-file and instrument context have separate bounds, and the overall
4 MiB request bound still refuses oversize input rather than truncating it.

The new source is not operational qualification. Before activation:

1. Independently run the deterministic `TestWinnerWorkloadReview*`, existing
   winner-review suites, and `TestCompetitionReviewInstrument*` tests, including
   the private packet's database producer regression on the designated test
   host. These tests make no live model calls.
2. Precommit the exact wrapper/binary/image/settings/addendum and input hashes.
   Extend the qualified one-shot plan with all cases in
   [`testdata/workload-review-qualification-v1.json`](../../testdata/workload-review-qualification-v1.json),
   alongside existing benign, fabrication, injection and containment controls.
   Use authenticated instrument context for the intended epoch. Keep expected
   verdict labels outside provider input; the mock-test context is not genuine
   qualification evidence.
3. Run the actual pinned Qwen/Gemma union, retaining each original subcheck,
   six findings, input/output hashes, provider receipts, usage and containment
   evidence. Include deliberate generic large-transfer refusal, incomplete
   parent-body suppression, an honest optimization with the same suspicious
   metrics, legitimate cancellation, and missing recovery-context uncertainty.
   Never resample a failed or uncertain completed case until it agrees.
4. Authorize deployment of the exact qualified control programs separately.
   Do not change epoch 1's scorer, numeric gates, accepted results or source pins.
   A future instrument change needs its own approved image and fresh baseline.

The historical qualification receipts and draft plans below do not cover this
new mandatory context/output contract. Passing mocked schema/union tests proves
contract enforcement, not real-model detection or freedom from false positives.

## Historical key-backed policies

This is the real tool-free Responses API client for `gpt-5.4-mini` at `xhigh`
reasoning, not a template-approval emulator. Its strict judgment passes through
the manager's authenticated six-criterion validation before any main API write.
The model chooses findings, never identity fields or publication credentials.
Patch bytes and JSON scores are made readable in the input, with digests checked
before a provider request. Stream/event sizes and request lifetime are bounded.
Provider identity, token usage, and concise findings are retained privately;
hidden chain-of-thought is neither requested nor used as an audit artifact.

The new season freezes `elected_winner_gpt54mini_xhigh_v1`; `xhigh` is the
highest supported effort. Historical `elected_winner_sol61max_v1` records retain
their original Sol/max provenance and cannot be relabeled as mini reviews.
Independent prescribed tests still use Sol 6.1 low; the shared stream framing
does not select or migrate their model.

The Mini policy requests the documented immutable provider snapshot
`gpt-5.4-mini-2026-03-17`, not a floating API alias. Private evidence separately
records `policy_model`, `requested_model` and the actual returned `model`.
Returned identity must match the requested snapshot exactly; another revision
is not silently accepted. Legacy policy requests retain their original identity.

An HTTP refusal is not a review. The broker exits nonzero and emits a distinct
private `review_provider_refusal` envelope with bounded status/code/type,
request identity and capture digest; complete JSON bodies are credential-redacted.
Truncated/malformed bodies retain a digest only. No provider refusal triggers
an automatic retry, test execution, approval or disqualification. In particular,
model metadata access HTTP 200 does not prove funded inference: the October 26
actual Mini request returned HTTP 429 `credit_balance_exhausted` and remains
unqualified until credits and genuine broker tests pass.

The client follows the official [model contract](https://developers.openai.com/api/docs/models/gpt-5.4-mini)
and [structured output contract](https://developers.openai.com/api/docs/guides/structured-outputs).
It has no model tools, shell, subprocess invocation, API redirect, configurable
provider endpoint, or ambient proxy. All candidate text is untrusted input.

## Deployment boundary

Build the current trusted manager with `CGO_ENABLED=0` and the locked dependency
graph. Make a fresh private build directory containing only that `sim-latency`
binary, the reviewed Dockerfile, and a public `ca-certificates.crt`. Build the
scratch image locally; record its immutable image ID and executable digest.
Do not use the repository root as the Docker build context.

Provision a **dedicated** model API key at
`vault/main/competition-review-openai.key` (mode 0600). Do not repurpose a Codex
login credential or the main reviewer API token. Install its private runtime copy
as `/var/lib/urnetwork/competition-review/model.key` (mode 0400), readable by the
Docker-remapped container UID 65532; determine the host UID from the actual
daemon mapping. Keep its root-owned parent mode 0700. Only this exact key leaf is
mounted, never a vault/config parent, main source tree, SSH agent, or Docker socket.

Install `run-broker.sh` root-owned/executable and configure the root-owned,
non-group/world-writable `/etc/urnetwork/competition-review.json`:

```json
{
  "image_id": "sha256:REPLACE_WITH_ACTUAL_IMAGE_ID",
  "model_key_file": "/var/lib/urnetwork/competition-review/model.key"
}
```

The wrapper runs its immutable scratch image non-root/read-only, without
capabilities, with one CPU on the management cores and 2 GiB memory. The fixed
client uses provider HTTPS through a host-resolved, single-provider-IP fence;
external DNS in the container is disabled. Deployment must verify that no alternative
tools, host mounts or credentials are reachable. An IP fence is not independently
a TLS-hostname-aware proxy. The wrapper is for the qualified
host's core mapping, not a portable assumption about another evaluator host.
Pin the installed wrapper digest as the supervisor's `--review-sha256`; pin and
retain the image/key deployment record separately. No key appears in command
arguments or the model prompt. The scoped main reviewer token remains only in
the parent manager.

## Qualification before enablement

Mock tests are not genuine model or containment qualification. Before enabling
the production review policy, prove the actual installed wrapper and provider
can review benign, fabricated-score, benchmark-special-case, prompt-injection,
and insufficient-context examples. Check all six findings, exact source/score
identity, retained private provenance, model/effort pins and API idempotency.
Verify malformed/completed uncertainty cannot be resampled into approval, and
that unavailable model access pauses progression without approving/rejecting a
candidate. Verify memory/CPU bounds, no main source/config/vault/socket mounts,
and cleanup on cancellation. Retain those receipts with the launch evidence.

Follow the [provider qualification procedure](../../launch/REVIEW-PROVIDER-QUALIFICATION.md)
for the fixed cases, exact snapshot, observed container/cgroup/firewall receipts,
and bounded cancellation checks. Its draft tools and this uninstalled wrapper
are preparation only until their independent execution and live proof pass.

As of implementation, no live provider qualification or key provisioning is
implied. The broker must not be enabled solely because its Go tests pass.
