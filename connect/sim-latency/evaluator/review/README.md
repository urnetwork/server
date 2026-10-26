# Isolated elected-winner review broker

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
client needs provider HTTPS/DNS egress; deployment must verify that no alternative
tools, host mounts or credentials are reachable. The wrapper is for the qualified
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

As of implementation, no live provider qualification or key provisioning is
implied. The broker must not be enabled solely because its Go tests pass.
