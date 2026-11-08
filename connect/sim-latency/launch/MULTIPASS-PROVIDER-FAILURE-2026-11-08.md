# Qwen/Gemma qualification: provider privacy refusal

The controlled qualification run stopped at 2026-11-08 09:04:58 UTC. It did not
qualify the reviewers and did not review or reject the elected submission.

- Unit: `urnetwork-review-qg-controls-recovery-f866-20261108-v1.service`.
- Owner: `2ae4f15c5c7c6b6a2b4a5aa204f79413cbc8cecb751353a100a471c7b1b5f30e`.
- First generation: operation 183, `qwen-scan-0001`, started 09:01:43 UTC.
- Retained failure: `provider returned hidden reasoning despite disabled inclusion; not retained`.
- Protected group-report SHA-256:
  `287e225943bf273a7f1849292f6d955907793962f75349dc18b1b94654172be4`.
- Exact report version: `dc627385-46b6-4c8b-8eba-23ac87b6413e`.
- Report retention: through 2028-01-01; native outcome reports no failed originals.

The client requested `include_reasoning: false`. Its privacy guard rejects
reasoning event/item types as well as nonempty reasoning-content fields. The
failure message alone therefore does not establish whether the provider returned
actual reasoning text or an empty reasoning-shaped item. Rejected transport was
not retained; do not invent its contents or retrieve hidden reasoning.

## Next diagnostic boundary

Ask Snow operations to inspect the existing request around 09:01:43–09:04:58 UTC
from sille. Request only response/event type names, terminal status, model
identity, and whether a reasoning item was empty or nonempty—not reasoning text.
Determine how the running Responses frontend handles `include_reasoning: false`.
Do not send another inference draw as an observational probe.

If the provider must change, retain its deployment receipt and seek authorization
for a fresh prospectively declared qualification. If the client mistakes empty
metadata for private content, add deterministic regression tests and independently
qualify the correction before a new release. Neither path permits relabeling this
failed attempt as passed or silently retrying its first draw.

Winner approval, rewards, and source promotion remain held.
