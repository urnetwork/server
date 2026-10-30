# Owner-approved first-committed-seed calibration amendment

The owner explicitly answered **“Approve first committed seed only”** to:

> Both calibration seeds are now committed, but neither has been measured.
> Two seeds will finish after the fixed launch time. Do you approve evaluating
> only the first committed seed, requiring correct reference ordering,
> preserving the second unstarted, and keeping R9, all gates, significance,
> margin and deadlines unchanged? This is a prospective reduction in confidence,
> not selection based on results.

No independent B measurement had started when approval arrived. The two-seed
native preparation and verification had completed, but the executor had **not**
been launched. This amendment therefore chooses the first fixed position before
observing any B result. It does not choose the better-looking seed, discard a
failed measured run, authorize a replacement, or permit new seed generation.

The preceding [two-seed declaration](HELD-OUT-TWO-B-DECLARATION-2026-10-27.json)
and [published commitment plan](HELD-OUT-TWO-B-PLAN-2026-10-27.json) stay immutable:

- Declaration SHA: `69202c5938c226faac9eeb6f90037869e1a391587e010d2a9787101efdc60c44`.
- Public plan SHA: `b3150c5098ea9d4cb155be76c5a423967ecca479813ed75061db5b5bf50bcc43`;
  published commit `40a27c5fb5ba4f368f187b5cd6894ae9e6670d23`.
- Original private plan SHA: `0b8c65e3186d631782c6247d18e2369b9994851effced026037ed032401c6abb`.

Only position **1**, round `74ea5b76-6308-48f5-bd55-8f852a17d215`, may be
evaluated under the amended protocol. Preserve its existing seed commitment,
provider data, source lock, policies and reference ordering. Its exact jobs are:

| Existing role order | Existing job ID |
| --- | --- |
| better | `d11e4ae4-c3f8-4687-b481-3b02bc336130` |
| worse | `23fc7aeb-20ee-40d6-9ad4-2ef495671dda` |
| no-op | `8422293a-07f4-43c7-99a2-4bcee8ebd283` |

Position 2, round `9c8bc5b9-d5d5-44fd-98df-7ca7f449efc5`, remains committed
but **unstarted**. Preserve its original private/public records; do not execute,
replace or regenerate it as part of this launch calibration.

The amended acceptance requires correct ordering of the unchanged three
references on this **one** selected seed, with every measured outcome retained.
Keep R9 controls and R9 candidates, all six gates, the same significance test,
the frozen 26.9% practical margin, original per-job 10,800-second bound, accounting,
artifact integrity, and cleanup. The only permitted later request bindings are
the original protocol's already-declared shared-baseline fields; there is no
new freedom to alter requests, patches, workload or selection after outcomes.

This retains less independent-seed evidence than the previous two-seed plan.
The reduced confidence is accepted explicitly to meet the fixed public opening;
it must not be reported as two measured seeds or the original larger screen.
The accepted three-A reduction and corrected C2 bridge remain separately
preserved. The bridge is operational compatibility evidence, not equivalence.

Before measurement, qualify the named amended operator, authenticate the
unchanged original two-seed plans, freeze the exact first-position derivation,
and publish its complete selected commitment plan. The old two-seed executor
must not be launched by mistake. Preserve all original authority and failures.
No fresh entropy draw or automatic retry is authorized.

The conditional production-policy preimage must also be updated from a
two-seed requirement to this exact first-committed-seed requirement before
activation; do not relabel or edit sealed `f6967287…` evidence. This amendment
does not grant host readiness, model qualification, source activation, API/worker
acknowledgments or public-admission permission. Those gates remain mandatory.
