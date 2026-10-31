# Production intake launch receipt

Production submission intake opened at **2026-10-29 13:06:42 UTC**, later than
the October 28 deadline. This is the owner-approved intake-first replacement
season, not a claim of on-time launch or completed scoring qualification.

| Field | Observed value |
| --- | --- |
| API | `https://api.bringyour.com` |
| Competition | `sim-latency-season-1` |
| Season | `sim-latency-production-20261028-r2` |
| Epoch | 1, production (`staging: false`) |
| Round | `01a1855d-49bd-0250-90db-d0c0885f0e46` |
| Opens | `2026-10-29T13:06:42.413416Z` |
| Closes | `2026-11-05T13:06:42.413416Z` |
| Decision contract | `provisional_reward_v2` |
| Runtime capability | `competition.reward.v2.runtime-blob.v1` |
| Intake status | `intake_ready: true` |
| Grading status | `grading_ready: false` |
| Publication status | `publication_held: true` |

## Evidence and containment

- Source bootstrap and eleven-artifact publication completed against config
  commit `c3be1e1c91b61bfc3ee0e7b364488a49831e26d5`.
- Native preparation completed successfully, including the exact-target R1
  readiness evaluation, authenticated evidence and API/worker acknowledgments.
  The supervisor then recorded `source_active` and `round_created` normally.
- The expired, empty v2 staging epoch 2 was the remaining production-opening
  guard. Its round `01a162a6-8b83-3f5c-edd0-d50210ba92dc` closed on October 24,
  had zero submissions and no baseline, and was sealed through the existing
  `PostgresStore.SealAndElect` path at 13:06 UTC. Its genuine decision is
  `no_winner` / `no_submissions`, with shadow settlement. No historical source,
  deadline, policy or evaluation was rewritten.
- Both latest production holds were independently read from PostgreSQL after
  creation: `evaluation` and `reward_delivery`, active revision 1, bound to
  owner evidence `2a5e8fcf301b12cd6f9983e8ffb753804d31a9e863097c7368a2f85d236ef3ad`.
- The existing Apex production token passed
  `POST /competition/v2/score/preflight` with HTTP 200 and
  `reward_eligible: true`. This was screening only: no fee was charged and no
  job was enqueued. It is not an actual paid submission/score proof.
- Public `/competition/info` returned HTTP 200, `active_round.status: open`
  and `intake_ready: true` after persistent-service handover.
- `urnetwork-competition-production-r2.service` is installed, enabled and
  running on sille. Its checked-in unit accompanies this receipt. Management
  is bounded to CPUs `20,22`, 8 GiB and no swap; evaluator resource boundaries
  remain independent and unchanged. Reboot still requires genuine current-boot
  qualification; enabling a service does not waive that gate.

## Unchanged evaluation and remaining gates

The frozen ten-repository evaluation checkpoint, Go 1.27.3, image
`sha256:98c9061c4a43a25e7b581a5193ca6e7f48e49e7b7fc03b7c1b6190a029befee2`,
R9, 26.9% takeover margin, significance checks, three-hour cap, six weekly
epochs, $20 fee and unbounded FIFO remain unchanged. Main API/control programs
remain independently maintainable on main.

The actual deferred calibration and genuine Qwen/Gemma provider qualification
are **not complete**. Reviewer unit controls passed 239 tests three times,
but those results do not establish real model behavior. Keep evaluation and
reward/publication holds active until the remaining gates have real evidence
and explicit recovery is recorded through the native revision-fenced flow.

Do not report the competition as fully grading-ready, issue a winner, or release
rewards on the strength of this intake receipt alone.
